package storage

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// sha256Multihash builds a well-formed sha2-256 multihash over data:
// 0x12 (sha2-256) 0x20 (32) + the 32-byte digest. Keys in this store are
// digests of this size; values (the blobs) are the large part.
func sha256Multihash(data []byte) []byte {
	sum := sha256.Sum256(data)
	return append([]byte{0x12, 0x20}, sum[:]...)
}

// backendNames is the set of backends the shared tests run against, so the
// dir, sql and http backends are held to the same contract.
func backendNames() []StorageType {
	return []StorageType{StorageDir, StorageSQL, StorageHTTP}
}

// pathFor maps a backend type to the path NewStorage expects under dir: a
// directory for the dir backend, a database file for the sql backend. (The http
// backend takes a base URL, so its path is resolved separately in the tests.)
func pathFor(backend StorageType, dir string) string {
	if backend == StorageSQL {
		return filepath.Join(dir, "store.db")
	}
	return dir
}

func mustNew(t *testing.T, backend StorageType, path string, mode Mode) StorageBackend {
	t.Helper()
	b, err := NewStorage(backend, path, mode)
	if err != nil {
		t.Fatalf("NewStorage(%s, %q, %v): %v", backend, path, mode, err)
	}
	return b
}

// newHTTPTestServer starts an in-memory httptest server implementing the
// on-wire contract the http backend speaks (GET/PUT/HEAD on /<hex> for blobs
// and PUT/GET on /name/<name> for the name index, the latter body the hex
// multihash). It is the remote the tests point NewStorage(StorageHTTP) at, so
// the http backend is held to the same read/write contract as dir and sql. It
// does not honor Range requests (a permitted server behavior), so Peek is
// exercised via a full 200 body.
func newHTTPTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	var mu sync.Mutex
	blobs := map[string][]byte{}
	index := map[string][]byte{}

	handle := func(w http.ResponseWriter, r *http.Request) {
		target := strings.TrimPrefix(r.URL.Path, "/")

		if isIndexPath(target) {
			name := strings.TrimPrefix(target, indexPrefix+"/")
			switch r.Method {
			case http.MethodPut:
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				mu.Lock()
				index[name] = body
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			case http.MethodGet:
				mu.Lock()
				v, found := index[name]
				mu.Unlock()
				if !found {
					http.Error(w, "name not found", http.StatusNotFound)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(v)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
			return
		}

		id := target
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			blobs[id] = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			mu.Lock()
			b, found := blobs[id]
			mu.Unlock()
			if !found {
				http.Error(w, "blob not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b)
		case http.MethodHead:
			mu.Lock()
			b, found := blobs[id]
			mu.Unlock()
			if !found {
				http.Error(w, "blob not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(handle))
	t.Cleanup(srv.Close)
	return srv
}

// isIndexPath reports whether target is a name index entry ("index/<name>"),
// as opposed to a blob id (a bare hex segment).
func isIndexPath(target string) bool {
	return strings.HasPrefix(target, indexPrefix+"/")
}

// TestRoundTrip exercises the full read/write contract of every backend:
// Put/Has/Get round-trips (including an empty value), Index/Lookup with
// last-add-wins, Peek, persistence across reopening, and the read-only guard
// (a read-only backend reads fine but refuses Put/Index with ErrReadOnly).
//
// The body is long because it makes one assertion per backend capability —
// splitting it out would lose more than it gains, so it is excused from the
// gocyclo budget rather than broken into helpers.
func TestRoundTrip(t *testing.T) { //nolint:gocyclo
	for _, backend := range backendNames() {
		t.Run(string(backend), func(t *testing.T) {
			dir := t.TempDir()
			path := pathFor(backend, dir)
			if backend == StorageHTTP {
				// One shared in-memory server for the whole subtest, so the
				// store persists across the reopen steps below.
				path = newHTTPTestServer(t).URL
			}

			big := make([]byte, 4<<10)
			if _, err := rand.Read(big); err != nil {
				t.Fatal(err)
			}

			type entry struct{ key, value []byte }
			entries := []entry{
				{key: []byte("key-1"), value: []byte(`{"a":1,"b":"two"}`)},
				{key: []byte("key-2"), value: big},
				{key: []byte("key-3"), value: nil}, // empty value
			}

			w := mustNew(t, backend, path, ReadWrite)
			for _, e := range entries {
				if err := w.Put(e.key, int64(len(e.value)), bytes.NewReader(e.value)); err != nil {
					t.Fatalf("Put(%q): %v", e.key, err)
				}
			}
			wrtKey := entries[0].key
			for _, e := range entries {
				if ok, err := w.Has(e.key); err != nil || !ok {
					t.Fatalf("Has(%q) = %v, %v; want true, nil", e.key, ok, err)
				}
			}
			if ok, err := w.Has([]byte("missing")); err != nil || ok {
				t.Fatalf("Has(missing) = %v, %v; want false, nil", ok, err)
			}

			// Name index: two blobs under distinct names.
			if err := w.Index("alpha", entries[0].key); err != nil {
				t.Fatal(err)
			}
			if err := w.Index("beta", entries[1].key); err != nil {
				t.Fatal(err)
			}
			if d, err := w.Lookup("alpha"); err != nil || !bytes.Equal(d, entries[0].key) {
				t.Fatalf("Lookup(alpha) = %q, %v; want %q", d, err, entries[0].key)
			}
			if d, err := w.Lookup("beta"); err != nil || !bytes.Equal(d, entries[1].key) {
				t.Fatalf("Lookup(beta) = %q, %v; want %q", d, err, entries[1].key)
			}
			// Last add wins: re-point alpha at the second blob.
			if err := w.Index("alpha", entries[1].key); err != nil {
				t.Fatal(err)
			}
			if d, err := w.Lookup("alpha"); err != nil || !bytes.Equal(d, entries[1].key) {
				t.Fatalf("Lookup(alpha) after re-add = %q, %v; want %q (last add wins)", d, err, entries[1].key)
			}

			// Peek is bounded by the request and matches the stored head.
			head := string(entries[0].value[:2]) // `{"`
			if p, err := w.Peek(entries[0].key, 2); err != nil || string(p) != head {
				t.Fatalf("Peek(key-1,2) = %q, %v; want %q", p, err, head)
			}
			if p, err := w.Peek(entries[2].key, 4); err != nil || len(p) != 0 {
				t.Fatalf("Peek(empty,4) = %q, %v; want empty, nil", p, err)
			}

			// Get streams the exact bytes back.
			for _, e := range entries {
				r, size, err := w.Get(e.key)
				if err != nil {
					t.Fatalf("Get(%q): %v", e.key, err)
				}
				if size != int64(len(e.value)) {
					t.Fatalf("Get(%q) size = %d; want %d", e.key, size, len(e.value))
				}
				got, rerr := io.ReadAll(r)
				if err := r.Close(); err != nil {
					t.Fatalf("reader Close(%q): %v", e.key, err)
				}
				if rerr != nil {
					t.Fatalf("ReadAll(Get(%q)): %v", e.key, rerr)
				}
				if !bytes.Equal(got, e.value) {
					t.Fatalf("Get(%q) = %d bytes; want %d", e.key, len(got), len(e.value))
				}
			}

			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			// Persistence: reopen read-write and the data is still there.
			w2 := mustNew(t, backend, path, ReadWrite)
			if ok, err := w2.Has(wrtKey); err != nil || !ok {
				t.Fatalf("Has after reopen = %v, %v; want true, nil", ok, err)
			}
			if err := w2.Close(); err != nil {
				t.Fatal(err)
			}

			// Read-only: reads work, writes are refused.
			ro := mustNew(t, backend, path, ReadOnly)
			if ok, err := ro.Has(wrtKey); err != nil || !ok {
				t.Fatalf("read-only Has = %v, %v; want true, nil", ok, err)
			}
			if err := ro.Put(wrtKey, 1, bytes.NewReader([]byte("x"))); err != ErrReadOnly {
				t.Fatalf("read-only Put = %v; want ErrReadOnly", err)
			}
			if err := ro.Index("nope", wrtKey); err != ErrReadOnly {
				t.Fatalf("read-only Index = %v; want ErrReadOnly", err)
			}
			if err := ro.Close(); err != nil {
				t.Fatal(err)
			}
			// Close is idempotent.
			if err := ro.Close(); err != nil {
				t.Fatalf("second Close = %v; want nil", err)
			}
		})
	}
}

// TestConcurrentReads is the regression guard for the pooled-connection
// design: many goroutines read a shared backend at once (the exact load a
// shared HTTP gateway applies) and every read must return the correct bytes.
// A writer runs in parallel to prove readers are not blocked by concurrent
// writes (sql via WAL, dir via atomic rename into distinct files).
func TestConcurrentReads(t *testing.T) {
	for _, backend := range backendNames() {
		t.Run(string(backend), func(t *testing.T) {
			dir := t.TempDir()
			path := pathFor(backend, dir)
			if backend == StorageHTTP {
				path = newHTTPTestServer(t).URL
			}

			payload := make([]byte, 2<<20)
			if n, err := rand.Read(payload); err != nil || n != len(payload) {
				t.Fatal(err)
			}

			// Seed one big blob keyed by its (properly-sized) multihash and index it.
			key := sha256Multihash(payload)
			w := mustNew(t, backend, path, ReadWrite)
			if err := w.Put(key, int64(len(payload)), bytes.NewReader(payload)); err != nil {
				t.Fatal(err)
			}
			if err := w.Index("big", key); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			ro := mustNew(t, backend, path, ReadOnly)
			defer ro.Close()

			// A writer adds distinct new blobs concurrently with the readers; the
			// readers only ever read the pre-existing `payload` key, so there is no
			// key collision to reason about. Both open the store read-write while
			// `ro` stays read-only: the sql backend does this in WAL mode (readers
			// unblocked by a writer), the dir backend by writing a fresh file per
			// key.
			writer := mustNew(t, backend, path, ReadWrite)
			defer writer.Close()

			const writerBlobs = 8
			const writerSize = 256

			const readers = 16
			const reps = 5
			var wg sync.WaitGroup
			var readFails atomic.Int64

			// The writer is tracked in the same WaitGroup as the readers so its
			// deferred Close (above) can only fire once it has finished writing —
			// otherwise an in-flight Put would race the backend's teardown.
			start := make(chan struct{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < writerBlobs; i++ {
					key := make([]byte, 16)
					rand.Read(key)
					val := make([]byte, writerSize)
					rand.Read(val)
					if err := writer.Put(key, writerSize, bytes.NewReader(val)); err != nil {
						t.Errorf("concurrent Put: %v", err)
					}
				}
			}()

			for i := 0; i < readers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					for r := 0; r < reps; r++ {
						if got, err := readBlob(ro, key); err != nil || !bytes.Equal(got, payload) {
							readFails.Add(1)
						}
						if p, err := ro.Peek(key, 64); err != nil || len(p) == 0 || !bytes.Equal(p, payload[:64]) {
							readFails.Add(1)
						}
						// A name lookup round-trips through the same store.
						if d, err := ro.Lookup("big"); err != nil || !bytes.Equal(d, key) {
							readFails.Add(1)
						}
					}
				}()
			}
			close(start)
			wg.Wait()

			if f := readFails.Load(); f != 0 {
				t.Fatalf("%d of %d concurrent reads were wrong", f, readers*reps)
			}
		})
	}
}

// readBlob streams a blob and returns its full content, closing the reader
// (which returns the backend's pooled connection) on the way out.
func readBlob(b StorageBackend, key Digest) ([]byte, error) {
	r, _, err := b.Get(key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return got, nil
}
