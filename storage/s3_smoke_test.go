package storage

import (
	"bytes"
	"os"
	"strconv"
	"testing"
)

// TestS3Smoke is an opt-in end-to-end probe against a real S3 / Ceph RGW
// endpoint. It only runs when BLOB_S3_ENDPOINT and BLOB_S3_BUCKET are both
// set; otherwise it skips, so `go test ./...` stays green in sandboxes without
// a cluster. Credentials come from BLOB_USER / BLOB_PASS (or empty for
// anonymous).
//
// It exercises the code paths the in-process shim cannot: real SigV4 signing
// against RGW, the ranged-GET Peek, the last-add-wins alias re-point, and the
// read-only guard. Run with, e.g.:
//
//	BLOB_S3_ENDPOINT=127.0.0.1:7480 BLOB_S3_BUCKET=blobstore \
//	BLOB_USER=... BLOB_PASS=... \
//	go test -v -run TestS3Smoke ./storage/
func TestS3Smoke(t *testing.T) {
	endpoint := os.Getenv("BLOB_S3_ENDPOINT")
	bucket := os.Getenv("BLOB_S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("set BLOB_S3_ENDPOINT and BLOB_S3_BUCKET to run the s3 smoke test")
	}
	auth := Auth{User: os.Getenv("BLOB_USER"), Password: os.Getenv("BLOB_PASS")}

	w, ro := newSmokePairs(t, StorageS3, endpoint, bucket, auth)

	// Two payloads, each keyed by its own (well-formed) sha2-256 multihash.
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
		got, err := readBlob(ro, key)
		if err != nil {
			t.Fatalf("Get(%q): %v", key, err)
		}
		if !bytes.Equal(got, payloadA) && !bytes.Equal(got, payloadB) {
			t.Fatalf("Get(%q) = %q; want %q or %q", key, got, payloadA, payloadB)
		}
	}
}

// newSmokePairs opens a read-write and a read-only backend of the given type
// against a real server (path = base URL for http), registering cleanup. It
// is shared by the http and s3 smoke tests so they are held to the same
// end-to-end contract (and skip symmetrically on their own env var).
func newSmokePairs(t *testing.T, backend StorageType, endpoint, bucket string, auth Auth) (StorageBackend, StorageBackend) {
	t.Helper()
	// Region is set to us-east-1 so the client does not need to make a
	// bucket-?location lookup (the in-process SIM and a plain RGW do not serve
	// one in the format we expect). A real AWS-like deployment can override via
	// BLOB_S3_REGION.
	w, err := NewStorage(backend, endpoint, bucket, "us-east-1", auth, ReadWrite)
	if err != nil {
		t.Fatalf("NewStorage(%s, %q, %q, rw): %v", backend, endpoint, bucket, err)
	}
	ro, err := NewStorage(backend, endpoint, bucket, "us-east-1", auth, ReadOnly)
	if err != nil {
		t.Fatalf("NewStorage(%s, %q, %q, ro): %v", backend, endpoint, bucket, err)
	}
	t.Cleanup(func() {
		_ = ro.Close()
		_ = w.Close()
	})
	return w, ro
}
