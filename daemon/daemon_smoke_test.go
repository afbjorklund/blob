package daemon

import (
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"blob/cid"
	"blob/identity"
	"blob/storage"
)

// TestOCIServeSmoke is the opt-in, end-to-end counterpart of TestOCIServe for
// the /v2/ distribution endpoint. Where TestOCIServe holds the wire contract
// against an in-memory backend, this drives the *real* pipeline a user uses:
// a real dir backend, a single-manifest OCI layout stored in it (the same
// `storeLayout` the unit test uses), the real daemon.Handler served over a real
// TCP socket, and probed with a stock `curl`. Like TestDirSmoke /
// TestHTTPSmoke / TestS3Smoke it runs only when its env var is set, so
// `go test ./...` stays green in sandboxes.
//
//	BLOB_SERVE_SMOKE=1 go test -v -run TestOCIServeSmoke ./daemon
func TestOCIServeSmoke(t *testing.T) {
	if os.Getenv("BLOB_SERVE_SMOKE") == "" {
		t.Skip("set BLOB_SERVE_SMOKE=1 to run the /v2/ smoke test (needs a scratch dir and `curl`)")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("the /v2/ smoke test needs `curl` on PATH")
	}

	dir := t.TempDir()
	backend, err := storage.NewStorage(storage.StorageDir, dir, "", "", storage.Auth{}, storage.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	l := buildTestLayout(t)
	storeLayout(t, backend, "smoke", l)

	srv := httptest.NewServer(New(backend).Handler())
	t.Cleanup(srv.Close)
	base := "http://" + srv.Listener.Addr().String()

	// The index bytes and CID, read straight from the store (the ground truth
	// to serve): the index key is the name's key, the CID derives by peeking its
	// first byte (Json → `{`), which is exactly what `blob oci import` printed.
	key, err := backend.Lookup("smoke")
	if err != nil {
		t.Fatal(err)
	}
	first, err := backend.Peek(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.FromOCI(l.indexDigest)
	if err != nil {
		t.Fatal(err)
	}
	if string(key) != string(id) {
		t.Fatalf("name key %q != index key %q", key, id)
	}
	c := cid.Build(cid.CodecForFirstByte(first[0]), key)
	cidStr, err := cid.Render(c)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := backend.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	want, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Ping: 200 with the pull capability.
	if body, code, err := curlGET(t, base+"/v2/"); err != nil || code != "200" || !strings.Contains(body, "pull") {
		t.Fatalf("ping = %q (%s): %v; want 200 with the pull capability", body, code, err)
	}

	// `latest` is our one synthesised tag (→ the layout root). The *primary*
	// repository is blob/<CID> (the exact CID `blob oci import` prints); the
	// secondary is name/<name> (same as the HTTP /name/<name>). Both must
	// serve the stored index bytes over the real socket.
	if body, code, err := curlGET(t, base+"/v2/blob/"+cidStr+"/manifests/latest"); err != nil || code != "200" || body != string(want) {
		t.Fatalf("manifest latest (blob/<CID>) = %q (%s): %v; want the stored index bytes", body, code, err)
	}
	if body, code, err := curlGET(t, base+"/v2/name/smoke/manifests/latest"); err != nil || code != "200" || body != string(want) {
		t.Fatalf("manifest latest (name/<name>) = %q (%s): %v; want the stored index bytes", body, code, err)
	}

	t.Logf("served the smoke layout (%s) over %s", cidStr, base)
}

// curlGET issues a stock-`curl` GET and returns (body, status-code string). It
// splits the body and the code into two requests so a 4xx with a body is never
// mistaken for a 200 (curl here runs without --fail, so a non-2xx is a normal,
// inspectable outcome rather than an error exit).
func curlGET(t *testing.T, url string) (string, string, error) {
	t.Helper()
	body, err := exec.Command("curl", "-s", url).Output()
	if err != nil {
		return string(body), "", err
	}
	code, err := exec.Command("curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", url).Output()
	if err != nil {
		return string(body), "", err
	}
	return string(body), strings.TrimSpace(string(code)), nil
}
