package oci

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// memBlob is one layer (name is informational; content is what matters).
type memBlob struct {
	name string
	data []byte
}

// buildLayout creates a single-manifest OCI layout in dir with the given layers
// (plus a config) and returns the index.json bytes and the manifest digest.
func buildLayout(t *testing.T, dir string, layers ...memBlob) (indexBytes []byte, md Digest) {
	t.Helper()

	blobs := map[Digest][]byte{}
	var refLayers []Descriptor
	for _, l := range layers {
		d := DigestOf(l.data)
		blobs[d] = l.data
		refLayers = append(refLayers, Descriptor{MediaType: MediaTypeImageLayer, Digest: d, Size: int64(len(l.data))})
	}

	cfgData := []byte(`{"architecture":"amd64","os":"linux"}`)
	cfgD := DigestOf(cfgData)
	blobs[cfgD] = cfgData

	m := Manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeImageManifest,
		Config:        Descriptor{MediaType: MediaTypeImageConfig, Digest: cfgD, Size: int64(len(cfgData))},
		Layers:        refLayers,
	}
	mb, err := Marshal(&m)
	if err != nil {
		t.Fatal(err)
	}
	mdigest := DigestOf(mb)
	blobs[mdigest] = mb

	idx := Index{
		SchemaVersion: 2,
		MediaType:     MediaTypeImageIndex,
		Manifests:     []Descriptor{{MediaType: MediaTypeImageManifest, Digest: mdigest, Size: int64(len(mb))}},
	}
	ib, err := Marshal(&idx)
	if err != nil {
		t.Fatal(err)
	}

	dl := NewDir(dir)
	if err := dl.New(); err != nil {
		t.Fatal(err)
	}
	for d, data := range blobs {
		if err := dl.Put(d, int64(len(data)), bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := dl.PutIndex(ib); err != nil {
		t.Fatal(err)
	}
	return ib, mdigest
}

func TestReachableExcludesUnreferenced(t *testing.T) {
	dir := t.TempDir()
	ib, _ := buildLayout(t, dir, memBlob{"bin/app", []byte("payload-1")})

	src := NewDir(dir)

	// Add an unreferenced blob (as a hybrid carries).
	unrefData := []byte("stray-not-referenced-by-any-manifest")
	unrefD := DigestOf(unrefData)
	if err := src.Put(unrefD, int64(len(unrefData)), bytes.NewReader(unrefData)); err != nil {
		t.Fatal(err)
	}

	got, err := Reachable(src, ib)
	if err != nil {
		t.Fatal(err)
	}

	want := map[Digest]bool{DigestOf(ib): true}
	idx := mustParseIndex(t, ib)
	want[idx.Manifests[0].Digest] = true
	r, _, err := src.Get(idx.Manifests[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	mf, err := ParseManifest(r)
	if err != nil {
		t.Fatal(err)
	}
	want[mf.Config.Digest] = true
	for _, l := range mf.Layers {
		want[l.Digest] = true
	}

	for d := range want {
		if !contains(got, d) {
			t.Errorf("Reachable missing %s", d)
		}
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("Reachable unexpectedly includes %s", g)
		}
	}
	if contains(got, unrefD) {
		t.Errorf("Reachable includes the unreferenced blob %s", unrefD)
	}
}

func contains(ds []Digest, d Digest) bool {
	for _, x := range ds {
		if x == d {
			return true
		}
	}
	return false
}

func TestCopyRoundTripDir(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	ib, _ := buildLayout(t, srcDir,
		memBlob{"bin/app", []byte("payload-A")},
		memBlob{"data/file.txt", []byte("some text content")},
	)

	src := NewDir(srcDir)
	dst := NewDir(dstDir)
	if err := dst.New(); err != nil {
		t.Fatal(err)
	}

	idig, reachable, err := Copy(src, dst, ib)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if idig != DigestOf(ib) {
		t.Errorf("index digest %s != %s", idig, DigestOf(ib))
	}

	dstIndex, err := dst.IndexBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dstIndex, ib) {
		t.Errorf("dst index != src index\n got %s\nwant %s", dstIndex, ib)
	}

	// reachable[0] is the index digest — stored as index.json, not blobs/.
	// reachable[1:] are the blobs (manifest, config, layers).
	for _, d := range reachable[1:] {
		have, _ := dst.Has(d)
		if !have {
			t.Errorf("dst missing %s", d)
			continue
		}
		sb, err := readAll(src, d)
		if err != nil {
			t.Fatal(err)
		}
		db, err := readAll(dst, d)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(sb, db) {
			t.Errorf("blob %s differs between src and dst", d)
		}
	}
}

// buildTamperedLayout creates a single-manifest OCI layout in dir like
// buildLayout, except the layer: the manifest names a *spoofed* digest
// (<c…c>), while the blob stored at blobs/sha256/<c…c> holds different bytes
// (whose sha256 is something else). The layout is therefore internally valid
// JSON — index → manifest → config/layer all present — but content-inconsistent:
// the bytes do not hash to the digest the manifest claims. That is exactly the
// shape a corrupt or malicious import has, and it is what Verify must reject.
// Every real digest (index, manifest, config) matches its content, so the
// failure is isolated to the single spoofed layer. Returns the index bytes.
func buildTamperedLayout(t *testing.T, dir string) (indexBytes []byte) {
	t.Helper()

	realData := []byte("payload-A-content-not-claimed-by-the-manifest")
	spoof := Digest("sha256:" + make64('c'))    // valid sha256 digest, no matching content
	blobs := map[Digest][]byte{spoof: realData} // stored under the spoofed name

	cfgData := []byte(`{}`)
	blobs[DigestOf(cfgData)] = cfgData

	m := Manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeImageManifest,
		Config:        Descriptor{MediaType: MediaTypeImageConfig, Digest: DigestOf(cfgData), Size: int64(len(cfgData))},
		// Claims the layer is `spoof`; the file under that name holds realData.
		Layers: []Descriptor{{MediaType: MediaTypeImageLayer, Digest: spoof, Size: int64(len(realData))}},
	}
	mb, err := Marshal(&m)
	if err != nil {
		t.Fatal(err)
	}
	blobs[DigestOf(mb)] = mb

	idx := Index{
		SchemaVersion: 2,
		MediaType:     MediaTypeImageIndex,
		Manifests:     []Descriptor{{MediaType: MediaTypeImageManifest, Digest: DigestOf(mb), Size: int64(len(mb))}},
	}
	ib, err := Marshal(&idx)
	if err != nil {
		t.Fatal(err)
	}

	dl := NewDir(dir)
	if err := dl.New(); err != nil {
		t.Fatal(err)
	}
	for d, data := range blobs {
		if err := dl.Put(d, int64(len(data)), bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := dl.PutIndex(ib); err != nil {
		t.Fatal(err)
	}
	return ib
}

func TestVerify(t *testing.T) {
	// A consistent layout (buildLayout derives every digest from the content)
	// verifies cleanly.
	dir := t.TempDir()
	ib, _ := buildLayout(t, dir,
		memBlob{"a", []byte("ok-1")},
		memBlob{"b", []byte("ok-2")},
	)
	if err := Verify(NewDir(dir), ib); err != nil {
		t.Fatalf("Verify(consistent) = %v, want nil", err)
	}

	// A layout whose claimed layer digest does not match the stored content is
	// rejected: the content re-hashes to its real digest, not the spoofed one.
	bad := t.TempDir()
	ibBad := buildTamperedLayout(t, bad)
	err := Verify(NewDir(bad), ibBad)
	if err == nil {
		t.Fatal("Verify(tampered) = nil, want a mismatch error")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("Verify(tampered) = %q, want it to surface a digest mismatch", err)
	}
}

// readAll fetches one blob from a dir layout.
func readAll(src *DirLayout, d Digest) ([]byte, error) {
	r, _, err := src.Get(d)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func TestNewArtifact(t *testing.T) {
	layer := []byte("artifact-payload")
	ld := DigestOf(layer)
	a, err := NewArtifact(ld, int64(len(layer)), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.ConfigDigest != DigestOf([]byte(`{}`)) {
		t.Errorf("config digest %s != empty-config digest", a.ConfigDigest)
	}
	if a.LayerDigest != ld {
		t.Errorf("layer digest %s != %s", a.LayerDigest, ld)
	}
	idx := mustParseIndex(t, a.IndexBytes)
	if len(idx.Manifests) != 1 {
		t.Fatalf("index has %d manifests, want 1", len(idx.Manifests))
	}
	if idx.Manifests[0].Digest != a.ManifestDigest {
		t.Errorf("index manifest digest %s != %s", idx.Manifests[0].Digest, a.ManifestDigest)
	}
	m := mustParseManifest(t, a.ManifestBytes)
	if len(m.Layers) != 1 {
		t.Fatalf("manifest has %d layers, want 1", len(m.Layers))
	}
	if m.Layers[0].Digest != ld {
		t.Errorf("layer digest mismatch")
	}
}

func TestMediaTypesFor(t *testing.T) {
	// SIF magic prefix (10 bytes) -> the SIF types.
	cfg, layer := MediaTypesFor(SifMagic)
	if cfg != MediaTypeSifConfig || layer != MediaTypeSifLayer {
		t.Errorf("MediaTypesFor(SIF magic) = (%q,%q), want (%q,%q)",
			cfg, layer, MediaTypeSifConfig, MediaTypeSifLayer)
	}
	// A real SIF: magic + more bytes.
	cfg, layer = MediaTypesFor(append(append([]byte{}, SifMagic...), []byte("rest")...))
	if cfg != MediaTypeSifConfig || layer != MediaTypeSifLayer {
		t.Errorf("MediaTypesFor(SIF+rest) = (%q,%q), want SIF types", cfg, layer)
	}
	// Launcher-prepended SIF (executable Apptainer/Singularity images): the
	// stub + magic -> the SIF types.
	cfg, layer = MediaTypesFor(append(append([]byte{}, SifLauncher...), SifMagic...))
	if cfg != MediaTypeSifConfig || layer != MediaTypeSifLayer {
		t.Errorf("MediaTypesFor(launcher+SIF) = (%q,%q), want SIF types", cfg, layer)
	}
	// Launcher + magic + more bytes (a real file head).
	cfg, layer = MediaTypesFor(append(append(append([]byte{}, SifLauncher...), SifMagic...), []byte("rest")...))
	if cfg != MediaTypeSifConfig || layer != MediaTypeSifLayer {
		t.Errorf("MediaTypesFor(launcher+SIF+rest) = (%q,%q), want SIF types", cfg, layer)
	}
	// Launcher stub but not the magic after it -> generic.
	cfg, layer = MediaTypesFor(append(append([]byte{}, SifLauncher...), []byte("XIF_MAGIC\x00")...))
	if cfg != MediaTypeUnknownConfig || layer != MediaTypeGenericBinary {
		t.Errorf("MediaTypesFor(launcher+non-SIF) = (%q,%q), want generic", cfg, layer)
	}
	// Non-SIF generic content -> unknown config + generic binary.
	cfg, layer = MediaTypesFor([]byte("SIF_MAGICX")) // last byte wrong
	if cfg != MediaTypeUnknownConfig || layer != MediaTypeGenericBinary {
		t.Errorf("MediaTypesFor(non-SIF) = (%q,%q), want generic", cfg, layer)
	}
	// Shorter than the magic -> non-SIF (no false positive on a prefix).
	cfg, layer = MediaTypesFor([]byte("SIF_MA"))
	if cfg != MediaTypeUnknownConfig || layer != MediaTypeGenericBinary {
		t.Errorf("MediaTypesFor(short) = (%q,%q), want generic", cfg, layer)
	}
	// A JSON blob (artifact with a JSON payload) -> still generic (we only
	// special-case SIF).
	cfg, layer = MediaTypesFor([]byte(`{"a":1}`))
	if cfg != MediaTypeUnknownConfig || layer != MediaTypeGenericBinary {
		t.Errorf("MediaTypesFor(json) = (%q,%q), want generic", cfg, layer)
	}
}

func TestListMembersPlainTar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeTarEntry(t, tw, "bin/app", []byte("hello"))
	writeTarEntry(t, tw, "etc/config", []byte("cfg"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	ms, err := ListMembers(bytes.NewReader(buf.Bytes()), false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range ms {
		names = append(names, m.Name)
	}
	sort.Strings(names)
	if len(names) != 2 {
		t.Fatalf("ListMembers = %v, want 2 entries", names)
	}
}

func TestReadMember(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeTarEntry(t, tw, "a.txt", []byte("AAA"))
	writeTarEntry(t, tw, "b.txt", []byte("BBB"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	rc, size, err := ReadMember(bytes.NewReader(buf.Bytes()), false, "b.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if size != 3 || string(b) != "BBB" {
		t.Errorf("ReadMember(b.txt) = %q (%d), want BBB (3)", b, size)
	}

	rc2, _, err := ReadMember(bytes.NewReader(buf.Bytes()), false, "nope.txt")
	if err != nil {
		t.Errorf("ReadMember(missing) err = %v, want nil", err)
	}
	if rc2 != nil {
		t.Errorf("ReadMember(missing) reader != nil")
	}
}

func TestTarFileSource(t *testing.T) {
	// Build a hybrid-shaped tar: OCI blobs + docker half (ignored on read).
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	ociLayout := []byte(`{"imageLayoutVersion":"1.0.0"}`)
	layer := []byte("layer-bytes")
	layerD := DigestOf(layer)
	// An index that references a manifest digest.
	manifest := []byte(`{"schemaVersion":2}`)
	mD := DigestOf(manifest)
	index := []byte(`{"schemaVersion":2,"manifests":[{"digest":"sha256:` + mD.Encoded() + `"}]}`)
	writeTarEntry(t, tw, "oci-layout", ociLayout)
	writeTarEntry(t, tw, "index.json", index)
	writeTarEntry(t, tw, "manifest.json", []byte(`[{"Config":"x"}]`)) // docker half
	writeTarEntry(t, tw, "repositories", []byte("{}"))                // docker half
	writeTarEntry(t, tw, "blobs/sha256/"+layerD.Encoded(), layer)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "hybrid.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	src, err := NewTarFileSource(f)
	if err != nil {
		t.Fatalf("NewTarFileSource: %v", err)
	}

	ib, err := src.IndexBytes()
	if err != nil {
		t.Fatalf("IndexBytes: %v", err)
	}
	if !bytes.Equal(ib, index) {
		t.Errorf("IndexBytes = %q, want %q", ib, index)
	}

	// The docker half is visible by name — main's legacy-archive warning
	// relies on this — while the index-based path ignores it.
	if !src.HasEntry("manifest.json") || !src.HasEntry("repositories") {
		t.Errorf("HasEntry: legacy docker markers not found")
	}
	if src.HasEntry("index.json") == false || src.HasEntry("blobs/sha256/x") {
		t.Errorf("HasEntry: expected index.json present, blob path absent")
	}

	// The layer (a reachable blob) must be present and readable.
	have, err := src.Has(layerD)
	if err != nil || !have {
		t.Fatalf("Has(layer) = %v, %v; want true, nil", have, err)
	}
	r, size, err := src.Get(layerD)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	lb, _ := io.ReadAll(r)
	if size != int64(len(layer)) || !bytes.Equal(lb, layer) {
		t.Errorf("Get(layer) = %q (%d), want %q (%d)", lb, size, layer, len(layer))
	}
}

// writeTarEntry writes one regular file entry into a tar stream.
func writeTarEntry(t *testing.T, tw *tar.Writer, name string, data []byte) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
}

func TestParseIndexDefaultsMediaType(t *testing.T) {
	// `podman save -f oci` writes an index.json with no mediaType.
	b := []byte(`{"schemaVersion":2,"manifests":[{"digest":"sha256:` + make64('a') + `","size":1}]}`)
	idx, err := ParseIndex(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if idx.MediaType != MediaTypeImageIndex {
		t.Errorf("MediaType = %q, want %q", idx.MediaType, MediaTypeImageIndex)
	}

	// A present-but-wrong media type is still rejected.
	bad := []byte(`{"schemaVersion":2,"mediaType":"application/x","manifests":[{"digest":"sha256:` + make64('a') + `"}]}`)
	if _, err := ParseIndex(bytes.NewReader(bad)); err == nil {
		t.Error("ParseIndex: expected error for foreign mediaType")
	}
}

func make64(c byte) string {
	s := make([]byte, 64)
	for i := range s {
		s[i] = c
	}
	return string(s)
}

func mustParseIndex(t *testing.T, b []byte) *Index {
	t.Helper()
	idx, err := ParseIndex(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func mustParseManifest(t *testing.T, b []byte) *Manifest {
	t.Helper()
	m, err := ParseManifest(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNewLegacyArchive(t *testing.T) {
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	layer := []byte("layer-content")
	manifest := []byte(`[{"Config":"sha256:` + DigestOf(config).Encoded() +
		`","RepoTags":["busybox:latest"],"Layers":["` +
		DigestOf(layer).Encoded() + `.tar.gz"]}]`)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeTarEntry(t, tw, "manifest.json", manifest)
	writeTarEntry(t, tw, "sha256:"+DigestOf(config).Encoded(), config)
	writeTarEntry(t, tw, DigestOf(layer).Encoded()+".tar.gz", layer)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "docker-archive.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	src, err := NewTarFileSource(f)
	if err != nil {
		t.Fatal(err)
	}
	la, err := NewLegacyArchive(src)
	if err != nil {
		t.Fatalf("NewLegacyArchive: %v", err)
	}

	// The synthesized index must be a well-formed OCI image index naming one
	// manifest, whose digest is la.manifestDigest.
	idx := mustParseIndex(t, la.indexBytes)
	if len(idx.Manifests) != 1 {
		t.Fatalf("index has %d manifests, want 1", len(idx.Manifests))
	}
	if idx.Manifests[0].Digest != la.manifestDigest {
		t.Errorf("index manifest digest %s != %s", idx.Manifests[0].Digest, la.manifestDigest)
	}

	// The synthesized manifest must reference the original config and the
	// single layer, with the correct media types and sizes.
	mf := mustParseManifest(t, la.manifestBytes)
	cfgD := DigestOf(config)
	layerD := DigestOf(layer)
	if mf.Config.Digest != cfgD {
		t.Errorf("manifest config digest %s != %s", mf.Config.Digest, cfgD)
	}
	if mf.Config.MediaType != MediaTypeImageConfig {
		t.Errorf("manifest config media type %q", mf.Config.MediaType)
	}
	if mf.Config.Size != int64(len(config)) {
		t.Errorf("manifest config size %d != %d", mf.Config.Size, len(config))
	}
	if len(mf.Layers) != 1 {
		t.Fatalf("manifest has %d layers, want 1", len(mf.Layers))
	}
	if mf.Layers[0].Digest != layerD {
		t.Errorf("manifest layer digest %s != %s", mf.Layers[0].Digest, layerD)
	}
	if mf.Layers[0].MediaType != MediaTypeImageLayerGzip {
		t.Errorf("manifest layer media type %q, want %q", mf.Layers[0].MediaType, MediaTypeImageLayerGzip)
	}
	if mf.Layers[0].Size != int64(len(layer)) {
		t.Errorf("manifest layer size %d != %d", mf.Layers[0].Size, len(layer))
	}

	// Every digest must be served by Get as the right bytes: the index and
	// manifest come from memory, the config and layer come verbatim from the
	// tar (the whole point — no re-hashing, no re-encoding).
	cases := map[Digest][]byte{
		la.indexDigest:    la.indexBytes,
		la.manifestDigest: la.manifestBytes,
		cfgD:              config,
		layerD:            layer,
	}
	for d, want := range cases {
		r, size, err := la.Get(d)
		if err != nil {
			t.Errorf("Get(%s): %v", d, err)
			continue
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Errorf("ReadAll(%s): %v", d, err)
			continue
		}
		if size != int64(len(want)) {
			t.Errorf("Get(%s) size %d != %d", d, size, len(want))
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Get(%s) = %q, want %q", d, got, want)
		}
	}

	// A foreign digest must refuse.
	_, _, err = la.Get(DigestOf([]byte("not in the archive")))
	if err == nil {
		t.Error("Get(foreign digest) = nil err, want missing-digest error")
	}
}

func TestNewLegacyArchiveMissingBlob(t *testing.T) {
	// manifest.json references a layer that is not in the tar: NewLegacyArchive
	// must fail at construction, not at Get time.
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	manifest := []byte(`[{"Config":"sha256:` + DigestOf(config).Encoded() +
		`","RepoTags":[],"Layers":["` + make64('a') + `"]}]`)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeTarEntry(t, tw, "manifest.json", manifest)
	writeTarEntry(t, tw, "sha256:"+DigestOf(config).Encoded(), config)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "missing.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src, err := NewTarFileSource(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLegacyArchive(src); err == nil {
		t.Error("NewLegacyArchive: err = nil, want missing-entry error")
	}
}

func TestNewLegacyArchiveMultipleImages(t *testing.T) {
	// A docker archive with more than one image entry is not the single-image
	// case this import handles: refuse with a clear error.
	config := []byte(`{}`)
	hex := DigestOf(config).Encoded()
	manifest := []byte("[" +
		`{"Config":"sha256:` + hex + `","Layers":[]}` + "," +
		`{"Config":"sha256:` + hex + `","Layers":[]}` + "]")

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeTarEntry(t, tw, "manifest.json", manifest)
	writeTarEntry(t, tw, "sha256:"+hex, config)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "multi.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src, err := NewTarFileSource(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLegacyArchive(src); err == nil {
		t.Error("NewLegacyArchive: err = nil, want multi-image error")
	}
}

func TestReadEntryAndEntrySize(t *testing.T) {
	// EntrySize + ReadEntry are the by-name accessors the legacy archive uses:
	// they must surface any regular-file entry, not just layout-shaped ones.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeTarEntry(t, tw, "manifest.json", []byte(`[]`))
	writeTarEntry(t, tw, "data.txt", []byte("hello world"))
	writeTarEntry(t, tw, "blobs/sha256/deadbeef", []byte("blob"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "entries.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	src, err := NewTarFileSource(f)
	if err != nil {
		t.Fatal(err)
	}

	if size, ok := src.EntrySize("data.txt"); !ok || size != 11 {
		t.Errorf("EntrySize(data.txt) = %d, %v; want 11, true", size, ok)
	}
	if size, ok := src.EntrySize("nope"); ok || size != 0 {
		t.Errorf("EntrySize(nope) = %d, %v; want 0, false", size, ok)
	}

	r, size, err := src.ReadEntry("blobs/sha256/deadbeef")
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}
	defer r.Close()
	if size != 4 {
		t.Errorf("ReadEntry size = %d, want 4", size)
	}
	b, _ := io.ReadAll(r)
	if string(b) != "blob" {
		t.Errorf("ReadEntry = %q, want %q", b, "blob")
	}

	if _, _, err := src.ReadEntry("nope"); err == nil {
		t.Error("ReadEntry(nope) err = nil, want missing-entry")
	}
}

func TestDigestFromName(t *testing.T) {
	cases := []struct {
		name string
		hex  string
	}{
		{"sha256:" + make64('a'), make64('a')},
		{make64('b') + ".tar", make64('b')},
		{make64('c') + ".tar.gz", make64('c')},
		{make64('d') + ".tgz", make64('d')},
	}
	for _, c := range cases {
		got, ok := digestFromName(c.name)
		if !ok || got != c.hex {
			t.Errorf("digestFromName(%q) = %q, %v; want %q, true", c.name, got, ok, c.hex)
		}
	}
}
