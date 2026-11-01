package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"blob/cid"
	"blob/identity"
	"blob/oci"
	"blob/storage"

	godigest "github.com/opencontainers/go-digest"
)

// OCI distribution (/v2/) read endpoint on top of the daemon, so a layout
// already in the store can be pulled by stock OCI clients (podman, skopeo,
// oras, containerd, buildkit) without an `oci export` step.
//
// It mirrors the two HTTP gateway surfaces in the store's own language, so an
// OCI client and an HTTP client address the *same* content the same way:
//
//	HTTP gateway        blob://host/blob/<CID>        ->  http://host/blob/<CID>
//	OCI registry        docker://host/blob/<CID>      ->  http://host/v2/blob/<CID>/manifests/<ref>
//	                                    host/name/<name> ->  http://host/v2/name/<name>/manifests/<ref>
//
// `blob/<CID>` is the *primary* surface: `<CID>` is the outer (index-root) CID
// that `blob oci import` prints, and its multihash *is* the store key of the
// layout's index.json — so the OCI pull and the `blob://` gateway hit the very
// same index blob. `name/<name>` is the secondary surface: it resolves <name>
// through the name index (last-add-wins) and serves the same layout, just like
// /name/<name> 302-redirects to /blob/<CID>.
//
// Read-only by design: the backend is opened storage.ReadOnly by the caller,
// so the write paths do not exist by construction. The endpoints implemented
// are the distribution-spec read surface:
//
//	GET    /v2/                              the spec's ping
//	GET|HEAD /v2/<repo>/manifests/<ref>     the index (for `latest`) or a manifest
//	GET|HEAD /v2/<repo>/blobs/<digest>      any blob (config / layer / ...)
//
// `<ref>` is `latest` (the only tag; we have no name:tag split yet, so this is
// the default) or an OCI digest `sha256:<hex>` (→ the index, or a manifest).
// The inner blobs are always addressed by their `sha256:` digest — clients
// pull them that way, and we do not need a CID for anything but the outer index
// root. Push / tag-listing / auth are intentionally out of scope for v1.

// ociRepo bundles the per-request state for one OCI repository: the index key,
// its bytes, its digest, and the set of reachable digests. Building is the
// same handful of backend calls `oci ls` makes (one Get of the index, then a
// walk of its manifests). Two kinds of content produce a valid ociRepo today:
//
//   - a stored OCI index (dir/tar layout import) — the classic shape; and
//   - a stored layer of a wrapped bare file (`oci import` or `blob add` of a
//     non-layout file). For the latter no OCI JSON exists in the store; the
//     canonical single-layer artifact (index bytes, manifest bytes, `{}` config
//     bytes, layer key) is re-derived from the stored content with
//     `oci.NewArtifact`, and the scaffold's small JSON blobs are served from
//     memory — never persisted into the store, mirroring `oci export`'s
//     "synthesized for the output only" contract. `ociArtifact` is nil for
//     the index case and non-nil for the layer case; `indexBytes` holds the
//     canonical bytes (which the scaffold case serves from memory).
type ociRepo struct {
	repo        string
	backend     storage.StorageBackend
	indexKey    storage.Digest
	indexBytes  []byte
	indexDigest oci.Digest
	reachable   map[oci.Digest]bool
	// ociArtifact is non-nil for the layer case (bare file wrapped as the
	// canonical single-layer artifact); it carries the manifest and the
	// `{}`-config so they can be served from memory, since neither is in the
	// store. The layer itself is always served from the backend (it IS the
	// stored blob).
	ociArtifact *oci.Artifact
}

// errNameUnknown is the sentinel a ref failed to resolve to a stored object;
// the handler maps it to the distribution NAME_UNKNOWN (404) error.
var errNameUnknown = errors.New("daemon: oci name unknown")

// resolveIndexKey maps an OCI repository name to the store key (multihash) of
// its layout index:
//
//   - "blob/<CID>"  (primary): <CID> is the outer (index-root) CID from
//     `blob oci import`; its multihash is exactly the index.json key, so this
//     needs no name lookup — cid.Parse + digestFromCID.
//   - "name/<name>" (secondary): <name> is resolved through the store's name
//     index (last-add-wins), mirroring the HTTP gateway's /name/<name>.
//
// Any other repository name is rejected with an error, which the handler
// reports as NAME_UNKNOWN (404).
func resolveIndexKey(backend storage.StorageBackend, repo string) (storage.Digest, error) {
	if key, ok := strings.CutPrefix(repo, "blob/"); ok {
		c, err := cid.Parse(key)
		if err != nil {
			return nil, errors.New("repository " + repo + ": not a valid CID")
		}
		return digestFromCID(c)
	}
	if name, ok := strings.CutPrefix(repo, "name/"); ok {
		return backend.Lookup(name)
	}
	return nil, errors.New("repository must be blob/<CID> or name/<name>")
}

// firstByteJSON peeks at most one byte of the blob named by key and reports
// whether it opens a JSON value under the store's content-based codec rule
// (so at most one byte is read). This is the only shape an OCI index.json can
// take, and it is the first discriminator between "stored index" (walk it)
// and "stored layer" (wrap it as the canonical single-layer artifact).
func firstByteJSON(backend storage.StorageBackend, key storage.Digest) (bool, error) {
	b, err := backend.Peek(key, 1)
	if err != nil {
		return false, err
	}
	return len(b) > 0 && cid.CodecForFirstByte(b[0]) == cid.CodeJson, nil
}

// indexRepo builds an ociRepo from a stored index.json blob: the index bytes,
// its digest, and the reachable set (the index, its manifests, each manifest's
// config and layers). This is the classic layout shape `oci import` persists
// for a dir- or tar-layout input.
func indexRepo(repo string, backend storage.StorageBackend, key storage.Digest, indexBytes []byte) (*ociRepo, error) {
	reachable, err := oci.Reachable(source{backend: backend}, indexBytes)
	if err != nil {
		return nil, err
	}
	set := make(map[oci.Digest]bool, len(reachable))
	for _, d := range reachable {
		set[d] = true
	}
	return &ociRepo{
		repo:        repo,
		backend:     backend,
		indexKey:    key,
		indexBytes:  indexBytes,
		indexDigest: oci.DigestOf(indexBytes),
		reachable:   set,
	}, nil
}

// artifactRepo wraps a stored blob (a bare file added with `oci import` or
// `blob add`, or even a JSON blob that is not a valid image index — any
// content, really) in its canonical single-layer OCI artifact and builds the
// per-repo state for it. The scaffold's small JSON blobs (index, manifest,
// `{}` config) are served from memory by the serving helpers — the CAS is
// never touched, matching `oci export`'s "synthesize for the output only"
// policy: `blob oci import file` and `blob add file` produce the same stored
// layer, and the /v2 surface serves the same synthesized artifact for both.
//
// The layer's OCI digest is the identity of the stored key re-spelled in
// go-digest form (identity.ToOCI is the reverse of identity.FromOCI); we do
// not re-read the layer bytes to produce it.
func artifactRepo(repo string, backend storage.StorageBackend, key storage.Digest) (*ociRepo, error) {
	st, err := backend.Stat(key)
	if err != nil {
		return nil, err
	}
	head, err := backend.Peek(key, oci.SifSniffLen)
	if err != nil {
		return nil, err
	}
	cfgMT, layerMT := oci.MediaTypesFor(head)
	layerDigest, err := identity.ToOCI(key)
	if err != nil {
		return nil, err
	}
	a, err := oci.NewArtifact(layerDigest, st.Size, layerMT, cfgMT)
	if err != nil {
		return nil, err
	}
	reachable := []oci.Digest{a.IndexDigest, a.ManifestDigest, a.ConfigDigest, a.LayerDigest}
	set := make(map[oci.Digest]bool, len(reachable))
	for _, d := range reachable {
		set[d] = true
	}
	return &ociRepo{
		repo:        repo,
		backend:     backend,
		indexKey:    key,
		indexBytes:  a.IndexBytes,
		indexDigest: a.IndexDigest,
		reachable:   set,
		ociArtifact: a,
	}, nil
}

// newOCIRepo builds the per-repo resolver: resolve the repository to a store
// key, then branch on the content.
//
// A resolved key that is a valid OCI image index (a JSON blob whose first byte
// is '{' AND whose shape passes `oci.ParseIndex`) is the classic "stored
// layout" case — indexRepo walks it.
//
// Any other key (a stored bare file, or a JSON blob that is not a valid
// image index — for example a config JSON) is the "stored layer" case —
// artifactRepo wraps it as the canonical single-layer artifact and serves
// the scaffold from memory. This is the "any blob in the store is retrievable
// over /v2" goal: `oci export` has the same policy (wrap the bare file in
// the canonical artifact, don't 404).
func newOCIRepo(backend storage.StorageBackend, repo string) (*ociRepo, error) {
	key, err := resolveIndexKey(backend, repo)
	if err != nil {
		return nil, err
	}

	jsonHead, err := firstByteJSON(backend, key)
	if err != nil {
		return nil, err
	}

	if jsonHead {
		r, _, err := backend.Get(key)
		if err != nil {
			return nil, err
		}
		indexBytes, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			return nil, err
		}
		if _, perr := oci.ParseIndex(bytes.NewReader(indexBytes)); perr == nil {
			return indexRepo(repo, backend, key, indexBytes)
		}
		// JSON blob that is not a valid image index: fall through and
		// wrap it as its canonical single-layer artifact. Same wrap-or-404
		// trade-off `oci export` makes for the same blob.
	}

	return artifactRepo(repo, backend, key)
}

// resolveRef maps a manifest ref to a stored digest: `latest`/empty → the
// layout index (the only tag, and the natural default entry); a `sha256:<hex>`
// token → that digest, if reachable; anything else → unknown.
func (o *ociRepo) resolveRef(ref string) (oci.Digest, error) {
	if ref == "" || ref == "latest" {
		return o.indexDigest, nil
	}
	d, err := godigest.Parse(ref)
	if err != nil {
		return "", errNameUnknown
	}
	if !o.reachable[d] {
		return "", errNameUnknown
	}
	return d, nil
}

// ociV2Handler returns an http.Handler for the /v2/ subtree, wired into the
// Server's mux as `mux.Handle("/v2/", s.ociV2Handler())`. It mirrors the
// flat-handler style of serveBlob/serveName (which close over the shared
// backend via the Server receiver).
func (s *Server) ociV2Handler() http.Handler {
	backend := s.backend
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveV2(w, r, backend)
	})
}

func serveV2(w http.ResponseWriter, r *http.Request, backend storage.StorageBackend) {
	// OPTIONS is the spec's capability probe (skopeo sends it to learn the API
	// version before continuing). It is answered for any /v2/... before the
	// repository is even looked up.
	if r.Method == http.MethodOptions {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Read endpoints only: anything that mutates (POST/PUT/DELETE) is refused —
	// the read-only backend has no write path, but this keeps the HTTP contract
	// honest rather than leaking an inner error.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		ociError(w, http.StatusMethodNotAllowed, "UNSUPPORTED_METHOD", r.Method+": read-only")
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	if rest == "" {
		ociPing(w)
		return
	}

	kind, repo, ref := splitOCIRef(rest)
	if kind == "" {
		ociError(w, http.StatusNotFound, "NAME_UNKNOWN", rest)
		return
	}

	obj, err := newOCIRepo(backend, repo)
	if err != nil {
		ociError(w, http.StatusNotFound, "NAME_UNKNOWN", repo)
		return
	}

	switch kind {
	case "manifests":
		serveV2Manifest(w, r, obj, ref)
	case "blobs":
		serveV2Blob(w, r, obj, ref)
	}
}

// splitOCIRef splits an OCI pull path — <repo>/manifests/<ref> or
// <repo>/blobs/<digest> — into (kind, repo, ref). <repo> may itself contain
// slashes (e.g. "blob/<cid>" or "name/ubuntu"), so it is everything before the
// "…/manifests/" or "…/blobs/" marker. kind is "" when neither marker is
// present (so the caller 404s a malformed path).
func splitOCIRef(rest string) (kind, repo, ref string) {
	for _, marker := range []string{"/manifests/", "/blobs/"} {
		if i := strings.Index(rest, marker); i > 0 {
			kind = marker[1 : len(marker)-1] // "manifests" or "blobs"
			repo = rest[:i]
			ref = rest[i+len(marker):]
			return
		}
	}
	return "", "", ""
}

// ociPing is the spec's `GET /v2/` response: a 200 with an empty error list and
// the pull capability advertised. The body is static; no store lookup.
func ociPing(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"errors":[],"capabilities":["pull"]}`))
}

// resolveBody returns the bytes for the digest d, drawing from the in-memory
// scaffold (if this repo is a wrapped bare-file layer) or from the backend
// (any other reachable blob). For a wrapped artifact, the manifest, index,
// and `{}`-config are served from memory (they are not in the store); the
// layer is always read from the store (it IS the stored blob). The
// media-type for the manifest (so `serveV2Manifest` can set Content-Type)
// is derived from the content — index bytes always carry the image-index
// mediaType, manifest bytes always carry the manifest mediaType — in the
// same way `oci export` writes them.
func (o *ociRepo) resolveBody(d oci.Digest) ([]byte, error) {
	if a := o.ociArtifact; a != nil {
		switch d {
		case a.IndexDigest:
			return a.IndexBytes, nil
		case a.ManifestDigest:
			return a.ManifestBytes, nil
		case a.ConfigDigest:
			return oci.EmptyConfig, nil
		}
	}
	id, err := identity.FromOCI(d)
	if err != nil {
		return nil, err
	}
	r, _, err := o.backend.Get(id)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// serveV2Manifest serves the index (for `latest`) or the named manifest by
// digest. Manifests are small (KB-scale), so the bytes are read whole to set
// Content-Length and the manifest's own mediaType; a HEAD request sets the same
// headers with no body.
func serveV2Manifest(w http.ResponseWriter, r *http.Request, obj *ociRepo, ref string) {
	d, err := obj.resolveRef(ref)
	if err != nil {
		ociError(w, http.StatusNotFound, "NAME_UNKNOWN", ref)
		return
	}

	body, err := obj.resolveBody(d)
	if err != nil {
		ociError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", d.String())
		return
	}

	// The Content-Type is the manifest's own mediaType when present (index vs.
	// manifest); we parse only the top-level field, so this works for both a
	// manifest and the index (whose parsed mediaType is the index).
	mediaType := oci.MediaTypeImageManifest
	if d == obj.indexDigest {
		mediaType = oci.MediaTypeImageIndex
	}
	var meta struct {
		MediaType string `json:"mediaType"`
	}
	if err := json.Unmarshal(body, &meta); err == nil && meta.MediaType != "" {
		mediaType = meta.MediaType
	}

	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Docker-Content-Digest", d.String())
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	// X-Blob-Cid: the store's CID for the *layout root* (the index, which is
	// what `blob oci import` printed), the same value the HTTP gateway's
	// /name/<name> 302 Location carries. The spec's Docker-Content-Digest is
	// only "sha256:<hex>" — it lacks the codec — so it does not pin the CID
	// (Json vs Raw is decided by first-byte, see the daemon package); the
	// X- prefix keeps it out of the distribution-spec namespace and OCI
	// clients ignore unknown response headers, so this can never break a
	// compliant pull. It is the discoverable "real path", the same way the dir
	// backend's symlink or sql's foreign key links a name to its blob.
	//
	// We only attach it when the object is the index (d == obj.indexDigest),
	// for both `blob/<CID>` and `name/<name>` repos. On `blob/<CID>` the CID is
	// already in the URL and the header is redundant (harmless — a consistent
	// "CID for the root" contract); on `name/<name>` it carries the derived
	// CID. On any sub-manifest it would mislead (the CID names the *whole
	// layout*), so we do not attach it there.
	if d == obj.indexDigest && len(body) > 0 {
		if id, err := identity.FromOCI(d); err == nil {
			c := cid.Build(cid.CodecForFirstByte(body[0]), id)
			if s, err := cid.Render(c); err == nil {
				w.Header().Set("X-Blob-Cid", s)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// serveV2Blob streams a reachable blob (config, layer, or any manifest/index).
// It is the pull's heavy lift: the body is streamed end-to-end (a layer may be
// GB/TB), never buffered. Content-Type is the generic octet-stream (the spec
// permits any media type on a blob; clients select a parser from the manifest's
// mediaType, which was already sent on the manifest fetch).
//
// For wrapped-bare-file repos the manifest and `{}`-config are served from
// memory — they are not in the store, and the "synthesize for the output
// only" policy means the CAS is not polluted by these small scaffolding blobs
// (mirroring `oci export`). The layer itself is always served from the store;
// *it is* the stored blob.
func serveV2Blob(w http.ResponseWriter, r *http.Request, obj *ociRepo, ref string) {
	d, err := godigest.Parse(ref)
	if err != nil {
		ociError(w, http.StatusBadRequest, "MANIFEST_INVALID", "invalid digest")
		return
	}
	if !obj.reachable[d] {
		ociError(w, http.StatusNotFound, "BLOB_UNKNOWN", d.String())
		return
	}

	// Synthesized-scaffold case: serve the manifest and the `{}`-config from
	// memory. They are small (KB-scale) and deterministic, so reading them
	// whole and sending them as-is is fine and cheaper than a backend round
	// trip. The heavy layer still streams from the store below.
	if obj.ociArtifact != nil {
		if d == obj.ociArtifact.ManifestDigest || d == obj.ociArtifact.ConfigDigest {
			body, err := obj.resolveBody(d)
			if err != nil {
				ociError(w, http.StatusNotFound, "BLOB_UNKNOWN", d.String())
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Docker-Content-Digest", d.String())
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodHead {
				return
			}
			_, _ = w.Write(body)
			return
		}
	}

	id, err := identity.FromOCI(d)
	if err != nil {
		ociError(w, http.StatusInternalServerError, "BLOB_UNKNOWN", d.String())
		return
	}

	reader, stat, err := obj.backend.Get(id)
	if err != nil {
		ociError(w, http.StatusNotFound, "BLOB_UNKNOWN", d.String())
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", d.String())
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size, 10))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, reader)
}

// ociError renders a distribution-spec error body:
//
//	{"errors":[{"code":"<code>","message":"<message>"}]}
//
// Clients (podman, skopeo, oras) surface the code + message, so the code should
// be one of the spec's (NAME_UNKNOWN, MANIFEST_UNKNOWN, BLOB_UNKNOWN, ...). The
// status code carries the class (404 for not-found).
type ociErrorEntry struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ociErrorBody struct {
	Errors []ociErrorEntry `json:"errors"`
}

func ociError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ociErrorBody{Errors: []ociErrorEntry{{Code: code, Message: message}}})
}
