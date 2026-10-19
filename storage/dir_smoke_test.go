package storage

import (
	"bytes"
	"os"
	"strconv"
	"testing"
)

// TestDirSmoke is the dir-backend counterpart of TestHTTPSmoke / TestS3Smoke:
// an opt-in end-to-end probe against a real directory on whatever filesystem
// the user has chosen — a network file share (NFS, CIFS/SMB), a FUSE mount
// (e.g. rclone, mount-s3, sshfs), an exotic local mount, etc. It only runs
// when BLOB_DIR_PATH is set, so `go test ./...` stays green in sandboxes / CI
// where the flag directory would just be a scratch tempfile (the table-driven
// round-trip in storage_test.go already covers that case in-process).
//
// It exercises the same end-to-end contract as the other smoke tests: Put /
// Get / Peek / Has, the name-index round-trip with last-add-wins, and the
// read-only guard. Run with, e.g.:
//
//	BLOB_DIR_PATH=/mnt/nfs/blobstore \
//	go test -v -run TestDirSmoke ./storage/
func TestDirSmoke(t *testing.T) {
	path := os.Getenv("BLOB_DIR_PATH")
	if path == "" {
		t.Skip("set BLOB_DIR_PATH to run the dir smoke test")
	}

	w, ro := newSmokePairs(t, StorageDir, path, "", Auth{})

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

	// Names: the index directory must include our new name.
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
		got, err := readBlob(ro, key)
		if err != nil {
			t.Fatalf("Get(%q): %v", key, err)
		}
		if !bytes.Equal(got, payloadA) && !bytes.Equal(got, payloadB) {
			t.Fatalf("Get(%q) = %q; want %q or %q", key, got, payloadA, payloadB)
		}
	}
}
