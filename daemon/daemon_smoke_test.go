package daemon

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"blob/cid"
	"blob/identity"
	"blob/oci"
	"blob/storage"
)

// TestServeSmoke is the opt-in, end-to-end, "real" counterpart of the in-memory
// TestOCIServe: a real dir backend, a real daemon.Handler served over a real
// TCP socket, and the OCI distribution API probed with a stock `curl` (the
// probe that a `podman pull` / `oras` / `skopeo` / `crane` client would make,
// minus the client-specific behavior on top of the spec). It only runs when
// BLOB_SERVE_SMOKE is set, so `go test ./...` stays green without curl on PATH.
//
// It holds one *common protocol* — the promise to an end user — against the
// two kinds of content `oci import` produces (an OCI **image** and an OCI
// **artifact**), each in the four address/method combinations:
//
//	method × address:     HTTP-gateway CID     HTTP-gateway name         OCI CID                    OCI name
//	===============================================================================================================
//	blob by CID          GET /blob/<CID>        GET /name/<name> (302)   GET /v2/blob/<CID>/…      GET /v2/name/<name>/…
//
// and asserts that they all deliver the same stored layout root (its
// index.json bytes) with the CID discoverable on each — via `Location:
// /blob/<CID>` on the HTTP name redirect and via the `X-Blob-Cid` response
// header on the OCI manifest response (the spec's `Docker-Content-Digest`
// does not carry the codec, which our CIDs do need). The one `GET /v2/.../
// blobs/<digest>` pull also exercises the OCI blob endpoint the same way a
// real client would. No push, no tag listing, no sessions — this is just the
// read surface, held to the same contract regardless of which way a client
// gets to it.
//
//	BLOB_SERVE_SMOKE=1 go test -v -run TestServeSmoke ./daemon
func TestServeSmoke(t *testing.T) {
	if os.Getenv("BLOB_SERVE_SMOKE") == "" {
		t.Skip("set BLOB_SERVE_SMOKE=1 to run the serve smoke test (needs a scratch dir and `curl`)")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("the serve smoke test needs `curl` on PATH")
	}

	dir := t.TempDir()
	backend, err := storage.NewStorage(storage.StorageDir, dir, "", "", storage.Auth{}, storage.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	// Two layouts stored the same way `oci import` would — an image and an
	// artifact, one name each — so both content kinds are served over the wire.
	layouts := []puller{
		newImageLayout(t, backend, "image"),
		newArtifactLayout(t, backend, "art"),
	}

	srv := httptest.NewServer(New(backend).Handler())
	t.Cleanup(srv.Close)
	base := "http://" + srv.Listener.Addr().String()

	for _, p := range layouts {
		t.Run(p.name, func(t *testing.T) { serveProtocol(t, base, p) })
	}
}

// puller is one stored layout (an image or an artifact) reduced to the few
// facts the smoke protocol needs: the name it is indexed under, the outer
// (index-root) CID, the root object (the index bytes), and the root's OCI
// digest (addressable at the /v2/ blobs endpoint like any reachable blob).
// nameIsIndex is false for a stored artifact's file-name (which, by the
// CLI's naming policy, points at the file's bytes rather than the scaffold)
// — the by-name subtests then skip for that puller.
type puller struct {
	name        string
	cid         string
	idx         []byte
	idxDigest   oci.Digest
	nameIsIndex bool
}

// newImageLayout builds a minimal OCI image (index → manifest → config + layer)
// and stores every reachable blob plus the name→index link, exactly as
// `oci import` does for a layout with manifests.
func newImageLayout(t *testing.T, be storage.StorageBackend, name string) puller {
	t.Helper()
	l := buildTestLayout(t)
	storeDigest(t, be, l.indexDigest, l.indexBytes)
	storeDigest(t, be, l.manifestDigest, l.manifestBytes)
	storeDigest(t, be, l.configDigest, l.configBytes)
	storeDigest(t, be, l.layerDigest, l.layerBytes)
	storeName(t, be, name, l.indexDigest)
	return puller{name: name, cid: outerCID(t, be, name), idx: l.indexBytes, idxDigest: l.indexDigest, nameIsIndex: true}
}

// newArtifactLayout wraps a bare payload as a single-layer OCI artifact
// (index → manifest[artifactType] → `{}` config + layer) and stores it. Under
// the CLI's naming policy (`oci import <file>`) a bare file's name points at
// the *file's bytes* (consistent with `blob add`), not at this scaffold — so
// the name here records against the payload, not `a.IndexDigest`, and the
// OCI-by-`name/...` subtest in `serveProtocol` does not run for artifacts.
// The scaffold remains reachable by its CID, the same way it is reachable by
// a name for images where the name does in fact point at the index.
func newArtifactLayout(t *testing.T, be storage.StorageBackend, name string) puller {
	t.Helper()
	payload := []byte("smoke artifact layer payload")
	layerD := oci.DigestOf(payload)
	a, err := oci.NewArtifact(layerD, int64(len(payload)),
		oci.MediaTypeGenericBinary, "application/vnd.blob.artifact.v1", oci.MediaTypeUnknownConfig)
	if err != nil {
		t.Fatal(err)
	}
	storeDigest(t, be, a.IndexDigest, a.IndexBytes)
	storeDigest(t, be, a.ManifestDigest, a.ManifestBytes)
	storeDigest(t, be, a.ConfigDigest, oci.EmptyConfig) // the `{}` config
	storeDigest(t, be, a.LayerDigest, payload)
	storeName(t, be, name, a.LayerDigest)
	return puller{name: name, cid: outerCIDByIndex(t, be, a.IndexDigest), idx: a.IndexBytes, idxDigest: a.IndexDigest, nameIsIndex: false}
}

// outerCIDByIndex derives the CID for a known index digest (without a name to
// look up): the index bytes under their content codec, rendered base32hex —
// the same address `oci import` prints and the /v2/blob surface serves.
func outerCIDByIndex(t *testing.T, be storage.StorageBackend, idxD oci.Digest) string {
	t.Helper()
	id, err := identity.FromOCI(idxD)
	if err != nil {
		t.Fatal(err)
	}
	r, _, _, err := be.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	c := cid.Build(cid.CodecForFirstByte(b[0]), id)
	s, err := cid.Render(c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// outerCID is a layout's outer (index-root) CID — what `blob oci import`
// prints: the index key under the content codec (peeked, the daemon's own
// name→CID rule), rendered base32hex.
func outerCID(t *testing.T, be storage.StorageBackend, name string) string {
	t.Helper()
	key, err := be.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	first, err := be.Peek(key, 1)
	if err != nil || len(first) == 0 {
		t.Fatalf("Peek index first byte: %v (len %d)", err, len(first))
	}
	c := cid.Build(cid.CodecForFirstByte(first[0]), key)
	s, err := cid.Render(c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func storeDigest(t *testing.T, be storage.StorageBackend, d oci.Digest, b []byte) {
	t.Helper()
	id, err := identity.FromOCI(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Put(id, int64(len(b)), bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
}

func storeName(t *testing.T, be storage.StorageBackend, name string, indexD oci.Digest) {
	t.Helper()
	id, err := identity.FromOCI(indexD)
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Index(name, id); err != nil {
		t.Fatal(err)
	}
}

// serveProtocol is the common pull protocol, run once per layout: the layout
// root (index) must be reachable by {CID, name} over both methods (HTTP
// gateway and OCI distribution) with the CID discoverable on each, and one
// component (the index itself — reachable at the /v2/.../blobs/ endpoint like
// any other reachable blob) must be pullable. All cells must come back 200
// with the exact stored index bytes.
func serveProtocol(t *testing.T, base string, p puller) {
	t.Helper()

	t.Run("HTTP root by CID", func(t *testing.T) {
		body, hdr, code, err := curlGET(t, base+"/blob/"+p.cid)
		if err != nil || code != "200" {
			t.Fatalf("GET /blob/<CID> = %q (%s): %v; want 200", body, code, err)
		}
		if !bytes.Equal([]byte(body), p.idx) {
			t.Fatalf("GET /blob/<CID> body = %d bytes; want the %d stored index bytes", len(body), len(p.idx))
		}
		_ = hdr // the CID is already in the URL; no separate discovery header needed
	})

	t.Run("HTTP name → 302 → /blob/<CID>", func(t *testing.T) {
		if !p.nameIsIndex {
			// A wrapped artifact's name resolves to the file's bytes (raw),
			// not the OCI scaffold — the 302 target is a raw CID, not p.cid.
			t.Skip("artifact name points at the file's bytes, not the index CID")
		}
		code, loc, err := curlHead(t, base+"/name/"+p.name)
		if err != nil || code != "302" {
			t.Fatalf("GET /name/%s = (%s): %v; want 302", p.name, code, err)
		}
		if !strings.HasSuffix(loc, "/blob/"+p.cid) {
			t.Fatalf("Location = %q; want a redirect to /blob/<CID>", loc)
		}
	})

	t.Run("OCI root by CID (X-Blob-Cid)", func(t *testing.T) {
		body, hdr, code, err := curlGET(t, base+"/v2/blob/"+p.cid+"/manifests/latest")
		if err != nil || code != "200" {
			t.Fatalf("GET /v2/blob/<CID>/manifests/latest = %q (%s): %v; want 200", body, code, err)
		}
		if !bytes.Equal([]byte(body), p.idx) {
			t.Fatalf("OCI by-CID body = %d bytes; want the %d stored index bytes", len(body), len(p.idx))
		}
		assertXBlobCid(t, hdr, p.cid)
	})

	t.Run("OCI root by name (X-Blob-Cid)", func(t *testing.T) {
		if !p.nameIsIndex {
			// The artifact's name resolves to file bytes; the OCI by-name
			// surface would try to parse them as an index and 404.
			t.Skip("artifact name points at the file's bytes, not the index")
		}
		body, hdr, code, err := curlGET(t, base+"/v2/name/"+p.name+"/manifests/latest")
		if err != nil || code != "200" {
			t.Fatalf("GET /v2/name/<name>/manifests/latest = %q (%s): %v; want 200", body, code, err)
		}
		if !bytes.Equal([]byte(body), p.idx) {
			t.Fatalf("OCI by-name body = %d bytes; want the %d stored index bytes", len(body), len(p.idx))
		}
		assertXBlobCid(t, hdr, p.cid)
	})

	// The index is itself a reachable blob (it is the very first entry of the
	// layout's reachable set), so it is pullable at the /v2/.../blobs/ endpoint
	// by its OCI digest — the same heavy pull a real client does for a layer.
	t.Run("OCI component /v2/…/blobs/<digest>", func(t *testing.T) {
		body, _, code, err := curlGET(t, base+"/v2/blob/"+p.cid+"/blobs/"+p.idxDigest.String())
		if err != nil || code != "200" {
			t.Fatalf("GET /v2/…/blobs/<digest> = %q (%s): %v; want 200", body, code, err)
		}
		if !bytes.Equal([]byte(body), p.idx) {
			t.Fatalf("component body = %d bytes; want the %d stored index bytes", len(body), len(p.idx))
		}
	})
}

// assertXBlobCid verifies the X-Blob-Cid response header matches the expected
// CID. The spec's Docker-Content-Digest does not carry the codec (which our
// CIDs do), so the store's own header is the discoverable "real path".
func assertXBlobCid(t *testing.T, hdr http.Header, want string) {
	t.Helper()
	got := hdr.Get("X-Blob-Cid")
	if got != want {
		t.Fatalf("X-Blob-Cid = %q; want %q", got, want)
	}
}

// curlGET issues a stock-`curl` GET and returns (body, headers, status-code
// string). It splits the body, code and headers into separate curls so a 4xx
// with a body is never mistaken for a 200 (curl runs without --fail, so a
// non-2xx is a normal, inspectable outcome rather than an error exit). It does
// not follow redirects (the 302 case is asserted separately via curlHead).
func curlGET(t *testing.T, url string) (string, http.Header, string, error) {
	t.Helper()
	body, err := exec.Command("curl", "-s", url).Output()
	if err != nil {
		return string(body), nil, "", err
	}
	code, err := exec.Command("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", url).Output()
	if err != nil {
		return string(body), nil, "", err
	}
	hdrOut, err := exec.Command("curl", "-s", "-D", "-", "-o", "/dev/null", url).Output()
	if err != nil {
		return string(body), nil, strings.TrimSpace(string(code)), err
	}
	hdr := http.Header{}
	for _, line := range strings.Split(strings.ReplaceAll(string(hdrOut), "\r", ""), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && strings.HasPrefix(f[0], "HTTP/") {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			hdr.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	return string(body), hdr, strings.TrimSpace(string(code)), nil
}

// curlHead issues a stock-`curl` -I and returns (status code, the value of the
// Location header, if any). It is the no-body form used to assert the
// /name/<name> 302 and its target without following it.
func curlHead(t *testing.T, url string) (string, string, error) {
	t.Helper()
	out, err := exec.Command("curl", "-s", "-I", url).Output()
	if err != nil {
		return "", "", err
	}
	var code, location string
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && strings.HasPrefix(f[0], "HTTP/") {
			code = f[1]
		}
		if i := strings.Index(line, "Location:"); i >= 0 {
			location = strings.TrimSpace(line[i+len("Location:"):])
		}
	}
	return code, location, nil
}
