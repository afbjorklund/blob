package mount

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"bazil.org/fuse"

	"blob/cid"
	"blob/storage"
)

// TestMountSmoke is the opt-in, end-to-end, "real" counterpart of the
// in-memory tree tests: a real dir backend, a real FUSE connection, and the
// mount probed with the standard POSIX tooling (ls, stat, cat), the way a
// shell user would exercise the two mount points.
//
//	BLOB_MOUNT_SMOKE=1 go test -v -run TestMountSmoke ./mount
//
// It needs /dev/fuse plus a setuid fusermount(3) helper (checked at test time
// so the skip is legible in a sandbox that lacks either).
func TestMountSmoke(t *testing.T) { //nolint:gocyclo
	if os.Getenv("BLOB_MOUNT_SMOKE") == "" {
		t.Skip("set BLOB_MOUNT_SMOKE=1 to run the fuse smoke test (needs /dev/fuse and fusermount)")
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("no /dev/fuse: %v (needs a host with the kernel FUSE module loaded)", err)
	}
	if _, err := exec.LookPath("fusermount"); err != nil {
		t.Skipf("no fusermount(3) on PATH: %v", err)
	}

	// Seed a real dir backend with two blobs under two names (one JSON-head,
	// one plain) — the minimum shape the two namespaces need.
	storeDir := t.TempDir()
	be, err := storage.NewStorage(storage.StorageDir, storeDir, "", "", storage.Auth{}, storage.ReadWrite)
	if err != nil {
		t.Fatalf("open dir backend: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	jsonPayload := []byte(`{"smoke":"json"}`)
	rawPayload := []byte("smoke raw content 1234567890")
	jkey, _ := identityOfHash(jsonPayload)
	rkey, _ := identityOfHash(rawPayload)
	for _, pair := range []struct {
		name string
		key  storage.Digest
		b    []byte
	}{
		{"smoke.json", jkey, jsonPayload},
		{"smoke.txt", rkey, rawPayload},
	} {
		if err := be.Put(pair.key, int64(len(pair.b)), bytes.NewReader(pair.b)); err != nil {
			t.Fatal(err)
		}
		if err := be.Index(pair.name, pair.key); err != nil {
			t.Fatal(err)
		}
	}

	// The CIDs the store prints (`add` would derive the same, from first byte).
	cJSON, err := cid.Render(cid.Build(cid.CodeJson, jkey))
	if err != nil {
		t.Fatal(err)
	}
	cRaw, err := cid.Render(cid.Build(cid.CodeRaw, rkey))
	if err != nil {
		t.Fatal(err)
	}

	// Mount an existing empty directory; clean up unmount + close for sure, so
	// a test failure does not strand a FUSE mount.
	mountPoint := t.TempDir()
	fsys := New(be)
	conn, merr := fsys.Mount(mountPoint)
	if merr != nil {
		t.Skipf("fuse.Mount(%s) failed: %v (kernel may need the fusermount setuid helper or the FUSE module)", mountPoint, merr)
	}
	unmounted := false
	t.Cleanup(func() {
		if !unmounted {
			_ = fuse.Unmount(mountPoint)
			unmounted = true
		}
		_ = conn.Close()
	})

	// Serve until the conn is torn down; run it in a goroutine so we can poll
	// readiness from the test's main goroutine.
	done := make(chan error, 1)
	go func() { done <- fsys.Serve(conn) }()

	// Readiness: the first time `ls name/` lists both recorded names the
	// kernel has completed INIT and a lookup round-trip (10s is generous for
	// a cold mount point).
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := exec.Command("ls", mountPoint+"/name").CombinedOutput()
		if err == nil && strings.Contains(string(out), "smoke.json") && strings.Contains(string(out), "smoke.txt") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mount did not become ready within 10s (last: out=%q err=%v)", out, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// --- the core contract: read by CID and by name (the symlink), both exact ---

	t.Run("read-by-CID", func(t *testing.T) {
		pairs := []struct {
			name, cid string
			want      []byte
		}{
			{"json", cJSON, jsonPayload},
			{"raw", cRaw, rawPayload},
		}
		for _, p := range pairs {
			out, err := exec.Command("dd", "if="+mountPoint+"/blob/"+p.cid, "bs=4096", "count=8", "oflag=nonblock").Output()
			if err != nil {
				t.Fatalf("dd blob/%s: %v", p.cid, err)
			}
			if !bytes.Equal(out, p.want) {
				t.Fatalf("dd blob/%s = %q, want %q", p.cid, out, p.want)
			}
		}
	})

	t.Run("read-by-name-follows-symlink", func(t *testing.T) {
		out, err := exec.Command("cat", mountPoint+"/name/smoke.json").Output()
		if err != nil {
			t.Fatalf("cat name/smoke.json: %v", err)
		}
		if !bytes.Equal(out, jsonPayload) {
			t.Fatalf("cat name/smoke.json = %q, want %q", out, jsonPayload)
		}
		out2, err := exec.Command("cat", mountPoint+"/name/smoke.txt").Output()
		if err != nil {
			t.Fatalf("cat name/smoke.txt: %v", err)
		}
		if !bytes.Equal(out2, rawPayload) {
			t.Fatalf("cat name/smoke.txt = %q, want %q", out2, rawPayload)
		}
	})

	t.Run("stat-CID-size-matches", func(t *testing.T) {
		out, err := exec.Command("stat", "-c", "%s", mountPoint+"/blob/"+cJSON).Output()
		if err != nil {
			t.Fatalf("stat blob: %v", err)
		}
		size, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if perr != nil {
			t.Fatalf("stat size %q not a number: %v", out, perr)
		}
		if size != int64(len(jsonPayload)) {
			t.Fatalf("stat size = %d, want %d", size, len(jsonPayload))
		}
	})

	t.Run("write-is-refused", func(t *testing.T) {
		_, err := exec.Command("touch", mountPoint+"/name/smoke.json.new").CombinedOutput()
		if err == nil {
			t.Fatal("touch succeeded on a read-only mount; want a refusal")
		}
	})

	t.Run("ls-blob-is-empty", func(t *testing.T) {
		out, err := exec.Command("ls", mountPoint+"/blob").CombinedOutput()
		if err != nil {
			t.Fatalf("ls blob/: %v", err)
		}
		if len(bytes.TrimSpace(out)) != 0 {
			t.Fatalf("ls blob/ = %q, want empty (the CAS is not enumerable)", out)
		}
	})

	// Let the serve goroutine observe the EOF its cleanup triggers — a
	// bounded wait (the goroutine is a test-only concern; a hard hang is not).
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// The kernel teardown is in progress; nothing more we can do from
		// the test once the connection is closing.
	}
}
