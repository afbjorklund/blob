// Package mount exposes the store's read surface as a regular file system on
// a local mount point — the filesystem analogue of daemon (which exposes the
// same two namespaces over HTTP rather than POSIX). Both take a
// storage.StorageBackend and present name + blob, but pick the encoding the
// transport prefers (HTTP/JSON for daemon, directory entries and symlinks
// for mount).
//
// The mount presents the backend read-only. The root holds two directories:
//
//	name/  - one symbolic link per recorded index name, linking to the
//	         content's canonical blob path "../blob/<CID>". A FUSE symlink is
//	         just a (name, target) pair, so advertising an entry touches no
//	         bytes of the content: `ls name/` does a single Names() call, and
//	         `stat name/<n>` / `readlink` do a single Lookup plus a single
//	         Peek for the first-byte codec rule (the same name→CID daemon
//	         derives for /name/<name>).
//	blob/  - one regular file per stored blob, addressed by rendered CID.
//	         The directory is deliberately NOT enumerable (ReadDirAll returns
//	         nothing): the StorageBackend interface exposes no "list all
//	         blobs" method (Names() is the index, not the CAS), and that is
//	         the right shape — the CAS is addressable, not listable, exactly
//	         as daemon serves /blob/<CID> and the /v2/blob/<CID> OCI surface
//	         without ever enumerating the store.
//
// Content reads are **streaming, forward-only, page-bounded** — the kernel
// page cache does the re-read/caching the client would expect (it is FUSE's
// job, not ours), and our Handle returns the kernel's requested page at a
// time from a single forward reader, so a multi-GB blob never materialises
// whole on the mount side. Backward / random reads are not served by this
// handle (FUSE pages that were already read back, the way the HTTP gateway
// serves a `Range` only for a fresh GET).
//
// Mounting (fuse.Mount) and the FUSE protocol loop (fs.Serve) are the
// transport plumbing; everything FUSE-specific (the tree, the two top
// directories, the symlink target form, the streaming read) lives here.
// Only the read path is implemented: a write attempt is refused by the
// mount option ReadOnly() and, independently, by the backend
// (a storage.ReadOnly), so the read-only contract is doubly enforced.
//
// Mount is supported on Linux only (the FUSE subsystem there); on other
// platforms fuse.Mount returns an error and the mount command reports it.
package mount

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"

	"blob/cid"
	"blob/index"
	"blob/storage"
)

// FS is the FUSE file system this package serves. It is a single shared,
// read-only storage backend plus the fixed two-directory tree (name/, blob/)
// that presents it. All backend use goes through the backend's own
// concurrency story (pooled sql, per-op local files, serialised http/s3 — see
// the storage.StorageBackend docs), so a single FS value is safe to hand to
// fs.Serve, which may invoke its handler methods concurrently.
type FS struct {
	backend storage.StorageBackend
}

// New builds an FS over backend. backend should be opened storage.ReadOnly
// (mount only reads); the caller keeps and Closes it.
func New(backend storage.StorageBackend) *FS {
	return &FS{backend: backend}
}

// Root returns the root node the FUSE tree is served from: a directory whose
// only children are name/ (the index) and blob/ (the CAS).
func (f *FS) Root() (fs.Node, error) {
	return &rootNode{fs: f}, nil
}

// Mount opens a FUSE connection at dir, ready to serve this store. The caller
// must fs.Serve the returned *fuse.Conn (see Serve) and, when done, Close()
// it — closing the connection also tears down the kernel-side mount. dir must
// be an existing empty directory (FUSE mounts into an existing one).
func (f *FS) Mount(dir string) (*fuse.Conn, error) {
	return fuse.Mount(dir,
		fuse.ReadOnly(),
		fuse.FSName("blob"),
		fuse.Subtype("blobfs"),
	)
}

// Serve runs the FUSE protocol loop on conn until it returns. It blocks for
// the life of the mount; it returns when the connection terminates — either a
// fatal error, or a clean shutdown (a signal interrupts the process, the
// kernel closes /dev/fuse, fs.Serve sees EOF and returns nil). The caller
// owns conn's lifetime (Close it after Serve returns), mirroring
// daemon.Server.ListenAndServe handing back to the CLI.
func (f *FS) Serve(conn *fuse.Conn) error {
	return fs.Serve(conn, f)
}

// ServeContext is Serve with an explicit context: it runs fs.Serve in a
// goroutine and returns as soon as ctx is canceled (canceling also triggers
// the kernel teardown path — the FUSE connection is torn down when the
// process closes or the /dev/fuse fd drops, either of which is how the CLI's
// signal handler ends the mount). The returned error is fs.Serve's, if any.
// It does NOT close conn; the caller owns that.
func (f *FS) ServeContext(ctx context.Context, conn *fuse.Conn) error {
	done := make(chan error, 1)
	go func() {
		done <- fs.Serve(conn, f)
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		return err
	}
}

var (
	_ fs.FS             = (*FS)(nil)
	_ fs.Node           = (*rootNode)(nil)
	_ fs.Node           = (*nameDir)(nil)
	_ fs.Node           = (*blobDir)(nil)
	_ fs.Node           = (*nameLink)(nil)
	_ fs.Node           = (*blobFile)(nil)
	_ fs.Node           = (*ociTree)(nil)
	_ fs.NodeOpener     = (*blobFile)(nil)
	_ fs.Handle         = (*blobHandle)(nil)
	_ fs.HandleReader   = (*blobHandle)(nil)
	_ fs.HandleFlusher  = (*blobHandle)(nil)
	_ fs.HandleReleaser = (*blobHandle)(nil)
)

// rootNode is the filesystem root. Its only children are the two top
// directories: name/ (the name index, listed from Names()) and blob/ (the
// content CAS, addressable by CID, not listable).
type rootNode struct{ fs *FS }

func (n *rootNode) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (n *rootNode) ReadDirAll(_ context.Context) ([]fuse.Dirent, error) {
	return []fuse.Dirent{
		{Name: "name", Type: fuse.DT_Dir},
		{Name: "blob", Type: fuse.DT_Dir},
		{Name: "oci", Type: fuse.DT_Dir},
	}, nil
}

func (n *rootNode) Lookup(_ context.Context, name string) (fs.Node, error) {
	switch name {
	case "name":
		return &nameDir{fs: n.fs}, nil
	case "blob":
		return &blobDir{fs: n.fs}, nil
	case "oci":
		return &ociTree{fs: n.fs, level: 0}, nil
	default:
		return nil, syscall.ENOENT
	}
}

// nameDir is the name/ directory: one symbolic link per recorded index entry,
// linking to the blob's canonical CID path under blob/. It mirrors the daemon
// gateway's /name/<name> — the entry resolves to the same CID — but as a
// POSIX symlink instead of a 302 redirect (a FUSE symlink and an HTTP 302 are
// the same "name → canonical blob" pointer).
type nameDir struct{ fs *FS }

func (n *nameDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

// ReadDirAll lists the index (backend.Names) — the only enumeration this
// store exposes (the index, not the CAS). The result is already sorted by the
// backends (their contract: lexicographic clean-name order).
func (n *nameDir) ReadDirAll(_ context.Context) ([]fuse.Dirent, error) {
	names, err := n.fs.backend.Names()
	if err != nil {
		return nil, err
	}
	out := make([]fuse.Dirent, 0, len(names))
	for _, name := range names {
		out = append(out, fuse.Dirent{Name: name, Type: fuse.DT_Link})
	}
	return out, nil
}

// Lookup resolves one name to a nameLink (the symlink). It is cheap: one
// backend.Lookup for the key, one backend.Peek for the first-byte codec
// (the daemon name→CID rule), and a render. No bytes of the content are
// touched, so a name may point at a multi-GB blob and its entry still stats
// and lists in constant time.
func (n *nameDir) Lookup(_ context.Context, name string) (fs.Node, error) {
	be := n.fs.backend
	key, err := be.Lookup(name)
	if err != nil {
		return nil, syscall.ENOENT
	}

	// Derive the CID the same way daemon /name/<name> and `blob lookup` do:
	// the content-based codec rule (Json iff the first byte opens a JSON
	// value, else Raw — cid.CodecForFirstByte), so the entry links to the
	// exact CID `add` printed for the content.
	var first byte
	hdr, perr := be.Peek(key, 1)
	if perr != nil && !errors.Is(perr, io.EOF) {
		return nil, perr
	}
	if len(hdr) > 0 {
		first = hdr[0]
	}

	c := cid.Build(cid.CodecForFirstByte(first), key)
	cidStr, rerr := cid.Render(c)
	if rerr != nil {
		return nil, rerr
	}

	// The target is relative to name/<name>: hop up one level to the mount
	// root, then down into blob/<CID>. Relative (not absolute) is the "mount
	// anywhere" shape — it resolves to the sibling blob/ regardless of where
	// the mount lives (~/blob-mount, /mnt/…, …).
	target := "../blob/" + cidStr
	return &nameLink{target: target}, nil
}

// nameLink is the entry under name/: a regular symlink to ../blob/<CID>.
type nameLink struct {
	target string
}

func (l *nameLink) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeSymlink | 0o777
	return nil
}

func (l *nameLink) Readlink(_ context.Context, _ *fuse.ReadlinkRequest) (string, error) {
	return l.target, nil
}

// blobDir is the blob/ directory: one regular file per stored blob, named by
// its rendered CID (base32hex). ReadDirAll deliberately returns nothing —
// listing the CAS is not a store capability (Names() is the index, not the
// blob set), so the directory is addressable-by-path but not enumerable,
// matching how daemon serves /blob/<CID> and /v2/blob/<CID> without ever
// listing the store.
type blobDir struct{ fs *FS }

func (n *blobDir) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

func (*blobDir) ReadDirAll(_ context.Context) ([]fuse.Dirent, error) {
	return nil, nil
}

// Lookup parses the entry name as a CID (any multibase), checks that the
// store holds that codec, and confirms the blob exists (Has — the cheap
// existence probe that gives a clean ENOENT). It returns a blobFile; the
// content itself is streamed on demand by the handle, so a lookup stats the
// entry without reading a byte.
func (n *blobDir) Lookup(_ context.Context, name string) (fs.Node, error) {
	c, perr := cid.Parse(name)
	if perr != nil {
		return nil, syscall.ENOENT
	}
	// A CID the store does not hold (e.g. a foreign dag-pb codec) is not a
	// direct address in this store; it is only referable as a name under
	// name/, never as a blob/<CID> entry.
	if !cid.Supported(cid.Codec(c)) {
		return nil, syscall.ENOENT
	}
	key := storage.Digest(c.Hash())
	have, herr := n.fs.backend.Has(key)
	if herr != nil {
		return nil, herr
	}
	if !have {
		return nil, syscall.ENOENT
	}
	return &blobFile{fs: n.fs, key: key}, nil
}

// blobFile is the regular file under blob/<CID>. Attr is a self-contained
// metadata probe (a backend Stat — no content read, no handle opened at all).
// Open creates a blobHandle that owns the streaming reader for the
// open's lifetime.
type blobFile struct {
	fs  *FS
	key storage.Digest
}

func (b *blobFile) Attr(_ context.Context, a *fuse.Attr) error {
	a.Mode = 0o444
	// Size probe: backend.Stat returns the size and mtime without opening a
	// reader (a dir os.Stat; a sql SELECT with no blob opened; an http HEAD;
	// an s3 StatObject) — no content read and no handle to release, so a
	// `stat` costs exactly one metadata round trip and nothing else.
	stat, err := b.fs.backend.Stat(b.key)
	if err != nil {
		// A lookup already Has()-confirmed the blob, so a stat failure here is
		// a genuine I/O problem, not a not-found (which would already have
		// been ENOENT in the lookup). Surface it as-is.
		return err
	}
	a.Size = uint64(stat.Size)
	if stat.Mtime > 0 {
		t := time.Unix(stat.Mtime, 0).UTC()
		a.Mtime = t
		a.Ctime = t
	}
	return nil
}

func (b *blobFile) Open(_ context.Context, _ *fuse.OpenRequest, _ *fuse.OpenResponse) (fs.Handle, error) {
	// Open the streaming reader once for this open; the handle owns it and
	// releases it on Release. (A lookup has already confirmed existence, so a
	// not-found here would be a race with a concurrent deletion — impossible
	// on a read-only store — hence a plain I/O error surfaces.)
	reader, stat, err := b.fs.backend.Get(b.key)
	if err != nil {
		return nil, err
	}
	return &blobHandle{reader: reader, size: stat.Size}, nil
}

// blobHandle is the open-file view of one blob. It owns the backend reader
// for the open's lifetime and releases it only on **Release** (the FUSE
// "last fd is gone" hook — what returns a pooled sql connection, drops a
// remote S3/HTTP transfer, or closes a dir fd).
//
// Read/Flush distinction (the subtle one): FUSE sends a **Flush on every
// close(2)** of a fd that refers to the handle, and there can be several fds
// per handle; a Flush therefore does NOT mean the handle is over — **Release
// does** (it is sent once, when the last fd closes). So the backend reader is
// left open across any number of Flushes and closed exactly once, on Release.
// This is why `cat` (Open→Read…→Release) and tools that open-then-close a
// helper fd before their real read both work: neither tears the reader down
// until Release.
//
// Reads are **forward-only, page-bounded**: each Read serves the kernel's
// requested page at a time from the current position of a single forward
// reader. The reader never buffers the whole blob — that is the kernel page
// cache's job — and the handle's own buffer is at most the kernel's requested
// page.
//
// Lifetime race: an in-flight Read can race the Release that closes the
// reader. A mutex serializes the state, and a `closed` flag turns a
// post-Release Read into an ordinary EIO (not a nil dereference).
//
// Backward / re-reads are served by the kernel page cache (FUSE caches file
// data in-page by default); a backward request that reaches the handle is
// refused (EIO) rather than answering from the wrong forward position.
type blobHandle struct {
	mu     sync.Mutex
	reader io.Reader
	size   int64
	pos    int64
	closed bool
}

// Read serves the kernel's requested chunk (req.Size bytes at req.Offset)
// from the current forward position of the stream. The read is bounded by
// min(req.Size, remaining) so the handle never buffers beyond one page.
func (h *blobHandle) Read(_ context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		// A read that raced a Release (the kernel can still deliver one
		// after the last fd closed): the reader is gone, so there is nothing
		// to serve. EIO is the honest answer — the open is over — rather than
		// a nil dereference.
		return syscall.EIO
	}

	if req.Offset < h.pos {
		// A backward / non-sequential request. The kernel page cache should
		// have served this already (FUSE caches file data in-page by
		// default); if it did not, we simply cannot serve it — this is a
		// forward-only streaming handle, not a random-access file. Refuse so
		// a caller is never handed wrong bytes for the offset it asked for.
		return syscall.EIO
	}
	if req.Offset > h.pos {
		// A forward jump (a pread gap the kernel did not page-cache). Drain
		// the gap; bounded by the gap and released the moment we hand the
		// caller the requested page.
		if req.Offset >= h.size {
			resp.Data = nil
			return nil
		}
		gap := req.Offset - h.pos
		if _, err := io.CopyN(io.Discard, h.reader, gap); err != nil && err != io.EOF {
			return err
		}
	}

	remaining := h.size - req.Offset
	if remaining <= 0 {
		resp.Data = nil
		return nil
	}
	limit := int64(req.Size)
	if limit <= 0 || limit > remaining {
		limit = remaining
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(h.reader, buf)
	if err != nil && err != io.EOF && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	h.pos = req.Offset + int64(n)
	resp.Data = buf[:n]
	return nil
}

// Flush is sent by the kernel once per close(2) of a fd that refers to this
// handle. The reader must stay open across it (a handle can have several fds,
// and Release is what ends it), so Flush does only per-close housekeeping and
// does NOT touch the reader.
func (h *blobHandle) Flush(_ context.Context, _ *fuse.FlushRequest) error {
	return nil
}

// Release is sent by the kernel exactly once, when the last fd referring to
// this handle is closed — the FUSE "open lifetime is over" hook. This is
// where the backend reader is released (returns a pooled sql connection,
// drops a remote S3/HTTP transfer, closes a dir fd). It is also the natural
// point to tear down a handle if the process is dying: the kernel drops the
// last fd on process exit and delivers this release.
func (h *blobHandle) Release(_ context.Context, _ *fuse.ReleaseRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		if c, ok := h.reader.(io.Closer); ok {
			_ = c.Close()
		}
	}
	return nil
}

// ociTree is the upstream-reference surface: a 5-level projection of the
// name index restricted to names of the shape "oci/<os>/<arch>/<algo>/<hex>".
// The top three levels (os / arch / algo) are directories; the leaf is a
// symlink (a nameLink) to the blob's canonical path, exactly like the
// entries under name/.
//
// The node is parameterised by the depth of the path walked so far:
//
//	level 0  = walking the os components
//	level 1  = walking the arch components
//	level 2  = walking the algo components
//	level 3  = walking the hex leaves (symlinks)
//
// The target of each name is derived the same way nameDir.Lookup does
// (backend.Lookup for the key, backend.Peek for the first-byte codec,
// cid.Build + cid.Render). No bytes of the content are touched on the
// stat/lookup path, so a name may point at a multi-GB blob and its entry
// still stats and lists in constant time.
type ociTree struct {
	fs     *FS
	level  int
	walked []string
}

func (n *ociTree) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0o555
	return nil
}

// ReadDirAll lists the distinct values of the current component (level)
// across all recorded names of the right shape under the walked prefix.
// Names that do not parse as a well-formed oci/ reference are not in this
// surface; they are owned by name/ (see index.IsOciRef).
func (n *ociTree) ReadDirAll(_ context.Context) ([]fuse.Dirent, error) {
	names, err := n.fs.backend.Names()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []fuse.Dirent{}
	for _, nm := range names {
		os_, arch, algo, hex, ok := index.ParseOciRefName(nm)
		if !ok {
			continue
		}
		components := []string{os_, arch, algo, hex}
		if n.level >= len(components) {
			continue
		}
		// The walked prefix must match this name's leading components: the
		// tree walks in the order os, arch, algo, hex.
		if n.level > 0 {
			want := index.OciPrefix + "/" + strings.Join(components[:n.level], "/")
			if want != index.OciPrefix+"/"+strings.Join(n.walked, "/") {
				continue
			}
		}
		cur := components[n.level]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		t := fuse.DT_Dir
		if n.level == 3 {
			t = fuse.DT_Link
		}
		out = append(out, fuse.Dirent{Name: cur, Type: t})
	}
	return out, nil
}

// Lookup resolves the next component (at the current level) to either an
// inner ociTree one level deeper (level < 3) or a nameLink leaf pointing at
// the blob's canonical path (level == 3). The full canonical name is
// "oci/" + the walked components, joined by "/" and recorded as a single
// flat segment under index.Encode.
func (n *ociTree) Lookup(_ context.Context, name string) (fs.Node, error) {
	if n.level > 3 {
		return nil, syscall.ENOENT
	}
	newWalked := append(append([]string{}, n.walked...), name)

	if n.level < 3 {
		return &ociTree{fs: n.fs, level: n.level + 1, walked: newWalked}, nil
	}

	full := index.BuildOciName(newWalked[0], newWalked[1], newWalked[2], newWalked[3])
	// Defensive re-parse: the tree already filters to well-formed shapes in
	// ReadDirAll, but a caller may walk directly — verify the shape before
	// we consult the name table (a malformed hex, an extra component, etc.
	// is not this surface's).
	if _, _, _, _, ok := index.ParseOciRefName(full); !ok {
		return nil, syscall.ENOENT
	}

	be := n.fs.backend
	key, err := be.Lookup(full)
	if err != nil {
		return nil, syscall.ENOENT
	}
	var first byte
	hdr, perr := be.Peek(key, 1)
	if perr != nil && !errors.Is(perr, io.EOF) {
		return nil, perr
	}
	if len(hdr) > 0 {
		first = hdr[0]
	}
	c := cid.Build(cid.CodecForFirstByte(first), key)
	cidStr, rerr := cid.Render(c)
	if rerr != nil {
		return nil, rerr
	}
	// Target is relative to the leaf's directory: the leaf at depth 5 (oci +
	// os + arch + algo + hex under the root) needs 5 - 1 = 4 "../" hops to
	// climb back to the root, then down into "blob/<CID>" — the same shape
	// name/ uses (one "../" to climb from depth 1 to depth 0).
	target := strings.Repeat("../", 4) + "blob/" + cidStr
	return &nameLink{target: target}, nil
}
