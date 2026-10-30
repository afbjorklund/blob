package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"blob/cid"
	"blob/identity"
	"blob/index"
	"blob/oci"
	"blob/storage"

	"github.com/spf13/cobra"
)

// OCI import/export commands. These bridge the store's multihash keys to OCI
// digests using the identity package. Blobs are streamed between the layout and
// the store; only the small index.json and manifest are buffered.

// storeSink adapts a ReadWrite backend to an oci.Sink, translating each OCI
// digest to the store's identity (multihash) key.
type storeSink struct {
	backend storage.StorageBackend
}

func (s storeSink) Has(d oci.Digest) (bool, error) {
	id, err := identity.FromOCI(d)
	if err != nil {
		return false, nil
	}
	return s.backend.Has(id)
}

func (s storeSink) Put(d oci.Digest, size int64, r io.Reader) error {
	id, err := identity.FromOCI(d)
	if err != nil {
		return err
	}
	return s.backend.Put(id, size, r)
}

func (s storeSink) PutIndex(indexBytes []byte) error {
	id, err := identity.FromOCI(oci.DigestOf(indexBytes))
	if err != nil {
		return err
	}
	return s.backend.Put(id, int64(len(indexBytes)), bytes.NewReader(indexBytes))
}

// storeSource adapts a ReadOnly backend to an oci.Source, translating each OCI
// digest to the store's identity key.
type storeSource struct {
	backend storage.StorageBackend
}

func (s storeSource) Has(d oci.Digest) (bool, error) {
	id, err := identity.FromOCI(d)
	if err != nil {
		return false, nil
	}
	return s.backend.Has(id)
}

func (s storeSource) Get(d oci.Digest) (io.ReadCloser, int64, error) {
	id, err := identity.FromOCI(d)
	if err != nil {
		return nil, 0, err
	}
	// The store's Get returns (reader, Stat) — the Stat carries size, mtime,
	// and a local path when the backend keeps one. This adapter is used by
	// the OCI import/export path which streams bytes, so we project only
	// the size.
	r, st, err := s.backend.Get(id)
	return r, st.Size, err
}

// layoutSource is a read side (blobs by digest) that can also yield its own
// index.json — satisfied by oci.DirLayout and oci.TarFileSource.
type layoutSource interface {
	oci.Source
	IndexBytes() ([]byte, error)
}

// artifactSource serves the four blobs a wrapped file becomes (index, manifest,
// empty config, layer) so it can be imported exactly like a layout.
type artifactSource struct {
	a     *oci.Artifact
	layer io.Reader
	size  int64
}

func (s *artifactSource) Has(d oci.Digest) (bool, error) {
	switch d {
	case s.a.IndexDigest, s.a.ManifestDigest, s.a.ConfigDigest, s.a.LayerDigest:
		return true, nil
	}
	return false, nil
}

func (s *artifactSource) Get(d oci.Digest) (io.ReadCloser, int64, error) {
	switch d {
	case s.a.IndexDigest:
		return io.NopCloser(bytes.NewReader(s.a.IndexBytes)), int64(len(s.a.IndexBytes)), nil
	case s.a.ManifestDigest:
		return io.NopCloser(bytes.NewReader(s.a.ManifestBytes)), int64(len(s.a.ManifestBytes)), nil
	case s.a.ConfigDigest:
		return io.NopCloser(bytes.NewReader(oci.EmptyConfig)), int64(len(oci.EmptyConfig)), nil
	case s.a.LayerDigest:
		return io.NopCloser(s.layer), s.size, nil
	}
	return nil, 0, fmt.Errorf("oci: unknown digest %s", d)
}

func (s *artifactSource) IndexBytes() ([]byte, error) {
	return s.a.IndexBytes, nil
}

// firstByteCodec applies the content-based codec rule to a buffer (JSON for '{'
// or '[', raw otherwise) — the rule cid/centralises for a single byte.
func firstByteCodec(b []byte) cid.Code {
	if len(b) > 0 {
		return cid.CodecForFirstByte(b[0])
	}
	return cid.CodeRaw
}

// resolveStoreKey turns an `oci get/ls` argument into the store key (identity)
// of its root object: parsed as a CID when possible, else via the name index.
//
// A CID whose codec the store doesn't hold (a dag-pb CID from `ipfs add`,
// for a dag-pb-wrapped layout) is *not* a direct handle here, but we still
// let it through the name index — if a name entry was recorded under that
// exact string by an earlier `oci import --name <that-foreign-CID>`, the
// name lookup is how we reach it. The digest must match the CID's own
// (or the store/index are inconsistent and we refuse to lie about it).
func resolveStoreKey(arg string) (identity.Identity, error) {
	var foreignCid cid.Cid
	var isForeign bool
	if c, err := cid.Parse(arg); err == nil {
		if cid.Supported(cid.Codec(c)) {
			key, err := digestFromCID(c)
			if err != nil {
				return nil, err
			}
			return key, nil
		}
		foreignCid = c
		isForeign = true
	}
	if id, err := identity.FromOCIString(arg); err == nil {
		return id, nil
	}
	backend, err := getStorageBackend(storage.ReadOnly)
	if err != nil {
		return nil, err
	}
	defer backend.Close()
	key, err := backend.Lookup(arg)
	if err != nil {
		if isForeign {
			return nil, fmt.Errorf("unsupported codec %q: this store only holds raw and json (and the name %q is not recorded)", cid.CodecName(cid.Codec(foreignCid)), arg)
		}
		return nil, fmt.Errorf("%q: name not found in index", arg)
	}
	if isForeign && !bytes.Equal(key, cid.Key(foreignCid)) {
		return nil, fmt.Errorf("name %q resolves to a different digest than the CID it is named after (store/index inconsistency)", arg)
	}
	return key, nil
}

// openLayoutSource opens arg as an OCI source: a directory layout, a tar
// archive layout (plain, or gzip-compressed — a .tgz/.tar.gz), or (falling
// back) a bare file wrapped as a single-layer artifact. It returns the source,
// a closer the caller must invoke (nil otherwise), and the name target: the
// digest whose store key the name index should record names against ("" for a
// genuine layout, which names its index; for a wrapped bare file, the file's
// own bytes — the artifact's single layer — so that a name recorded by
// `oci import f` and one recorded by `blob add f` resolve to the same key and
// the OCI scaffolding is addressed by its index CID, not by name).
//
// Routing is content-based, in this order:
//
//  1. a tar whose contents parse as a layout (index.json + ≥1 manifest) →
//     the layout's blobs; for a gzip file, decompressed to a temp file first;
//  2. a tar without index.json (e.g. a legacy docker archive) or a gzip that
//     is not a tar → warn, then a single-layer artifact of the file's own
//     bytes;
//  3. anything else → a single-layer artifact of the file's own bytes.
func openLayoutSource(arg string) (layoutSource, func(), oci.Digest, error) {
	// A directory is an OCI layout.
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		return oci.NewDir(arg), nil, "", nil
	}

	// A file: try to read it as a tar layout (docker-save / podman-save output,
	// including the v25+ "hybrid", whose OCI half we read and docker half we
	// ignore).
	f, err := os.Open(arg)
	if err != nil {
		return nil, nil, "", err
	}
	if isGzip(f) {
		// A gzip stream is not a tar: decompress it to a temp file and read
		// that as the tar. The temp file stays open for TarFileSource's seeks,
		// so the successful source's closer removes and closes it; the raw
		// source f is dropped along the way.
		dir, err := filepath.Abs(filepath.Dir(arg))
		if err != nil {
			_ = f.Close()
			return nil, nil, "", err
		}
		tmp, err := os.CreateTemp(dir, filepath.Base(arg)+".gunzipped-*.tmp")
		if err != nil {
			_ = f.Close()
			return nil, nil, "", err
		}
		if err := gunzipTo(f, tmp); err != nil {
			_ = f.Close()
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
			return nil, nil, "", fmt.Errorf("oci: %s is not a valid gzip stream: %v", arg, err)
		}
		_ = f.Close()
		// gunzipTo read src and wrote tmp (leaving its position at the end);
		// rewind it so the tar scan below reads from the start.
		_, _ = tmp.Seek(0, io.SeekStart)
		if src, err := oci.NewTarFileSource(tmp); err == nil {
			if _, err := src.IndexBytes(); err == nil {
				return src, func() {
					_ = tmp.Close()
					_ = os.Remove(tmp.Name())
				}, "", nil
			}
		}
		// A gzip that is not a tar layout (or a layout with no index.json)
		// still means the *raw* file is the artifact, not its contents.
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return artifactWrap(arg)
	}
	if src, err := oci.NewTarFileSource(f); err == nil {
		if _, err := src.IndexBytes(); err == nil {
			return src, func() { _ = f.Close() }, "", nil
		}
		// A manifest.json without an index.json is a legacy docker archive
		// (e.g. `podman save` with no -f). We do not import that format's
		// per-blob structure — warn and fall through to the single-blob wrap.
		if src.HasEntry("manifest.json") {
			_, _ = fmt.Fprintf(os.Stderr,
				"warning: %s is a legacy docker archive (manifest.json, no OCI index): importing it as a single blob (wrapped as an OCI artifact)\n"+
					"  for per-layer import, save it in OCI form first: `podman save -f oci` or docker v25+\n",
				arg)
		}
	}
	_ = f.Close()

	// Not a (gzip) tar layout: a bare file — wrap it as a single-layer artifact.
	return artifactWrap(arg)
}

// artifactWrap wraps a bare file as a single-layer OCI artifact (its own bytes
// as the layer — the digest recorded as nameTarget, matching `blob add`; the
// index/manifest/config are the scaffold addressed by their own CIDs).
func artifactWrap(arg string) (layoutSource, func(), oci.Digest, error) {
	size, err := fileSize(arg)
	if err != nil {
		return nil, nil, "", err
	}
	layerDigest := ociDigestOf(arg)
	if layerDigest == "" {
		return nil, nil, "", fmt.Errorf("oci: could not compute digest of %s", arg)
	}
	// Sniff the first SifSniffLen bytes to pick the media types (SIF or
	// generic). Bounded; the file is not buffered.
	head, err := readFirstBytes(arg, oci.SifSniffLen)
	if err != nil {
		return nil, nil, "", err
	}
	cfgMT, layerMT := oci.MediaTypesFor(head)
	a, err := oci.NewArtifact(layerDigest, size, layerMT, cfgMT)
	if err != nil {
		return nil, nil, "", err
	}
	layer, err := openFile(arg)
	if err != nil {
		return nil, nil, "", err
	}
	return &artifactSource{a: a, layer: layer, size: size}, func() { _ = layer.Close() }, layerDigest, nil
}

// gzipMagic are the leading two bytes of every gzip stream (RFC 1952).
var gzipMagic = []byte{0x1f, 0x8b}

// isGzip reports whether r's first two bytes are the gzip magic, and rewinds r
// to the start so the caller can still read the whole stream.
func isGzip(r *os.File) bool {
	head := make([]byte, 2)
	m, _ := io.ReadFull(r, head)
	gzip := m == 2 && bytes.Equal(head[:2], gzipMagic)
	_, _ = r.Seek(0, io.SeekStart)
	return gzip
}

// gunzipTo streams the gzip at src decompressed into dst (appending). src and
// dst are not closed by this; the caller owns both and must Close dst (its
// Sync is what commits the data, so close it after the read the caller needs).
func gunzipTo(src *os.File, dst *os.File) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, gz); err != nil {
		_ = gz.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	return nil
}

// fileSize returns the size of a regular file.
func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// readFirstBytes reads up to n leading bytes of a file (fewer if shorter,
// 0 if empty) and closes the file. A streaming helper for the magic-byte
// sniff — bounded by n, never materializes the blob.
func readFirstBytes(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	m, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:m], nil
}

// ociDigestOf is a streaming helper kept here so the bare-file wrap path does
// not need a separate hashing call — it re-uses identity to name the layer.
// (The artifact path in main already hashes once; this is only for the case
// above, which hashes the file a second time to obtain its digest. For large
// files the caller could pass the digest directly.)
func ociDigestOf(path string) oci.Digest {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	id, err := identity.HashReader(f)
	if err != nil {
		return ""
	}
	d, err := identity.ToOCI(id)
	if err != nil {
		return ""
	}
	return d
}

// openFile returns an open handle to path (the caller must Close it), or an
// error. It exists so the bare-file wrap path can stream the layer directly
// from an open descriptor rather than re-opening it.
func openFile(path string) (*os.File, error) { return os.Open(path) }

// registerOCICmds wires the `oci` command group onto root.
func registerOCICmds(root *cobra.Command) {
	ociCmd := &cobra.Command{
		Use:   "oci",
		Short: "Import and export OCI layouts (images and artifacts)",
	}

	// The primary verbs are `import`/`export` — OCI is the format these
	// commands speak (in a layout, out a layout). `add` and `get` are kept as
	// aliases so they read like the rest of the CLI where a name/CID is the
	// native address: for this store that is the CID, and OCI is supporting
	// conversion around it, not the store's own language.
	addCmd := &cobra.Command{
		Use:     "import <layout-dir | layout.tar | file>",
		Aliases: []string{"add"},
		Short:   "Import an OCI layout (or wrap a file as an OCI artifact) into the store",
		Args:    cobra.ExactArgs(1),
		RunE:    runOCIAdd,
	}
	addCmd.Flags().StringArrayVarP(&ociNames, "name", "", []string{}, "additional index name to record (repeatable); the basename of the argument is always recorded as well — for a layout pointing at its index, for a bare file pointing at the file's own bytes (the artifact's single layer, the same key `blob add` uses)")

	getCmd := &cobra.Command{
		Use:     "export <cid|name|digest> -o <dir | tar>",
		Aliases: []string{"get"},
		Short:   "Export a stored OCI layout to a directory or tar",
		Args:    cobra.ExactArgs(1),
		RunE:    runOCIExport,
	}
	getCmd.Flags().StringVarP(&outputPath, "output", "o", "", "output directory or .tar file (default: the argument)")

	lsCmd := &cobra.Command{
		Use:   "ls <cid|name>",
		Short: "List a stored OCI layout's manifests and layers",
		Args:  cobra.ExactArgs(1),
		RunE:  runOCILs,
	}

	ociCmd.AddCommand(addCmd, getCmd, lsCmd)
	root.AddCommand(ociCmd)
}

// runOCIAdd imports an OCI layout (or wraps a bare file as an artifact) into
// the store and records names. The *name* always points at the object the user
// handed us: for a layout (dir/tar) the layout's index, for a bare file the
// file's own bytes (its single layer) — the same key `blob add` would record,
// so a name from `oci import f` and one from `blob add f` cannot clobber each
// other and `blob get f` keeps returning the file. Each --name is just an
// additional alias of that same key. The printed CID is the layout's index —
// the OCI scaffold — and is the object to `oci export`/`oci ls`/`oras pull`
// the bare file in (its name is the file's, not the scaffold's).
func runOCIAdd(cmd *cobra.Command, args []string) error {
	src, closer, nameTarget, err := openLayoutSource(args[0])
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer()
	}

	indexBytes, err := src.IndexBytes()
	if err != nil {
		return err
	}

	backend, err := getStorageBackend(storage.ReadWrite)
	if err != nil {
		return err
	}
	defer backend.Close()

	indexDigest, reachable, err := oci.Copy(src, storeSink{backend}, indexBytes)
	if err != nil {
		return err
	}

	indexKey, err := identity.FromOCI(indexDigest)
	if err != nil {
		return err
	}

	// The layout's own CID: the index bytes under the content-based codec,
	// rendered base32hex — the OCI scaffold's address, the object the daemon's
	// /v2/ and /blob/ surfaces serve, and the handle for `oci export`/`ls`.
	c := cid.Build(firstByteCodec(indexBytes), indexKey)
	cidStr, err := cid.Render(c)
	if err != nil {
		return err
	}

	// Names record against the object given: the layout's index for a dir/tar,
	// or the file's bytes (nameTarget) for a bare file, so `oci import f` and
	// `blob add f` name the same key. A bare file takes its name straight from
	// the basename — exactly what `blob add` uses — unlike a layout, whose
	// `.tar`/`.tgz` suffix is stripped (`docker load` convention).
	base := index.Default(args[0])
	nameKey := indexKey
	if nameTarget != "" {
		base = filepath.Base(args[0])
		nameKey, err = identity.FromOCI(nameTarget)
		if err != nil {
			return err
		}
	}

	records, skipped, err := recordNames(base, ociNames, nameKey, backend)
	if err != nil {
		return err
	}

	if len(skipped) > 0 {
		fmt.Printf("Imported %d OCI blob(s); index %s; name %s; skipped %s (it is the blob's own handle)\n",
			len(reachable), cidStr, strings.Join(records, ", "), strings.Join(skipped, ", "))
		return nil
	}
	if len(records) == 1 {
		fmt.Printf("Imported %d OCI blob(s); index %s; name %q\n", len(reachable), cidStr, records[0])
		return nil
	}
	fmt.Printf("Imported %d OCI blob(s); index %s; names %s\n",
		len(reachable), cidStr, strings.Join(records, ", "))
	return nil
}

// recordNames records the index names for an import: the baseline `base`
// name plus every `extras` entry, de-duplicated, all pointing at the same
// blob key (`key`). For a layout (dir/tar) `key` is the layout's index and
// `base` is its basename with the `.tar`-family suffix stripped; for a bare
// file `key` is the file's own bytes (its single layer, the same key `blob
// add` uses) and `base` is its literal basename. A name that `index.Check`
// flags as the blob's own native handle (a sha256 digest or a CID of a held
// codec that resolves to `key`) is *skipped* — it is redundant with the
// blob's direct address — and reported separately.
func recordNames(base string, extras []string, key identity.Identity, backend storage.StorageBackend) (records, skipped []string, err error) {
	seen := map[string]bool{}
	record := func(nm string) error {
		if seen[nm] {
			return nil
		}
		seen[nm] = true
		skip, cerr := index.Check(nm, key)
		if cerr != nil {
			return cerr
		}
		if skip {
			skipped = append(skipped, nm)
			return nil
		}
		if err := backend.Index(nm, key); err != nil {
			return err
		}
		records = append(records, nm)
		return nil
	}

	if base != "" {
		if err = record(base); err != nil {
			return records, skipped, err
		}
	}
	for _, nm := range extras {
		if nm == "" {
			continue
		}
		if err = record(nm); err != nil {
			return records, skipped, err
		}
	}
	return records, skipped, nil
}

// requireOCIShape checks the resolved blob is stored under the JSON codec —
// via the same first-byte Peek the codec rule itself uses (so the blob is
// never read, only one byte), not by parsing it. An OCI index is always
// JSON, so a non-JSON blob is not a layout; naming it as such (rather than
// dumping a raw decode error over its first bytes) is the whole point.
func requireOCIShape(key identity.Identity, backend storage.StorageBackend) error {
	first, err := peekFirstByte(backend, storage.Digest(key))
	if err != nil {
		return err
	}
	if cid.CodecForFirstByte(first) != cid.CodeJson {
		return fmt.Errorf("not an OCI layout: the blob is not JSON, so it cannot be an index.json (it may have been added with `blob add`; `oci ls` needs the OCI index CID)")
	}
	return nil
}

// runOCIExport writes a stored OCI layout to a directory or tar.
func runOCIExport(cmd *cobra.Command, args []string) error {
	key, err := resolveStoreKey(args[0])
	if err != nil {
		return err
	}

	backend, err := getStorageBackend(storage.ReadOnly)
	if err != nil {
		return err
	}
	defer backend.Close()

	if err := requireOCIShape(key, backend); err != nil {
		return err
	}

	indexReader, _, err := backend.Get(key)
	if err != nil {
		return err
	}
	defer indexReader.Close()
	indexBytes, err := io.ReadAll(indexReader)
	if err != nil {
		return err
	}

	out := outputPath
	if out == "" {
		out = args[0]
	}

	sink, closeSink, err := layoutSinkFor(out)
	if err != nil {
		return err
	}
	defer closeSink()

	src := storeSource{backend}
	_, _, err = oci.Copy(src, sink, indexBytes)
	if err != nil {
		return err
	}
	fmt.Printf("Exported %q -> %s\n", args[0], out)
	return nil
}

// layoutSinkFor builds an oci.Sink + closer for a dir or tar output path.
func layoutSinkFor(out string) (oci.Sink, func(), error) {
	if isTarPath(out) {
		f, err := os.Create(out)
		if err != nil {
			return nil, nil, err
		}
		tw := tar.NewWriter(f)
		return oci.NewTarSink(tw), func() { _ = tw.Close(); _ = f.Close() }, nil
	}
	d := oci.NewDir(out)
	if err := d.New(); err != nil {
		return nil, nil, err
	}
	return d, func() {}, nil
}

func isTarPath(p string) bool {
	return filepath.Ext(p) == ".tar" || filepath.Ext(p) == ".tgz"
}

// runOCILs prints a stored layout's index, manifests, and layer descriptors.
func runOCILs(cmd *cobra.Command, args []string) error {
	key, err := resolveStoreKey(args[0])
	if err != nil {
		return err
	}

	backend, err := getStorageBackend(storage.ReadOnly)
	if err != nil {
		return err
	}
	defer backend.Close()

	if err := requireOCIShape(key, backend); err != nil {
		return err
	}

	indexReader, _, err := backend.Get(key)
	if err != nil {
		return err
	}
	defer indexReader.Close()
	indexBytes, err := io.ReadAll(indexReader)
	if err != nil {
		return err
	}

	idx, err := oci.ParseIndex(bytes.NewReader(indexBytes))
	if err != nil {
		return err
	}

	fmt.Printf("index %s (%s, %d manifest(s))\n",
		oci.DigestOf(indexBytes), idx.MediaType, len(idx.Manifests))

	src := storeSource{backend}
	for _, m := range idx.Manifests {
		r, _, err := src.Get(m.Digest)
		if err != nil {
			fmt.Printf("  manifest %s: <unreadable: %v>\n", m.Digest, err)
			continue
		}
		defer r.Close()
		mf, err := oci.ParseManifest(r)
		if err != nil {
			fmt.Printf("  manifest %s: <invalid: %v>\n", m.Digest, err)
			continue
		}
		fmt.Printf("  manifest %s (%s, %d B)\n",
			m.Digest, mf.MediaType, m.Size)
		if mf.Config.Digest != "" {
			fmt.Printf("    config  %s (%s, %d B)\n",
				mf.Config.Digest, mf.Config.MediaType, mf.Config.Size)
		}
		for i, l := range mf.Layers {
			fmt.Printf("    layer[%d] %s (%s, %d B)\n", i, l.Digest, l.MediaType, l.Size)
		}
	}
	return nil
}
