package mount

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"

	"blob/cid"
	"blob/identity"
	"blob/storage"
)

var ctx = context.Background()

// mem is an in-memory storage.StorageBackend for exercising the FUSE tree
// without the kernel FUSE subsystem (which needs /dev/fuse and a privileged
// fusermount). The tree methods are all driven by the backend interface, so a
// fake store stands in for any real read-only backend.
type mem struct {
	blobs map[string][]byte // key (multihash bytes) -> blob
	names map[string]string // name -> key
}

func newMem() *mem {
	return &mem{blobs: map[string][]byte{}, names: map[string]string{}}
}

// store puts b under its content key and (if name != "") records name -> key.
func (m *mem) store(t testing.TB, name string, b []byte) storage.Digest {
	t.Helper()
	id, err := identityOfHash(b)
	if err != nil {
		t.Fatal(err)
	}
	m.blobs[string(id)] = b
	if name != "" {
		m.names[name] = string(id)
	}
	return storage.Digest(id)
}

// identityOfHash returns b's sha2-256 multihash (the store key) — the content
// → key direction every real backend derives.
func identityOfHash(b []byte) (storage.Digest, error) {
	id, err := identity.HashReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return id, nil
}

func (m *mem) Put(_ storage.Digest, _ int64, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	id, err := identityOfHash(b)
	if err != nil {
		return err
	}
	m.blobs[string(id)] = b
	return nil
}

func (m *mem) Get(hash storage.Digest) (io.ReadCloser, storage.Stat, error) {
	b, ok := m.blobs[string(hash)]
	if !ok {
		return nil, storage.Stat{}, syscall.ENOENT
	}
	return io.NopCloser(bytes.NewReader(b)), storage.Stat{Size: int64(len(b))}, nil
}

func (m *mem) Stat(hash storage.Digest) (storage.Stat, error) {
	b, ok := m.blobs[string(hash)]
	if !ok {
		return storage.Stat{}, syscall.ENOENT
	}
	return storage.Stat{Size: int64(len(b))}, nil
}

func (m *mem) Has(hash storage.Digest) (bool, error) {
	_, ok := m.blobs[string(hash)]
	return ok, nil
}

func (m *mem) Index(name string, hash storage.Digest) error {
	m.names[name] = string(hash)
	return nil
}

func (m *mem) Lookup(name string) (storage.Digest, error) {
	k, ok := m.names[name]
	if !ok {
		return nil, syscall.ENOENT
	}
	return storage.Digest(k), nil
}

func (m *mem) Peek(hash storage.Digest, n int) ([]byte, error) {
	b, ok := m.blobs[string(hash)]
	if !ok {
		return nil, syscall.ENOENT
	}
	if n < len(b) {
		b = b[:n]
	}
	return b, nil
}

func (m *mem) Names() ([]string, error) {
	names := make([]string, 0, len(m.names))
	for n := range m.names {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

func (m *mem) Close() error { return nil }

var _ storage.StorageBackend = (*mem)(nil)

// TestRootShape checks the fixed skeleton: the root lists exactly name/, blob/
// and oci/ (all directories), each reachable by Lookup, unknown entries ENOENT.
func TestRootShape(t *testing.T) {
	f := New(newMem())
	root, err := f.Root()
	if err != nil {
		t.Fatal(err)
	}
	r := root.(*rootNode)

	dirs, err := r.ReadDirAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range dirs {
		names = append(names, d.Name)
		if d.Type != fuse.DT_Dir {
			t.Fatalf("top-level %q type = %v, want DT_Dir", d.Name, d.Type)
		}
	}
	if len(names) != 3 || names[0] != "name" || names[1] != "blob" || names[2] != "oci" {
		t.Fatalf("top-level = %v, want [name blob oci]", names)
	}

	nd, err := r.Lookup(ctx, "name")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := nd.(*nameDir); !ok {
		t.Fatalf("Lookup(name) = %T, want *nameDir", nd)
	}
	bdn, err := r.Lookup(ctx, "blob")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bdn.(*blobDir); !ok {
		t.Fatalf("Lookup(blob) = %T, want *blobDir", bdn)
	}
	odn, err := r.Lookup(ctx, "oci")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := odn.(*ociTree); !ok {
		t.Fatalf("Lookup(oci) = %T, want *ociTree", odn)
	}
	if _, err := r.Lookup(ctx, "nope"); !isEnoent(err) {
		t.Fatalf("Lookup(nope) err = %v, want ENOENT", err)
	}
}

// TestNameSurface checks name/: it lists the recorded index names (all DT_Link),
// each Lookup returns a symlink whose target is ../blob/<derived CID> (the
// CID asserted via the store's own codec rule, independent of the mount path),
// and an unknown name is ENOENT. OCI origin references that the index also holds
// are NOT listed under name/ — they live under the dedicated oci/ tree only.
func TestNameSurface(t *testing.T) {
	m := newMem()
	jsonPayload := []byte(`{"hello":"world"}`)        // '{' -> Json
	rawPayload := []byte("plain bytes, no json head") // 'p' -> Raw
	jk := m.store(t, "j.json", jsonPayload)
	rk := m.store(t, "r.bin", rawPayload)
	// An OCI origin reference is also recorded in the index; it must not
	// surface under name/ (ls would otherwise show it as one opaque
	// slash-bearing entry), but it is still reachable via the oci/ tree.
	ociPayload := []byte("sif-ish bytes for the origin ref")
	ok_ := m.store(t, "", ociPayload)
	ociName := "oci/blob/linux/amd64/sha256/3f4d6b32e26703c6"
	if err := m.Index(ociName, ok_); err != nil {
		t.Fatal(err)
	}
	wantJ := wantCID(t, jsonPayload, jk)
	wantR := wantCID(t, rawPayload, rk)

	f := New(m)
	root, _ := f.Root()
	nameNode, err := root.(*rootNode).Lookup(ctx, "name")
	if err != nil {
		t.Fatal(err)
	}
	ndir := nameNode.(*nameDir)

	list, err := ndir.ReadDirAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Only the two ordinary names are listed — the OCI reference is not.
	if len(list) != 2 {
		t.Fatalf("name/ listing = %d entries, want 2 (oci/... entries hidden): %v", len(list), list)
	}
	for _, d := range list {
		if d.Type != fuse.DT_Link {
			t.Fatalf("name/%s type = %v, want DT_Link", d.Name, d.Type)
		}
		if d.Name == ociName {
			t.Fatalf("name/ listing contained OCI reference %q; it should be filtered (use oci/)", d.Name)
		}
	}

	for _, tc := range []struct{ name, target string }{
		{"j.json", "../blob/" + wantJ},
		{"r.bin", "../blob/" + wantR},
	} {
		node, err := ndir.Lookup(ctx, tc.name)
		if err != nil {
			t.Fatalf("Lookup(name/%s) err = %v", tc.name, err)
		}
		link, ok := node.(*nameLink)
		if !ok {
			t.Fatalf("Lookup(name/%s) = %T, want *nameLink", tc.name, node)
		}
		got, err := link.Readlink(ctx, &fuse.ReadlinkRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.target {
			t.Fatalf("readlink name/%s = %q, want %q", tc.name, got, tc.target)
		}
	}

	if _, err := ndir.Lookup(ctx, "no/such/name"); !isEnoent(err) {
		t.Fatalf("Lookup(name/no-such) err = %v, want ENOENT", err)
	}
}

// TestOciSurfaceListing checks that each level of the oci/ tree lists exactly
// the recorded values at that depth (and no more), so the top-level tree shows
// the variants recorded, the second level per variant shows the OS, etc.
func TestOciSurfaceListing(t *testing.T) {
	_, ot, keys := ociSurfaceSetup(t)
	const idx = "3f4d6b32e26703c6"
	_ = keys
	type lvl struct {
		node *ociTree
		name string
		want []string
	}
	// Walk the full blob branch and assert each level's listing.
	bt := ociChild(t, ot, "blob")
	lt := ociChild(t, bt, "linux")
	amt := ociChild(t, lt, "amd64")
	sht := ociChild(t, amt, "sha256")
	cases := []lvl{
		{ot, "oci/", []string{"blob", "json"}},
		{bt, "oci/blob/", []string{"linux"}},
		{lt, "oci/blob/linux/", []string{"amd64", "arm64"}},
		{amt, "oci/blob/linux/amd64/", []string{"sha256"}},
		{sht, "oci/blob/linux/amd64/sha256/", []string{idx}},
	}
	for _, c := range cases {
		list, err := c.node.ReadDirAll(ctx)
		if err != nil {
			t.Fatalf("%s: ReadDirAll: %v", c.name, err)
		}
		var got []string
		for _, e := range list {
			got = append(got, e.Name)
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: listing = %v, want %v", c.name, got, c.want)
			continue
		}
		for i, w := range c.want {
			if got[i] != w {
				t.Errorf("%s: got[%d]=%q, want %q (full listing %v)", c.name, i, got[i], w, got)
			}
		}
	}

	// The hex leaf is a DT_Link; the rest are DT_Dir.
	hexList, _ := sht.ReadDirAll(ctx)
	if len(hexList) == 1 && hexList[0].Type != fuse.DT_Link {
		t.Errorf("hex leaf type = %v, want DT_Link", hexList[0].Type)
	}

	// Unknown leaf: not recorded → ENOENT.
	if _, err := sht.Lookup(ctx, "XYZXZYX"); !isEnoent(err) {
		t.Errorf("Lookup(leaf XYZXZYX): err = %v, want ENOENT", err)
	}
}

// TestOciSurfaceLeaves checks that the leaf of each recorded reference, walked
// through the tree, is a symlink whose *target* — when used as a real symlink
// in a layout mirroring the mount — actually resolves to the correct
// blob/<CID>. This is the authoritative test for the "../" depth: a wrong
// count would make EvalSymlinks fail or land on the wrong file.
func TestOciSurfaceLeaves(t *testing.T) {
	m, ot, keys := ociSurfaceSetup(t)
	const idx = "3f4d6b32e26703c6"
	// Each case: path below the oci/ root (variant, os, arch, algo, hex) and
	// the CID the leaf must name (derived via the standard codec rule from the
	// stored payload + key in the mem — the same wantCID(t, ...) baseline).
	type tc struct {
		path []string
		want string
	}
	cases := []tc{
		{[]string{"blob", "linux", "amd64", "sha256", idx}, wantCID(t, keys.amdBlob, keys.mkBlob)},
		{[]string{"blob", "linux", "arm64", "sha256", idx}, wantCID(t, keys.armBlob, keys.mkArm)},
		{[]string{"json", "linux", "amd64", "sha256", idx}, wantCID(t, keys.amdIdx, keys.mkIdx)},
	}
	_ = m
	for _, c := range cases {
		// Walk to the leaf.
		node := ot
		for _, p := range c.path[:len(c.path)-1] {
			node = ociChild(t, node, p)
		}
		leaf := ociLeaf(t, node, c.path[len(c.path)-1])
		target, err := leaf.Readlink(ctx, &fuse.ReadlinkRequest{})
		if err != nil {
			t.Fatalf("Readlink(%v): %v", c.path, err)
		}
		// Lay out root/oci/<variant>/<os>/<arch>/<algo>/<hex> as a real dir
		// and root/blob/<CID> as a real file, then check the target resolves.
		ociLeafResolves(t, c.path, target, c.want)
	}
}

// ociSurfaceSetup returns a mem store pre-populated with the three standard
// reference names (blob/amd64, blob/arm64, json/amd64, all under the same
// origin digest), plus the top-level ociTree and the keys/payloads for the
// three stored blobs (so tests can derive the expected CIDs).
func ociSurfaceSetup(t *testing.T) (*mem, *ociTree, struct {
	amdBlob, armBlob, amdIdx []byte
	mkBlob, mkArm, mkIdx     storage.Digest
}) {
	t.Helper()
	m := newMem()
	const idx = "3f4d6b32e26703c6"
	mkBlob := m.store(t, "alpine-amd.sif", ociSetupPayloadAmdBlob)
	mkArm := m.store(t, "alpine-arm.sif", ociSetupPayloadArmBlob)
	mkIdx := m.store(t, "alpine-amd-oci-index.json", ociSetupPayloadAmdIdx)
	for _, pair := range []struct {
		nm   string
		dgst storage.Digest
	}{
		{"oci/blob/linux/amd64/sha256/" + idx, mkBlob},
		{"oci/blob/linux/arm64/sha256/" + idx, mkArm},
		{"oci/json/linux/amd64/sha256/" + idx, mkIdx},
	} {
		if err := m.Index(pair.nm, pair.dgst); err != nil {
			t.Fatalf("Index(%q): %v", pair.nm, err)
		}
	}
	f := New(m)
	root, err := f.Root()
	if err != nil {
		t.Fatal(err)
	}
	ociNode, err := root.(*rootNode).Lookup(ctx, "oci")
	if err != nil {
		t.Fatal(err)
	}
	keys := struct {
		amdBlob, armBlob, amdIdx []byte
		mkBlob, mkArm, mkIdx     storage.Digest
	}{
		ociSetupPayloadAmdBlob, ociSetupPayloadArmBlob, ociSetupPayloadAmdIdx,
		mkBlob, mkArm, mkIdx,
	}
	return m, ociNode.(*ociTree), keys
}

// ociChild advances into the next level of the tree (a directory).
func ociChild(t *testing.T, n *ociTree, name string) *ociTree {
	t.Helper()
	child, err := n.Lookup(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := child.(*ociTree)
	if !ok {
		t.Fatalf("child(%s) = %T, want *ociTree", name, child)
	}
	return c
}

// ociLeaf advances into a leaf (a symlink).
func ociLeaf(t *testing.T, n *ociTree, name string) *nameLink {
	t.Helper()
	child, err := n.Lookup(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := child.(*nameLink)
	if !ok {
		t.Fatalf("leaf(%s) = %T, want *nameLink", name, child)
	}
	return l
}

// ociLeafResolves lays out root/oci/<path parts...>/<leaf> and root/blob/<cid>
// as real filesystem entries, then checks that target (a relative symlink
// target) used at that leaf resolves to the expected blob. A wrong "../"
// depth would fail EvalSymlinks or land on the wrong file.
func ociLeafResolves(t *testing.T, belowOci []string, target, cid string) {
	t.Helper()
	root := t.TempDir()
	var linkPath []string
	linkPath = append(linkPath, "oci")
	linkPath = append(linkPath, belowOci...)
	link := filepath.Join(append([]string{root}, linkPath...)...)
	want := filepath.Join(root, "blob", cid)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("eval %q (target %q): %v", link, target, err)
	}
	if got != want {
		t.Fatalf("got %q want %q (target %q)", got, want, target)
	}
}

// The three payloads used by ociSurfaceSetup: the amd64 SIF, the arm64 SIF,
// and the OCI image index (the same bytes as `add` would store for a SIF, the
// arm64 twin, and a valid OCI index — each a distinct CAS entry).
var (
	ociSetupPayloadAmdBlob = []byte("alpine-amd64-bytes")
	ociSetupPayloadArmBlob = []byte("alpine-arm64-bytes")
	ociSetupPayloadAmdIdx  = []byte(`{"schemaVersion":2}`)
)

// TestBlobSurface checks blob/: a stored CID is a stat-able regular file that
// streams the exact stored bytes back (page by page, forward-only); unknown
// and foreign-codec CIDs are ENOENT; and the directory is not enumerable.
func TestBlobSurface(t *testing.T) {
	m := newMem()
	payload := bytes.Repeat([]byte("payload-byte-0123456789"), 262) // ~90 KiB
	key := m.store(t, "plain.bin", payload)

	c := cid.Build(cid.CodeRaw, key)
	cidStr, err := cid.Render(c)
	if err != nil {
		t.Fatal(err)
	}
	foreign := cid.Build(cid.CodeDagPb, key)
	foreignStr, err := cid.Render(foreign)
	if err != nil {
		t.Fatal(err)
	}

	f := New(m)
	root, _ := f.Root()
	blobDirNode, err := root.(*rootNode).Lookup(ctx, "blob")
	if err != nil {
		t.Fatal(err)
	}
	bd := blobDirNode.(*blobDir)

	list, err := bd.ReadDirAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("blob/ listing = %d entries, want 0 (not enumerable)", len(list))
	}

	fileNode, err := bd.Lookup(ctx, cidStr)
	if err != nil {
		t.Fatalf("Lookup(blob/%s) err = %v", cidStr, err)
	}

	// Attr: mode 0444 and correct size.
	var attr fuse.Attr
	if err := fileNode.Attr(ctx, &attr); err != nil {
		t.Fatal(err)
	}
	if attr.Mode&0o7777 != 0o444 {
		t.Fatalf("blob/<CID> mode = %#o, want 0o444", attr.Mode&0o7777)
	}
	if attr.Size != uint64(len(payload)) {
		t.Fatalf("blob/<CID> size = %d, want %d", attr.Size, len(payload))
	}

	// Open + page-wise forward Reads; the assembled bytes must be exact.
	hand, err := fileNode.(*blobFile).Open(ctx, &fuse.OpenRequest{Flags: 0o0}, &fuse.OpenResponse{})
	if err != nil {
		t.Fatal(err)
	}
	h := hand.(*blobHandle)
	defer tearDownHandle(t, h)

	const page = 4096
	var out []byte
	for off := int64(0); off < int64(len(payload)); off += page {
		size := page
		if rem := int64(len(payload)) - off; rem < int64(page) {
			size = int(rem)
		}
		resp := &fuse.ReadResponse{}
		req := &fuse.ReadRequest{Offset: off, Size: size}
		if err := h.Read(ctx, req, resp); err != nil {
			t.Fatalf("Read(off=%d, size=%d) err = %v", off, size, err)
		}
		out = append(out, resp.Data...)
	}
	if !bytes.Equal(out, payload) {
		t.Fatalf("reassembled %d bytes != stored %d bytes", len(out), len(payload))
	}

	// Past-the-end read: empty, not an error.
	resp := &fuse.ReadResponse{}
	if err := h.Read(ctx, &fuse.ReadRequest{Offset: int64(len(payload)), Size: page}, resp); err != nil {
		t.Fatalf("Read(off=end) err = %v, want nil", err)
	}
	if len(resp.Data) != 0 {
		t.Fatalf("Read(off=end) -> %d bytes, want 0", len(resp.Data))
	}

	if _, err := bd.Lookup(ctx, foreignStr); !isEnoent(err) {
		t.Fatalf("Lookup(blob/foreign-codec) err = %v, want ENOENT", err)
	}
	if _, err := bd.Lookup(ctx, "not-a-cid"); !isEnoent(err) {
		t.Fatalf("Lookup(blob/garbage) err = %v, want ENOENT", err)
	}

	// OCI digest form: sha256:<hex> of a stored payload resolves; the same
	// hex without the "sha256:" prefix does not; an unknown "sha256:<hex>"
	// does not.
	id, err := identity.HashReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	d, err := identity.ToOCI(id)
	if err != nil {
		t.Fatal(err)
	}
	dnode, err := bd.Lookup(ctx, d.String())
	if err != nil {
		t.Fatalf("Lookup(blob/%s) err = %v, want the blob", d, err)
	}
	if df, ok := dnode.(*blobFile); !ok || !bytes.Equal(df.key, key) {
		t.Fatalf("Lookup(blob/%s) = %T key %x, want *blobFile key %x", d, dnode, dnode, key)
	}
	if _, err := bd.Lookup(ctx, d.Encoded()); !isEnoent(err) {
		t.Fatalf("Lookup(blob/%s) (bare hex) err = %v, want ENOENT", d.Encoded(), err)
	}
	unknownHex := "0000000000000000ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if _, err := bd.Lookup(ctx, "sha256:"+unknownHex); !isEnoent(err) {
		t.Fatalf("Lookup(blob/sha256:%s) err = %v, want ENOENT", unknownHex, err)
	}
}

// TestBackwardReadRefused checks the forward-only contract: after a forward
// advance, a request for an earlier offset is refused (EIO) rather than
// serving wrong bytes.
func TestBackwardReadRefused(t *testing.T) {
	m := newMem()
	payload := bytes.Repeat([]byte("y"), 8192)
	key := m.store(t, "b.bin", payload)
	c := cid.Build(cid.CodeRaw, key)
	cidStr, _ := cid.Render(c)

	f := New(m)
	fileNode := blobLookup(t, f, cidStr)
	hand, err := fileNode.(*blobFile).Open(ctx, &fuse.OpenRequest{Flags: 0o0}, &fuse.OpenResponse{})
	if err != nil {
		t.Fatal(err)
	}
	h := hand.(*blobHandle)
	defer tearDownHandle(t, h)

	// Read forward one page first.
	if err := h.Read(ctx, &fuse.ReadRequest{Offset: 0, Size: 4096}, &fuse.ReadResponse{}); err != nil {
		t.Fatal(err)
	}
	// Now ask for an earlier offset: must be refused, not answer with bytes.
	err = h.Read(ctx, &fuse.ReadRequest{Offset: 0, Size: 4096}, &fuse.ReadResponse{})
	if err == nil {
		t.Fatal("backward read after forward: err = nil, want EIO")
	}
	if err != syscall.EIO {
		t.Fatalf("backward read: err = %v, want EIO", err)
	}
}

// tearDownHandle releases the handle the way the kernel does on close: a
// Flush (per-fd, must not tear the reader down) followed by a Release
// (last fd, which does). A test that reads after this is invalid.
func tearDownHandle(t *testing.T, h *blobHandle) {
	t.Helper()
	if err := h.Flush(ctx, &fuse.FlushRequest{}); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := h.Release(ctx, &fuse.ReleaseRequest{}); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestFlushIsNotTeardown captures the dd-vs-cat race: the kernel sends a
// Flush on each close(2) of a fd that refers to the handle, but there can be
// several fds per handle, so a Flush must NOT close the backend reader. The
// reader is closed exactly once, on Release. If a test did Flush→Read and
// the first read was a nil dereference, that's the bug being guarded.
func TestFlushIsNotTeardown(t *testing.T) {
	m := newMem()
	payload := bytes.Repeat([]byte("x"), 8192) // two 4 KiB pages, so a post-Flush read has bytes to serve
	key := m.store(t, "f.bin", payload)
	c := cid.Build(cid.CodeRaw, key)
	cidStr, _ := cid.Render(c)

	f := New(m)
	fileNode := blobLookup(t, f, cidStr)
	hand, _ := fileNode.(*blobFile).Open(ctx, &fuse.OpenRequest{Flags: 0o0}, &fuse.OpenResponse{})
	h := hand.(*blobHandle)
	defer tearDownHandle(t, h)

	// Read page 0, then Flush (the mid-open fd close dd does), then read
	// page 1 — the reader must still be open after the Flush.
	if err := h.Read(ctx, &fuse.ReadRequest{Offset: 0, Size: 4096}, &fuse.ReadResponse{}); err != nil {
		t.Fatalf("Read(off=0): %v", err)
	}
	if err := h.Flush(ctx, &fuse.FlushRequest{}); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// A post-Flush forward read must not EIO on a nil reader (the dd bug);
	// it should return the next page.
	resp := &fuse.ReadResponse{}
	if err := h.Read(ctx, &fuse.ReadRequest{Offset: 4096, Size: 100}, resp); err != nil {
		t.Fatalf("Read after Flush: %v; want the reader to still be open (Release is teardown, not Flush)", err)
	}
	if len(resp.Data) != 100 {
		t.Fatalf("Read after Flush: %d bytes, want 100", len(resp.Data))
	}
	if !bytes.Equal(resp.Data, payload[4096:4196]) {
		t.Fatalf("Read after Flush: wrong bytes — reader was clobbered mid-open")
	}
}

// TestReadAfterReleaseIsRefused verifies the other half of the race: after a
// Release (the FUSE "open over" hook), a spurious in-flight Read is answered
// with EIO rather than a nil-dereference panic.
func TestReadAfterReleaseIsRefused(t *testing.T) {
	m := newMem()
	payload := bytes.Repeat([]byte("z"), 4096)
	key := m.store(t, "r.bin", payload)
	c := cid.Build(cid.CodeRaw, key)
	cidStr, _ := cid.Render(c)

	f := New(m)
	fileNode := blobLookup(t, f, cidStr)
	hand, _ := fileNode.(*blobFile).Open(ctx, &fuse.OpenRequest{Flags: 0o0}, &fuse.OpenResponse{})
	h := hand.(*blobHandle)

	if err := h.Release(ctx, &fuse.ReleaseRequest{}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	err := h.Read(ctx, &fuse.ReadRequest{Offset: 0, Size: 4096}, &fuse.ReadResponse{})
	if err != syscall.EIO {
		t.Fatalf("Read after Release: err = %v, want EIO", err)
	}
}

// wantCID derives the store's CID the same way the real store does (content-
// based codec + key), independent of the mount's path — the assertion baseline.
func wantCID(t *testing.T, payload []byte, key storage.Digest) string {
	t.Helper()
	var first byte
	if len(payload) > 0 {
		first = payload[0]
	}
	s, err := cid.Render(cid.Build(cid.CodecForFirstByte(first), key))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// blobLookup returns the *blobFile node for a stored CID — the common
// f.Root → Lookup("blob") → Lookup(cidStr) trip, so the read-handle tests are
// not each repeating the same plumbing.
func blobLookup(t *testing.T, f *FS, cidStr string) fs.Node {
	t.Helper()
	root, err := f.Root()
	if err != nil {
		t.Fatal(err)
	}
	blobNode, err := root.(*rootNode).Lookup(ctx, "blob")
	if err != nil {
		t.Fatal(err)
	}
	node, err := blobNode.(*blobDir).Lookup(ctx, cidStr)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func isEnoent(err error) bool {
	return err == syscall.ENOENT || fuse.ToErrno(err) == fuse.Errno(syscall.ENOENT)
}
