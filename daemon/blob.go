package daemon

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"blob/cid"
	"blob/identity"
	"blob/oci"
	"blob/storage"
)

// source adapts a storage backend to an oci.Source, translating each OCI digest
// to the store's identity (multihash) key. It is the daemon's own small source
// side (main's OCI import/export keeps an equivalent adapter for its own
// use); the two are the same shape — the identity bridge is a re-encode, so
// both reach the same store key for the same content.
type source struct {
	backend storage.StorageBackend
}

func (s source) Has(d oci.Digest) (bool, error) {
	id, err := identity.FromOCI(d)
	if err != nil {
		return false, nil
	}
	return s.backend.Has(id)
}

func (s source) Get(d oci.Digest) (io.ReadCloser, int64, error) {
	id, err := identity.FromOCI(d)
	if err != nil {
		return nil, 0, err
	}
	// The store backend's Get now also returns an mtime; this reader only
	// needs the size and content, so we drop it (a 0 is treated as "unknown"
	// by callers, which is what an oci.Source consumer would expect anyway).
	r, size, _, err := s.backend.Get(id)
	return r, size, err
}

// digestFromCID returns the storage key for c, validating that the CID carries
// a codec the store holds (raw / json) and a hash identity understands (SHA-2
// 256 today).
func digestFromCID(c cid.Cid) (storage.Digest, error) {
	if !cid.Supported(cid.Codec(c)) {
		return nil, fmt.Errorf("daemon: unsupported codec %q: this store only holds raw and json", cid.CodecName(cid.Codec(c)))
	}
	if _, err := identity.DigestBytes(identity.Identity(c.Hash())); err != nil {
		return nil, err
	}
	return storage.Digest(c.Hash()), nil
}

// cidForDigest derives the CID of a *stored* blob from its identity by peeking
// the first byte (the content-based codec rule) and composing the CID. It
// reads only a single byte, so it is constant work regardless of blob size and
// never re-hashes — a name resolves to the exact CID that `add` printed.
func (s *Server) cidForDigest(d storage.Digest) (cid.Cid, error) {
	first, err := s.backend.Peek(d, 1)
	if err != nil {
		return cid.Cid{}, err
	}
	var firstByte byte
	if len(first) > 0 {
		firstByte = first[0]
	}
	return cid.Build(cid.CodecForFirstByte(firstByte), identity.Identity(d)), nil
}

// serveBlob streams a single blob to the response, identified by CID.
// The CID carries the codec (→ Content-Type) and the multihash (→ the
// digest, → Content-Digest) — both already known at request time, so no
// content is read for the headers. The body is streamed; the blob is
// never read into memory.
//
// The server's shared read-only backend is used here; it is safe to call Get
// on it from many goroutines concurrently (see Server).
func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request, ref string) {
	c, err := cid.Parse(ref)
	if err != nil {
		http.Error(w, "invalid CID: "+ref, http.StatusBadRequest)
		return
	}

	digest, err := digestFromCID(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	reader, size, mtime, err := s.backend.Get(digest)
	if err != nil {
		http.Error(w, "not found: "+ref, http.StatusNotFound)
		return
	}
	defer reader.Close()

	contentDigest, err := cid.ContentDigestHeader(c)
	if err != nil {
		http.Error(w, "invalid multihash", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Digest", contentDigest)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Type", cid.MediaType(c))
	// Last-Modified (RFC 7231) is the store's last-modified time for this
	// blob: the moment it was added to the store (not a client-supplied time,
	// and not the content's mtime). We omit it when the backend does not
	// track it (stored as 0), so a client that does a conditional GET
	// (If-Modified-Since) against a backend without this field still works.
	if mtime > 0 {
		w.Header().Set("Last-Modified", time.Unix(mtime, 0).UTC().Format(http.TimeFormat))
	}

	w.WriteHeader(http.StatusOK)

	_, _ = io.Copy(w, reader)
}

// serveIPFS is the IPFS-gateway compatibility surface: IPFS-style clients
// address content as /ipfs/<CID>, so this root lets them reach the store's
// /blob/<CID> surface without knowing this store's scheme. The CID (any
// multibase) is parsed first, and the store's normal resolution order is
// applied:
//
//   - held codec (raw / json): the CID's multihash is the store key directly,
//     so 301 to /blob/<CID> (rendered base32hex) — a direct address. 301
//     (Moved Permanently) rather than 302: a blob's CID is permanent by
//     construction (same bytes → same identity), so the redirect is safe to
//     cache, and it signals to the client "this is the canonical address,
//     use it going forward."
//   - foreign codec (e.g. dag-pb, the default `ipfs add` shape): the CID is
//     not a direct address, so it is tried as a name in the index ("ipfs get →
//     blob add" interop handle — index.Check explicitly allows this, and
//     `resolveDigest`/`resolveStoreKey` resolve it the same way). On a name
//     hit, 302 to /blob/<derived CID> (the store's CID for the content,
//     derived by the peek-first-byte rule, the same way /name/<name> does).
//     302 (Found) rather than 301: the name→CID mapping is mutable (last
//     add wins), so the redirect must not be cached. On a miss, 404.
//
// The redirect (not a direct serve) lets the client learn the canonical
// address and keeps the content served from exactly one place (/blob/<CID>)
// — same pattern as /name/<name>. The compatibility is bounded by shared
// content, not by address: an IPFS raw-leaf piece is content the store holds
// byte-for-byte, and a dag-pb root becomes addressable the moment a user
// records it under its CID as a name (which `add`/`oci import` allow via
// --name).
func (s *Server) serveIPFS(w http.ResponseWriter, r *http.Request, ref string) {
	c, err := cid.Parse(ref)
	if err != nil {
		http.Error(w, "invalid CID: "+ref, http.StatusBadRequest)
		return
	}
	if cid.Supported(cid.Codec(c)) {
		// Held codec: /blob/<CID> is a real address — apply the same gate
		// /blob/<CID> would (codec + identity-aware hash), then redirect to it
		// in the store's base (base32hex).
		if _, err := digestFromCID(c); err != nil {
			http.Error(w, "not held: "+cid.CodecName(cid.Codec(c)), http.StatusNotFound)
			return
		}
		cidStr, err := cid.Render(c)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// 301, not 302: a held codec's CID is the content's permanent identity
		// (same bytes → same address), so the redirect may be cached, and it
		// tells the client this is the canonical address to keep using.
		http.Redirect(w, r, "/blob/"+cidStr, http.StatusMovedPermanently)
		return
	}
	// Foreign codec: not a direct address. Try it as a name — the "ipfs get →
	// blob add" interop handle index.Check allows — and, if recorded, the store
	// derives its own CID for the content and redirects there, exactly as
	// /name/<name> does.
	s.serveName(w, r, ref)
}

// serveName resolves an index name to its blob, derives the blob's CID
// by peeking the first byte (the same content-based codec rule as
// `add`/`sum`), and issues a 302 redirect to /blob/<CID>. The redirect
// is the name→blob pointer (mirroring the dir backend's symlink for the
// same role) and leaves all streaming / header work to /blob/<CID>.
// 302 (not 301): the name→CID mapping is mutable (last add wins), so it
// must not be cached.
func (s *Server) serveName(w http.ResponseWriter, r *http.Request, name string) {
	digest, err := s.backend.Lookup(name)
	if err != nil {
		http.Error(w, "name not found: "+name, http.StatusNotFound)
		return
	}

	c, err := s.cidForDigest(digest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cidStr, err := cid.Render(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/blob/"+cidStr, http.StatusFound)
}
