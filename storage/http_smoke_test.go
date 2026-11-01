package storage

import (
	"bytes"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
)

// smokeSeq is the shared nonce for the opt-in smoke tests (http and s3), so
// concurrent runs use distinct keys/aliases and do not trample each other.
var smokeSeq atomic.Uint64

// TestHTTPSmoke is the http-backend counterpart of TestS3Smoke: an opt-in
// end-to-end probe against a real HTTP server speaking the backend's on-wire
// contract (PUT/GET/HEAD on /<hex> for blobs, PUT/GET on /index/<name> for the
// name index). It only runs when BLOB_HTTP_PATH is set, so `go test ./...`
// stays green without a server. Credentials come from BLOB_USER / BLOB_PASS.
//
// It exercises the same end-to-end contract as the s3 test (and the in-process
// shim cannot against a real proxy): real network round-trips, the ranged-GET
// Peek, last-add-wins alias re-point, and the read-only guard. The test server
// must implement the on-wire contract from storage/http.go:
//
//	blob   <base>/<hex>            PUT body=blob    GET/HEAD
//	index  <base>/index/<name>     PUT body=<hex>   GET
//
// Run with, e.g.:
//
//	BLOB_HTTP_PATH=http://localhost/blob \
//	BLOB_USER=... BLOB_PASS=... \
//	go test -v -run TestHTTPSmoke ./storage/
func TestHTTPSmoke(t *testing.T) {
	path := os.Getenv("BLOB_HTTP_PATH")
	if path == "" {
		t.Skip("set BLOB_HTTP_PATH to run the http smoke test")
	}
	auth := Auth{User: os.Getenv("BLOB_USER"), Password: os.Getenv("BLOB_PASS")}

	w, ro := newSmokePairs(t, StorageHTTP, path, "", auth)

	nonce := strconv.FormatUint(smokeSeq.Add(1), 10)
	payloadA := []byte("smoke-A " + nonce)
	payloadB := []byte("smoke-B " + nonce)
	keyA := sha256Multihash(payloadA)
	keyB := sha256Multihash(payloadB)

	if err := w.Put(keyA, int64(len(payloadA)), bytes.NewReader(payloadA)); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	if err := w.Put(keyB, int64(len(payloadB)), bytes.NewReader(payloadB)); err != nil {
		t.Fatalf("Put B: %v", err)
	}

	if p, err := w.Peek(keyA, 7); err != nil || string(p) != "smoke-A" {
		t.Fatalf("Peek A = %q, %v; want %q", p, err, "smoke-A")
	}
	if p, err := ro.Peek(keyB, 7); err != nil || string(p) != "smoke-B" {
		t.Fatalf("Peek B (read-only backend) = %q, %v; want %q", p, err, "smoke-B")
	}

	alias := "smoke-" + nonce
	if err := w.Index(alias, keyA); err != nil {
		t.Fatalf("Index alias→A: %v", err)
	}
	if d, err := w.Lookup(alias); err != nil || !bytes.Equal(d, keyA) {
		t.Fatalf("Lookup alias = %q, %v; want %q", d, err, keyA)
	}

	// Last add wins: re-point the alias at B and confirm the move.
	if err := w.Index(alias, keyB); err != nil {
		t.Fatalf("Index alias→B (last add wins): %v", err)
	}
	if d, err := w.Lookup(alias); err != nil || !bytes.Equal(d, keyB) {
		t.Fatalf("Lookup alias after re-add = %q, %v; want %q (last add wins)", d, err, keyB)
	}

	// Names: the autoindex of /index/ must include our new name (the server
	// is expected to have `autoindex_format json` per the backend contract).
	found := false
	if ns, err := w.Names(); err != nil {
		t.Fatalf("Names(): %v", err)
	} else {
		for _, n := range ns {
			if n == alias {
				found = true
			}
		}
		if !found {
			t.Fatalf("Names() = %v; want to include %q", ns, alias)
		}
	}

	// Read-only guard: the same writes must be refused on the read-only pair.
	if err := ro.Put(keyA, 0, bytes.NewReader(nil)); err != ErrReadOnly {
		t.Fatalf("read-only Put = %v; want ErrReadOnly", err)
	}
	if err := ro.Index("smoke-x", keyA); err != ErrReadOnly {
		t.Fatalf("read-only Index = %v; want ErrReadOnly", err)
	}

	// Has / Get round-trip on both keys.
	for _, key := range []Digest{keyA, keyB} {
		if ok, err := w.Has(key); err != nil || !ok {
			t.Fatalf("Has(%q) = %v, %v; want true, nil", key, ok, err)
		}
		// Stat (metadata-only) confirms the size without opening a reader.
		if st, serr := ro.Stat(key); serr != nil || st.Size <= 0 {
			t.Fatalf("Stat(%q) = %+v, %v; want size > 0", key, st, serr)
		}
		got, err := readBlob(ro, key)
		if err != nil {
			t.Fatalf("Get(%q): %v", key, err)
		}
		if !bytes.Equal(got, payloadA) && !bytes.Equal(got, payloadB) {
			t.Fatalf("Get(%q) = %q; want %q or %q", key, got, payloadA, payloadB)
		}
	}
}
