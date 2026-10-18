package main

import (
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

// OCI distribution (`/v2/`) read endpoint on top of `blob serve`, so a layout
// already in the store can be pulled by stock OCI clients (podman, skopeo, oras,
// containerd, buildkit) without an `oci export` step.
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
// Read-only by design: the backend is opened storage.ReadOnly in runServe, so the
// write paths do not exist by construction. The endpoints implemented are the
// distribution-spec read surface:
//
//	GET    /v2/                              the spec's ping
//	GET|HEAD /v2/<repo>/manifests/<ref>     the index (for `latest`) or a manifest
//	GET|HEAD /v2/<repo>/blobs/<digest>      any blob (config / layer / ...)
//
// `<ref>` is `latest` (the only tag; we have no name:tag split yet, so this is
// the default) or an OCI digest `sha256:<hex>` (→ the index, or a manifest).
// The inner blobs are always addressed by their `sha256:` digest — clients pull
// them that way, and we do not need a CID for anything but the outer index root.
// Push / tag-listing / auth are intentionally out of scope for v1.

// ociRepo bundles the per-request state for one OCI repository: the index key,
// its bytes, its digest, and the set of reachable digests. Building is the same
// handful of backend calls `oci ls` makes (one Get of the index, then a walk of
// its manifests).
type ociRepo struct {
	repo        string
	backend     storage.StorageBackend
	indexKey    storage.Digest
	indexBytes  []byte
	indexDigest oci.Digest
	reachable   map[oci.Digest]bool
}

// errNameUnknown is the sentinel a ref failed to resolve to a stored object;
// the handler maps it to the distribution NAME_UNKNOWN (404) error.
var errNameUnknown = errors.New("oci serve: name unknown")

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

// newOCIRepo builds the per-repo resolver: resolve the repository to an index
// key, stream the index.json, and walk it to the reachable set. A name that
// resolves to content that is not a valid OCI image index (e.g. a bare blob
// added with `add`) yields an error the handler reports as NAME_UNKNOWN.
func newOCIRepo(backend storage.StorageBackend, repo string) (*ociRepo, error) {
	key, err := resolveIndexKey(backend, repo)
	if err != nil {
		return nil, err
	}

	r, _, err := backend.Get(key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	indexBytes, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	// Reachable walks index → manifests → config+layers, which also validates the
	// index (a non-index blob fails here). It is the same call `oci ls` and
	// `oci export` rely on.
	reachable, err := oci.Reachable(storeSource{backend: backend}, indexBytes)
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

// ociV2Handler returns an http.Handler for the /v2/ subtree, wired in runServe
// as `mux.Handle("/v2/", ociV2Handler(backend))`. It mirrors the flat-handler
// style of serveBlob/serveName (which take the shared backend as an argument).
func ociV2Handler(backend storage.StorageBackend) http.Handler {
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

	id, err := identity.FromOCI(d)
	if err != nil {
		ociError(w, http.StatusBadRequest, "MANIFEST_INVALID", d.String())
		return
	}

	reader, _, err := obj.backend.Get(id)
	if err != nil {
		ociError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", d.String())
		return
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		ociError(w, http.StatusInternalServerError, "BLOB_UNKNOWN", d.String())
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

	id, err := identity.FromOCI(d)
	if err != nil {
		ociError(w, http.StatusInternalServerError, "BLOB_UNKNOWN", d.String())
		return
	}

	reader, size, err := obj.backend.Get(id)
	if err != nil {
		ociError(w, http.StatusNotFound, "BLOB_UNKNOWN", d.String())
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", d.String())
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
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
