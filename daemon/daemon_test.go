package daemon

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"blob/cid"
	"blob/identity"
	"blob/oci"
	"blob/storage"
)

// memBackend is an in-memory storage.StorageBackend for the handler tests:
// a blob map + a name map, mirroring the in-memory stores the storage backend
// tests use. The daemon speaks only the StorageBackend interface, so this
// stands in for a real read-only backend (the handler only calls Lookup / Get,
// so Put / Index / Peek / Close are minimal).
type memBackend struct {
	mu    sync.Mutex
	blobs map[string][]byte // identity (multihash bytes) -> blob
	names map[string]string // name -> identity
}

func newMemBackend() *memBackend {
	return &memBackend{blobs: map[string][]byte{}, names: map[string]string{}}
}

func (m *memBackend) Put(hash storage.Digest, size int64, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[string(hash)] = b
	return nil
}

// testMtime is the (fixed) last-modified time the in-memory backend reports
// for every blob, so the Last-Modified header the gateway emits is stable and
// assertable. It is well past the Unix epoch, so the "0 means unknown" rule in
// the gateway's serveBlob does not apply here.
const testMtime = int64(1000000000)

func (m *memBackend) Get(hash storage.Digest) (io.ReadCloser, int64, int64, error) {
	m.mu.Lock()
	b, ok := m.blobs[string(hash)]
	m.mu.Unlock()
	if !ok {
		return nil, 0, 0, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), testMtime, nil
}

func (m *memBackend) Has(hash storage.Digest) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.blobs[string(hash)]
	return ok, nil
}

func (m *memBackend) Index(name string, hash storage.Digest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.names[name] = string(hash)
	return nil
}

func (m *memBackend) Lookup(name string) (storage.Digest, error) {
	m.mu.Lock()
	k, ok := m.names[name]
	m.mu.Unlock()
	if !ok {
		return nil, errors.New("name not found")
	}
	return storage.Digest(k), nil
}

func (m *memBackend) Peek(hash storage.Digest, n int) ([]byte, error) {
	r, _, _, err := m.Get(hash)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if len(b) > n {
		b = b[:n]
	}
	return b, nil
}

func (m *memBackend) Names() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.names))
	for n := range m.names {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

func (m *memBackend) Close() error { return nil }

var _ storage.StorageBackend = (*memBackend)(nil)

// testLayout is a minimal single-manifest OCI layout: an index naming one
// manifest, which names a config and a layer.
type testLayout struct {
	indexBytes     []byte
	indexDigest    oci.Digest
	manifestBytes  []byte
	manifestDigest oci.Digest
	configBytes    []byte
	configDigest   oci.Digest
	layerBytes     []byte
	layerDigest    oci.Digest
}

func buildTestLayout(t *testing.T) *testLayout {
	t.Helper()

	layer := []byte("tar-like layer payload")
	layerDigest := oci.DigestOf(layer)

	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDigest := oci.DigestOf(config)

	m := oci.Manifest{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeImageManifest,
		Config:        oci.Descriptor{MediaType: oci.MediaTypeImageConfig, Digest: configDigest, Size: int64(len(config))},
		Layers:        []oci.Descriptor{{MediaType: oci.MediaTypeImageLayer, Digest: layerDigest, Size: int64(len(layer))}},
	}
	manifestBytes, err := oci.Marshal(&m)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := oci.DigestOf(manifestBytes)

	idx := oci.Index{
		SchemaVersion: 2,
		MediaType:     oci.MediaTypeImageIndex,
		Manifests:     []oci.Descriptor{{MediaType: oci.MediaTypeImageManifest, Digest: manifestDigest, Size: int64(len(manifestBytes))}},
	}
	indexBytes, err := oci.Marshal(&idx)
	if err != nil {
		t.Fatal(err)
	}
	indexDigest := oci.DigestOf(indexBytes)

	return &testLayout{
		indexBytes:     indexBytes,
		indexDigest:    indexDigest,
		manifestBytes:  manifestBytes,
		manifestDigest: manifestDigest,
		configBytes:    config,
		configDigest:   configDigest,
		layerBytes:     layer,
		layerDigest:    layerDigest,
	}
}

// storeLayout stores every blob of the layout under its store identity and
// records name → the index key (what `oci import` does). It takes the backend
// interface so the unit test can seed an in-memory store and the smoke test a
// real dir backend.
func storeLayout(t *testing.T, be storage.StorageBackend, name string, l *testLayout) {
	t.Helper()
	put := func(d oci.Digest, b []byte) {
		id, err := identity.FromOCI(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := be.Put(id, int64(len(b)), bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	put(l.indexDigest, l.indexBytes)
	put(l.manifestDigest, l.manifestBytes)
	put(l.configDigest, l.configBytes)
	put(l.layerDigest, l.layerBytes)

	id, err := identity.FromOCI(l.indexDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Index(name, id); err != nil {
		t.Fatal(err)
	}
}

// newTestServer serves the backend through the real daemon.Handler (the same
// mux the CLI serves), so the handler is tested against an in-memory backend
// but the exact routing the daemon wires.
func newTestServer(t *testing.T, be *memBackend) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(be).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func doHTTP(t *testing.T, method, url string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b, resp.Header
}

// TestOCIServe exercises the distribution read surface: ping, the primary
// `blob/<CID>` repository (the same CID `blob oci import` printed), the
// secondary `name/<name>` repository, manifest by digest and by the `latest`
// tag, HEAD, blobs (layer, config, unknown), OPTIONS, an unknown repository,
// and the Content-Type / Docker-Content-Digest headers. It runs against an
// in-memory backend holding a single-manifest layout, served through the same
// mux the daemon's Handler builds.
func TestOCIServe(t *testing.T) { //nolint:gocyclo
	be := newMemBackend()
	l := buildTestLayout(t)
	storeLayout(t, be, "test", l)
	srv := newTestServer(t, be)

	// The outer (index-root) CID is exactly what `blob oci import` would print:
	// the index bytes under the Json codec, rendered base32hex. identity.ToOCI
	// is the reverse of identity.FromOCI, so we can build the CID from the
	// index digest.
	idxKey, err := identity.FromOCI(l.indexDigest)
	if err != nil {
		t.Fatal(err)
	}
	c := cid.Build(cid.CodeJson, idxKey)
	cidStr, err := cid.Render(c)
	if err != nil {
		t.Fatal(err)
	}
	repoByID := "blob/" + cidStr
	const repoByName = "name/test"

	zeroDigest := "sha256:" + strings.Repeat("0", 64)

	type tc struct {
		name        string
		method      string
		path        string
		wantStatus  int
		wantBody    []byte
		noBody      bool
		contains    []string
		wantHeaders map[string]string
	}
	cases := []tc{
		{
			name:        "ping",
			method:      http.MethodGet,
			path:        "/v2/",
			wantStatus:  http.StatusOK,
			contains:    []string{`"pull"`},
			wantHeaders: map[string]string{"Content-Type": "application/json"},
		},
		{
			name:        "primary: manifest by index digest",
			method:      http.MethodGet,
			path:        "/v2/" + repoByID + "/manifests/" + l.indexDigest.String(),
			wantStatus:  http.StatusOK,
			wantBody:    l.indexBytes,
			wantHeaders: map[string]string{"Docker-Content-Digest": l.indexDigest.String(), "Content-Type": oci.MediaTypeImageIndex},
		},
		{
			// X-Blob-Cid: the store's CID for the index, discoverable even over
			// the OCI surface (the HTTP gateway's /name/<name> 302 exposes the
			// same value in its Location) — and independent of how the client
			// addressed it (here via name/).
			name:        "secondary: manifest latest exposes X-Blob-Cid",
			method:      http.MethodGet,
			path:        "/v2/" + repoByName + "/manifests/latest",
			wantStatus:  http.StatusOK,
			wantBody:    l.indexBytes,
			wantHeaders: map[string]string{"X-Blob-Cid": cidStr},
		},
		{
			name:       "primary: manifest by tag latest",
			method:     http.MethodGet,
			path:       "/v2/" + repoByID + "/manifests/latest",
			wantStatus: http.StatusOK,
			wantBody:   l.indexBytes,
		},
		{
			name:       "primary: manifest wrong tag",
			method:     http.MethodGet,
			path:       "/v2/" + repoByID + "/manifests/v1.2",
			wantStatus: http.StatusNotFound,
			contains:   []string{"NAME_UNKNOWN"},
		},
		{
			name:        "primary: manifest HEAD",
			method:      http.MethodHead,
			path:        "/v2/" + repoByID + "/manifests/" + l.indexDigest.String(),
			wantStatus:  http.StatusOK,
			noBody:      true,
			wantHeaders: map[string]string{"Docker-Content-Digest": l.indexDigest.String(), "Content-Length": strconv.Itoa(len(l.indexBytes))},
		},
		{
			name:       "secondary: manifest by tag latest",
			method:     http.MethodGet,
			path:       "/v2/" + repoByName + "/manifests/latest",
			wantStatus: http.StatusOK,
			wantBody:   l.indexBytes,
		},
		{
			name:       "primary: blob layer",
			method:     http.MethodGet,
			path:       "/v2/" + repoByID + "/blobs/" + l.layerDigest.String(),
			wantStatus: http.StatusOK,
			wantBody:   l.layerBytes,
		},
		{
			name:       "primary: blob config",
			method:     http.MethodGet,
			path:       "/v2/" + repoByID + "/blobs/" + l.configDigest.String(),
			wantStatus: http.StatusOK,
			wantBody:   l.configBytes,
		},
		{
			name:       "primary: blob unknown digest",
			method:     http.MethodGet,
			path:       "/v2/" + repoByID + "/blobs/" + zeroDigest,
			wantStatus: http.StatusNotFound,
			contains:   []string{"BLOB_UNKNOWN"},
		},
		{
			name:        "primary: blob HEAD",
			method:      http.MethodHead,
			path:        "/v2/" + repoByID + "/blobs/" + l.layerDigest.String(),
			wantStatus:  http.StatusOK,
			noBody:      true,
			wantHeaders: map[string]string{"Content-Length": strconv.Itoa(len(l.layerBytes)), "Docker-Content-Digest": l.layerDigest.String()},
		},
		{
			name:        "primary: OPTIONS manifest",
			method:      http.MethodOptions,
			path:        "/v2/" + repoByID + "/manifests/latest",
			wantStatus:  http.StatusOK,
			noBody:      true,
			wantHeaders: map[string]string{"Docker-Distribution-API-Version": "registry/2.0"},
		},
		{
			name:       "unknown repository",
			method:     http.MethodGet,
			path:       "/v2/no/such/manifests/latest",
			wantStatus: http.StatusNotFound,
			contains:   []string{"NAME_UNKNOWN"},
		},
		{
			name:       "invalid repository",
			method:     http.MethodGet,
			path:       "/v2/foo/manifests/latest",
			wantStatus: http.StatusNotFound,
			contains:   []string{"NAME_UNKNOWN"},
		},
		{
			name:       "primary: bad CID",
			method:     http.MethodGet,
			path:       "/v2/blob/nope-/-x/manifests/latest",
			wantStatus: http.StatusNotFound,
			contains:   []string{"NAME_UNKNOWN"},
		},
		{
			name:        "primary: manifest latest Content-Type",
			method:      http.MethodGet,
			path:        "/v2/" + repoByID + "/manifests/latest",
			wantStatus:  http.StatusOK,
			wantBody:    l.indexBytes,
			wantHeaders: map[string]string{"Content-Type": oci.MediaTypeImageIndex},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, hdr := doHTTP(t, c.method, srv.URL+c.path)
			if status != c.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", status, c.wantStatus, body)
			}
			if c.noBody {
				if len(body) != 0 {
					t.Errorf("expected empty body for HEAD/OPTIONS, got %d bytes", len(body))
				}
			}
			if c.wantBody != nil {
				if !bytes.Equal(body, c.wantBody) {
					t.Errorf("body = %q, want %q", body, c.wantBody)
				}
			}
			for _, s := range c.contains {
				if !strings.Contains(string(body), s) {
					t.Errorf("body %q missing %q", body, s)
				}
			}
			for k, v := range c.wantHeaders {
				if got := hdr.Get(k); got != v {
					t.Errorf("header %s = %q, want %q", k, got, v)
				}
			}
		})
	}

	t.Run("namespaces coexist", func(t *testing.T) {
		// The same index blob is reachable both under /blob/<CID> (the store's
		// own surface) and under /v2/blob/<CID>/blobs/<digest> (the OCI surface);
		// the bodies must be identical and the prefixes must not shadow each
		// other (a CID is a CID).
		st1, b1, _ := doHTTP(t, http.MethodGet, srv.URL+"/blob/"+cidStr)
		if st1 != http.StatusOK || !bytes.Equal(b1, l.indexBytes) {
			t.Fatalf("/blob/<CID> = %d %q", st1, b1)
		}
		st2, b2, _ := doHTTP(t, http.MethodGet, srv.URL+"/v2/"+repoByID+"/blobs/"+l.indexDigest.String())
		if st2 != http.StatusOK || !bytes.Equal(b2, l.indexBytes) {
			t.Fatalf("/v2 blobs = %d %q", st2, b2)
		}
		if !bytes.Equal(b1, b2) {
			t.Errorf("namespace bodies differ")
		}
	})

	t.Run("blob exposes Last-Modified when the backend reports one", func(t *testing.T) {
		// The /blob/<CID> surface surfaces the store's last-modified time via
		// Last-Modified (RFC 7231), for the benefit of HTTP clients that do
		// conditional GETs or maintenance tooling. The memBackend returns a
		// fixed testMtime, so the expected header is deterministic.
		_, _, hdr := doHTTP(t, http.MethodGet, srv.URL+"/blob/"+cidStr)
		want := time.Unix(testMtime, 0).UTC().Format(http.TimeFormat)
		if got := hdr.Get("Last-Modified"); got != want {
			t.Fatalf("Last-Modified = %q, want %q", got, want)
		}
	})
}

// TestNameSurface exercises the /name/<name> 302 for both a plain name and a
// registry-style name. The daemon's contract: the client sends the clean name
// (possibly URI-escaped on the wire, and possibly with a literal '/' in the
// path) and the server resolves it to the blob, deriving the CID and 302ing
// to /blob/<CID>. net/url decodes the request path once for us, so "net/url
// gives us the clean name" is the whole server-side step. This is the
// step-2 "user sees the clean name" contract on the wire, and the plain-name
// line is the regression guard for pre-step-2 stores.
func TestNameSurface(t *testing.T) {
	be := newMemBackend()
	l := buildTestLayout(t)

	// Record two names: a plain one (regression) and a registry-style one
	// (the step-2 feature). Same layout, different name — both must resolve
	// to the same CID.
	storeLayout(t, be, "busybox", l)
	storeLayout(t, be, "docker.io/library/busybox", l)
	srv := newTestServer(t, be)

	idxKey, err := identity.FromOCI(l.indexDigest)
	if err != nil {
		t.Fatal(err)
	}
	wantCID, err := cid.Render(cid.Build(cid.CodeJson, idxKey))
	if err != nil {
		t.Fatal(err)
	}
	wantLocation := "/blob/" + wantCID

	// The client can address the registry name with either a literal '/' (the
	// common case, a user pasting the name into curl) or the URI-form
	// percent-escape (%2F). Both arrive at the server as the same clean name,
	// because net/url decodes the request path once before we touch it.
	for _, path := range []string{
		"/name/busybox",
		"/name/docker.io/library/busybox",
		"/name/docker.io%2Flibrary%2Fbusybox",
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Do NOT follow the redirect: we want to see the 302 and its Location.
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		loc := resp.Header.Get("Location")
		code := resp.StatusCode
		_ = resp.Body.Close()
		if code != http.StatusFound {
			t.Errorf("GET %s = %d, want 302 (Location %q)", path, code, loc)
		} else if loc != wantLocation {
			t.Errorf("GET %s Location = %q, want %q", path, loc, wantLocation)
		}
	}
	// A name that was never recorded still 404s. (A 404 does not redirect, so
	// the default client is safe here.)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/name/no/such/name", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET unrecorded name = %d, want 404", resp.StatusCode)
	}
}
