// Package oci adds OCI import and export to the content-addressable store.
//
// It speaks the OCI Image Layout specification — a directory (or tar)
// containing `oci-layout`, `index.json`, and `blobs/<algorithm>/<encoded>` —
// and addresses every blob by its OCI digest (go-digest).
//
// The package is pure: its public surface is built on `digest.Digest` and
// `io`, and it never names a multihash or a CID. The store keeps its own key
// (an identity.Identity); main adapts a storage backend to the Source and Sink
// interfaces here and bridges digests to identities with the identity package,
// so an import and an `add` of the same bytes deduplicate for free. Blobs are
// streamed end-to-end (a layer may be MB, GB, or TB); only index.json and the
// manifest are small enough to be buffered.
//
// Only the modern OCI format is accepted: the output of
// `podman save -f oci-archive|oci-dir`, `skopeo ... oci-archive:`, and a v25+
// `docker save` "hybrid". The legacy docker-archive fields a hybrid carries
// alongside the OCI half (`manifest.json`, `repositories`) are ignored — import
// walks from index.json and stores only the reachable blobs.
package oci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	godigest "github.com/opencontainers/go-digest"
)

// Digest is an OCI content digest: an algorithm and a lowercase hex value, for
// example `sha256:<64 hex chars>`. It is an alias of go-digest's Digest, so the
// surface reads oci.Digest while accepting a plain value. Only sha256 appears in
// practice; this package does not special-case any other algorithm.
type Digest = godigest.Digest

// OCI media types this package recognises while traversing a layout.
const (
	MediaTypeImageIndex     = "application/vnd.oci.image.index.v1+json"
	MediaTypeImageManifest  = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeImageConfig    = "application/vnd.oci.image.config.v1+json"
	MediaTypeImageLayer     = "application/vnd.oci.image.layer.v1.tar"
	MediaTypeImageLayerGzip = "application/vnd.oci.image.layer.v1.tar+gzip"
	// MediaTypeUnknownConfig is the config media type for a bare artifact whose
	// config is `{}` — semantically "unknown config", per the OCI distribution
	// spec (not "empty", which means the content is intentionally null).
	MediaTypeUnknownConfig = "application/vnd.unknown.config.v1+json"
	// MediaTypeGenericBinary is the generic layer media type.
	MediaTypeGenericBinary = "application/octet-stream"

	// MediaTypeSifConfig and MediaTypeSifLayer are the SIF (Apptainer /
	// Singularity) media types — the one recognized special case when wrapping
	// a bare file; everything else is generic.
	MediaTypeSifConfig = "application/vnd.sylabs.sif.config.v1+json"
	MediaTypeSifLayer  = "application/vnd.sylabs.sif.layer.v1.sif"

	// LayoutVersion is the `imageLayoutVersion` value written in `oci-layout`.
	LayoutVersion = "1.0.0"
)

// SifMagic is the first 10 bytes of any SIF file (see apptainer/SIF.h).
// A var (not a const) because a []byte literal is not a constant.
var SifMagic = []byte("SIF_MAGIC\x00")

// MediaTypesFor returns the (config, layer) media types for a bare file wrapped
// as a single-layer artifact, from its leading bytes: the SIF magic → the SIF
// types, anything else → unknown config + generic binary. This mirrors the
// store's own two-branch content rule (json vs raw); SIF is the one recognized
// special case, the rest is generic. A head shorter than the magic is non-SIF.
func MediaTypesFor(head []byte) (config, layer string) {
	if len(head) >= len(SifMagic) && bytes.Equal(head[:len(SifMagic)], SifMagic) {
		return MediaTypeSifConfig, MediaTypeSifLayer
	}
	return MediaTypeUnknownConfig, MediaTypeGenericBinary
}

// Source is a read side: it supplies blob contents by OCI digest. Import
// implements it over a layout (dir or tar); export implements it over the
// store. Get streams; the caller must Close the returned reader.
type Source interface {
	// Has reports whether the digest is present.
	Has(d Digest) (bool, error)
	// Get streams the blob named by d (its size is the second value).
	Get(d Digest) (io.ReadCloser, int64, error)
}

// Sink is a write side: it stores blob contents by OCI digest. Import
// implements it over the store; export implements it over a layout (dir or
// tar). Put streams.
type Sink interface {
	// Has reports whether the digest is already stored (used to skip a
	// re-write during import, so the store deduplicates).
	Has(d Digest) (bool, error)
	// Put stores r (size bytes) under d.
	Put(d Digest, size int64, r io.Reader) error
	// PutIndex stores the layout's index.json (indexBytes) — to the layout
	// root for a layout sink, or to the store under its own digest for a store
	// sink. Called at most once per Copy.
	PutIndex(indexBytes []byte) error
}

// Descriptor is one entry of an index or manifest: media type, digest, size, and
// optional annotations.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      Digest            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Manifest is one image or artifact manifest. An image references a config and
// zero or more layers. An artifact (the single-file import case) references the
// empty config and a single layer, and additionally sets ArtifactType.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// Index is the root of a layout: the manifest descriptors it holds. A layout
// this package writes always holds at least one.
type Index struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Manifests     []Descriptor      `json:"manifests"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// GuessContentType returns the media type of a blob from its first byte — JSON
// for '{' or '[', octet-stream otherwise — the same content-not-extension rule
// the store uses for its JSON/Raw codec. Used when a caller does not specify a
// media type for an artifact layer.
func GuessContentType(first byte) string {
	if first == '{' || first == '[' {
		return "application/json"
	}
	return "application/octet-stream"
}

// DigestOf returns the digest (sha256) of the given bytes.
func DigestOf(b []byte) Digest { return godigest.FromBytes(b) }

// Marshal is the compact JSON encoding used for index.json, manifests, and
// configs.
func Marshal(v any) ([]byte, error) { return json.Marshal(v) }

// ParseIndex decodes an index.json blob and checks it names at least one manifest.
func ParseIndex(r io.Reader) (*Index, error) {
	var idx Index
	if err := json.NewDecoder(r).Decode(&idx); err != nil {
		return nil, err
	}
	if idx.MediaType != "" && idx.MediaType != MediaTypeImageIndex {
		return nil, fmt.Errorf("oci: not an image index: mediaType %q", idx.MediaType)
	}
	if len(idx.Manifests) == 0 {
		return nil, fmt.Errorf("oci: index names no manifests")
	}
	return &idx, nil
}

// ParseManifest decodes a manifest blob and checks it references a config and/or
// layer.
func ParseManifest(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, err
	}
	if m.Config.Digest == "" && len(m.Layers) == 0 {
		return nil, fmt.Errorf("oci: manifest references no config or layer")
	}
	return &m, nil
}

// Refs are the descriptors a manifest references: its config and its layers.
func (m *Manifest) Refs() []Descriptor {
	if m == nil {
		return nil
	}
	out := make([]Descriptor, 0, 1+len(m.Layers))
	if m.Config.Digest != "" {
		out = append(out, m.Config)
	}
	return append(out, m.Layers...)
}

// Reachable returns the layout's reachable blobs: the index itself first, then
// each manifest, then each manifest's config and layers, in listing order,
// deduplicated. `indexBytes` is the index.json blob. `src` supplies manifest
// blobs. Unreferenced blobs (e.g. the extra docker-archive entries a hybrid
// carries) are not returned, so a consumer imports/exports exactly the reachable
// set. The index digest is reachable[0].
func Reachable(src Source, indexBytes []byte) ([]Digest, error) {
	idx, err := ParseIndex(bytes.NewReader(indexBytes))
	if err != nil {
		return nil, err
	}

	idxDigest := DigestOf(indexBytes)
	out := []Digest{idxDigest}
	seen := map[Digest]bool{idxDigest: true}
	add := func(d Digest) {
		if d == "" {
			return
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}

	for _, ref := range idx.Manifests {
		add(ref.Digest)
		reader, _, err := src.Get(ref.Digest)
		if err != nil {
			return nil, err
		}
		mf, err := ParseManifest(reader)
		_ = reader.Close()
		if err != nil {
			return nil, err
		}
		for _, ref := range mf.Refs() {
			add(ref.Digest)
		}
	}

	return out, nil
}

// Copy moves every reachable blob from src to sink: first the index (via
// sink.PutIndex with indexBytes), then each reachable manifest, config, and
// layer (via sink.Put, streamed from src.Get). A digest already present in the
// sink is skipped, so a store sink deduplicates. Returns the index digest and
// the ordered reachable digests (index first) that were planned.
func Copy(src Source, sink Sink, indexBytes []byte) (Digest, []Digest, error) {
	reachable, err := Reachable(src, indexBytes)
	if err != nil {
		return "", nil, err
	}
	indexDigest := reachable[0]

	if err := sink.PutIndex(indexBytes); err != nil {
		return "", nil, err
	}

	for _, d := range reachable[1:] {
		have, err := sink.Has(d)
		if err != nil {
			return "", nil, err
		}
		if have {
			continue
		}
		reader, size, err := src.Get(d)
		if err != nil {
			return "", nil, err
		}
		err = sink.Put(d, size, reader)
		_ = reader.Close()
		if err != nil {
			return "", nil, err
		}
	}

	return indexDigest, reachable, nil
}
