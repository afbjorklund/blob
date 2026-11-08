package oci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	godigest "github.com/opencontainers/go-digest"
)

// A legacy docker archive is the `docker save` (and pre-OCI `podman save`)
// format: a tar whose entries are the image config `sha256:<hex>`, one layer
// per `<hex>.tar.gz`, and a top-level `manifest.json` — a *JSON array* that
// doubles as both the index (one entry per image) and the manifest (config +
// layers), carrying no index.json and no per-blob manifest:
//
//	[{"Config":"sha256:<hex>","RepoTags":["name:tag"],"Layers":["<hex>.tar.gz"]}]
//
// The config and layers are already canonical by content address: the tar
// entry name is the sha256 digest of the blob (with a `.tar.gz` suffix on
// compressed layers). So "upgrading" the archive to OCI form is pure
// scaffolding: synthesize an index.json and a single image manifest that
// reference the existing blobs, and read those blobs verbatim. Nothing is
// re-encoded or re-hashed, and `RepoTags` (a user label, not content) is not
// consulted.

// legacyImage is one entry of a docker archive's manifest.json.
type legacyImage struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags,omitempty"` // recorded, not used
	Layers   []string `json:"Layers"`
}

// blobRef is one blob the archive references: its OCI digest, its tar entry
// name, and its size (so the synthesized manifest carries correct sizes).
type blobRef struct {
	digest Digest
	entry  string // tar entry name (e.g. "sha256:<hex>" or "<hex>.tar.gz")
	size   int64
}

// LegacyArchive is the OCI-side view of a legacy docker archive, built over a
// TarFileSource. It synthesizes the OCI index + manifest that reference the
// archive's config and layers, and serves those blobs straight from the tar.
// It satisfies the store's layoutSource (oci.Source plus IndexBytes), so the
// import path ingests it exactly as it would a genuine layout.
type LegacyArchive struct {
	src            *TarFileSource
	indexBytes     []byte
	indexDigest    Digest
	manifestBytes  []byte
	manifestDigest Digest
	config         blobRef
	layers         []blobRef
}

// NewLegacyArchive inspects src's manifest.json and builds the OCI scaffolding
// (index + manifest) that references its config and layers. Every referenced
// blob must be present in the tar.
func NewLegacyArchive(src *TarFileSource) (*LegacyArchive, error) {
	data, err := entryBytes(src, "manifest.json")
	if err != nil {
		return nil, err
	}

	imgs, err := parseLegacyManifest(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	img := imgs[0]

	// Resolve each named blob to (digest, size), verifying it is present in
	// the tar before committing to the reference — a dangling reference is an
	// error here, not a Get-time failure.
	newRef := func(name string) (blobRef, error) {
		hex, ok := digestFromName(name)
		if !ok {
			return blobRef{}, fmt.Errorf("%q does not encode a sha256 digest", name)
		}
		size, present := src.EntrySize(name)
		if !present {
			return blobRef{}, fmt.Errorf("%q is not present in the archive", name)
		}
		return blobRef{digest: godigest.NewDigestFromEncoded(godigest.SHA256, hex), entry: name, size: size}, nil
	}

	config, err := newRef(img.Config)
	if err != nil {
		return nil, fmt.Errorf("oci: docker archive config %q: %v", img.Config, err)
	}

	layers := make([]blobRef, 0, len(img.Layers))
	for _, name := range img.Layers {
		ref, err := newRef(name)
		if err != nil {
			return nil, fmt.Errorf("oci: docker archive layer %q: %v", name, err)
		}
		layers = append(layers, ref)
	}

	mf := Manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeImageManifest,
		Config:        Descriptor{MediaType: MediaTypeImageConfig, Digest: config.digest, Size: config.size},
		Layers:        layerDescriptors(layers),
	}
	mb, err := Marshal(&mf)
	if err != nil {
		return nil, err
	}
	md := DigestOf(mb)

	idx := Index{
		SchemaVersion: 2,
		MediaType:     MediaTypeImageIndex,
		Manifests:     []Descriptor{{MediaType: MediaTypeImageManifest, Digest: md, Size: int64(len(mb))}},
	}
	ib, err := Marshal(&idx)
	if err != nil {
		return nil, err
	}

	return &LegacyArchive{
		src:            src,
		indexBytes:     ib,
		indexDigest:    DigestOf(ib),
		manifestBytes:  mb,
		manifestDigest: md,
		config:         config,
		layers:         layers,
	}, nil
}

// IndexBytes returns the synthesized OCI index.json.
func (a *LegacyArchive) IndexBytes() ([]byte, error) {
	return a.indexBytes, nil
}

// Has reports whether d is one of the blobs this archive supplies.
func (a *LegacyArchive) Has(d Digest) (bool, error) {
	if d == a.indexDigest || d == a.manifestDigest || d == a.config.digest {
		return true, nil
	}
	for _, l := range a.layers {
		if d == l.digest {
			return true, nil
		}
	}
	return false, nil
}

// Get streams the blob d names: the synthesized index or manifest, or the
// archive's config or a layer read straight from the tar.
func (a *LegacyArchive) Get(d Digest) (io.ReadCloser, int64, error) {
	switch d {
	case a.indexDigest:
		return io.NopCloser(bytes.NewReader(a.indexBytes)), int64(len(a.indexBytes)), nil
	case a.manifestDigest:
		return io.NopCloser(bytes.NewReader(a.manifestBytes)), int64(len(a.manifestBytes)), nil
	}
	if d == a.config.digest {
		return a.src.ReadEntry(a.config.entry)
	}
	for i := range a.layers {
		if d == a.layers[i].digest {
			return a.src.ReadEntry(a.layers[i].entry)
		}
	}
	return nil, 0, fmt.Errorf("oci: docker archive has no blob %s", d)
}

// parseLegacyManifest decodes a docker archive's manifest.json and requires
// exactly one image entry — the single-image case this store ingests.
func parseLegacyManifest(r io.Reader) ([]legacyImage, error) {
	var imgs []legacyImage
	if err := json.NewDecoder(r).Decode(&imgs); err != nil {
		return nil, err
	}
	if len(imgs) != 1 {
		return nil, fmt.Errorf("oci: docker archive has %d image(s); exactly one is required", len(imgs))
	}
	if imgs[0].Config == "" {
		return nil, fmt.Errorf("oci: docker archive manifest names no config")
	}
	return imgs, nil
}

// entryBytes reads the named entry of the tar.
func entryBytes(src *TarFileSource, name string) ([]byte, error) {
	r, _, err := src.ReadEntry(name)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// digestFromName recovers the sha256 digest hex encoded in a docker archive
// entry name — a config ("sha256:<hex>") or a layer ("<hex>.tar[.gz]"). The
// digest is a pure function of the name (and therefore of the blob's content),
// so it needs no access to the blob itself: strip the layer suffix, then take
// the hex after the final ":".
func digestFromName(name string) (hex string, ok bool) {
	hex = name
	for _, suf := range []string{".tar.gz", ".tgz", ".tar"} {
		if strings.HasSuffix(hex, suf) {
			hex = hex[:len(hex)-len(suf)]
			break
		}
	}
	if i := strings.LastIndex(hex, ":"); i >= 0 {
		return hex[i+1:], true
	}
	// A bare hex with no "algo:" prefix is also accepted.
	return hex, true
}

// layerDescriptors builds the manifest layer descriptors from the resolved
// layers, with the media type chosen from each entry name.
func layerDescriptors(layers []blobRef) []Descriptor {
	out := make([]Descriptor, 0, len(layers))
	for _, l := range layers {
		out = append(out, Descriptor{MediaType: layerMediaType(l.entry), Digest: l.digest, Size: l.size})
	}
	return out
}

// layerMediaType picks the OCI layer media type from an entry name: a
// `.tar.gz`/`.tgz` suffix means the layer is gzip-compressed.
func layerMediaType(name string) string {
	switch {
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		return MediaTypeImageLayerGzip
	default:
		return MediaTypeImageLayer
	}
}
