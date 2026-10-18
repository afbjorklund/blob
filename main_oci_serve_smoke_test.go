package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"blob/cid"
	"blob/storage"
)

// TestOCIServeSmoke is the opt-in, end-to-end counterpart of TestOCIServe for
// the /v2/ distribution endpoint. Where TestOCIServe holds the wire contract
// against an in-memory backend, this drives the *real* pipeline a user uses:
// a real dir backend, a real `oci import` of a file (the same runOCIAdd the
// CLI takes), the /v2/ handler served over a real TCP socket, probed with a
// stock `curl`. Like TestDirSmoke / TestHTTPSmoke / TestS3Smoke it runs only
// when its env var is set, so `go test ./...` stays green in sandboxes.
//
//	BLOB_SERVE_SMOKE=1 go test -v -run TestOCIServeSmoke .
func TestOCIServeSmoke(t *testing.T) {
	if os.Getenv("BLOB_SERVE_SMOKE") == "" {
		t.Skip("set BLOB_SERVE_SMOKE=1 to run the /v2/ smoke test (needs a scratch dir and `curl`)")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("the /v2/ smoke test needs `curl` on PATH")
	}

	// A small JSON file: first byte `{` → Json codec, so the CID derives
	// cleanly and the layout's reachable set is index → manifest → config({}) and layer.
	payload := []byte(`{"smoke":"oci-serve","value":42}`)
	file := filepath.Join(t.TempDir(), "smoke-artifact.json")
	if err := os.WriteFile(file, payload, 0644); err != nil {
		t.Fatal(err)
	}

	// Import it into a real dir store (the CLI's own import path).
	oldPath, oldName, oldType := storagePath, ociName, storageType
	storagePath, ociName, storageType = t.TempDir(), "smoke", "dir"
	defer func() { storagePath, ociName, storageType = oldPath, oldName, oldType }()
	if err := runOCIAdd(nil, []string{file}); err != nil {
		t.Fatalf("oci import: %v", err)
	}

	// Serve the same mux runServe wires, on a random localhost port.
	backend, err := getStorageBackend(storage.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/blob/", func(w http.ResponseWriter, r *http.Request) {
		serveBlob(w, r, backend, r.URL.Path[len("/blob/"):])
	})
	mux.Handle("/v2/", ociV2Handler(backend))
	srv := httptest.NewServer(mux)
	defer srv.Close()
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
	// repository is blob/<CID> (the exact CID `blob oci import` printed); the
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
