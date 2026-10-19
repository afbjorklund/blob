package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"blob/cid"
	"blob/identity"
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
	// The store's Get returns (reader, size, mtime, err) — the third value
	// is the blob's last-modified time. This adapter is used by the OCI
	// import/export path which streams bytes, so we drop the mtime.
	r, size, _, err := s.backend.Get(id)
	return r, size, err
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
func resolveStoreKey(arg string) (identity.Identity, error) {
	if c, err := cid.Parse(arg); err == nil {
		key, err := digestFromCID(c)
		if err != nil {
			return nil, err
		}
		return key, nil
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
		return nil, fmt.Errorf("%q: name not found in index", arg)
	}
	return key, nil
}

// openLayoutSource opens arg as an OCI source: a directory, a tar archive, or
// (falling back) a single bare file wrapped as a single-layer artifact. It
// returns the source and whether the bare-file path was taken (so the caller
// knows to close the opened file).
func openLayoutSource(arg string) (layoutSource, error) {
	// A directory is an OCI layout.
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		return oci.NewDir(arg), nil
	}

	// A file: try to read it as a tar layout (docker-save / podman-save output,
	// including the v25+ "hybrid", whose OCI half we read and docker half we
	// ignore). If it has no index.json it is not an OCI layout, so wrap the
	// whole file as a single-layer artifact.
	f, err := os.Open(arg)
	if err != nil {
		return nil, err
	}
	if src, err := oci.NewTarFileSource(f); err == nil {
		if _, err := src.IndexBytes(); err == nil {
			return src, nil
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

	// Not a tar layout: wrap the file as a single-layer artifact.
	size, err := fileSize(arg)
	if err != nil {
		return nil, err
	}
	layerDigest := ociDigestOf(arg)
	if layerDigest == "" {
		return nil, fmt.Errorf("oci: could not compute digest of %s", arg)
	}
	// Sniff the first len(oci.SifMagic) bytes to pick the media types
	// (SIF or generic). Bounded by 10 bytes; the file is not buffered.
	head, err := readFirstBytes(arg, len(oci.SifMagic))
	if err != nil {
		return nil, err
	}
	cfgMT, layerMT := oci.MediaTypesFor(head)
	a, err := oci.NewArtifact(layerDigest, size, layerMT, layerMT, cfgMT)
	if err != nil {
		return nil, err
	}
	layer, err := openFile(arg)
	if err != nil {
		return nil, err
	}
	return &artifactSource{a: a, layer: layer, size: size}, nil
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
	addCmd.Flags().StringVarP(&ociName, "name", "", "", "index name to record (default: basename of the argument)")

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
// the store and records it under a name.
func runOCIAdd(cmd *cobra.Command, args []string) error {
	src, err := openLayoutSource(args[0])
	if err != nil {
		return err
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

	name := ociName
	if name == "" {
		name = defaultName(args[0])
	}
	if err := backend.Index(name, indexKey); err != nil {
		return err
	}

	c := cid.Build(firstByteCodec(indexBytes), indexKey)
	cidStr, err := cid.Render(c)
	if err != nil {
		return err
	}
	fmt.Printf("Imported %d OCI blob(s); index %s (name %q)\n", len(reachable), cidStr, name)
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

	indexReader, _, _, err := backend.Get(key)
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

// defaultName derives an index name from an argument path, stripping a
// .tar/.tgz/.tar.gz suffix when present.
func defaultName(arg string) string {
	base := filepath.Base(arg)
	for _, suffix := range []string{".tar.gz", ".tar", ".tgz"} {
		base = trimSuffix(base, suffix)
	}
	return base
}

func trimSuffix(s, suffix string) string {
	if len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)]
	}
	return s
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

	indexReader, _, _, err := backend.Get(key)
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
		atype := mf.ArtifactType
		fmt.Printf("  manifest %s (%s, %d B%s)\n",
			m.Digest, mf.MediaType, m.Size, artifactSuffix(atype))
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

func artifactSuffix(at string) string {
	if at == "" {
		return ""
	}
	return " artifactType=" + at
}
