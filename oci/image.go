package oci

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrNotImageConfig is wrapped when a blob is not a recognizable OCI image
// config (not JSON, or JSON without a `rootfs` that names at least one
// diff_id). Callers distinguish "not an image config" from config-specific
// failures (missing layer, unsupported rootfs.type) with errors.Is.
var ErrNotImageConfig = errors.New("oci: not an image config (no rootfs.diff_ids)")

// Image is the result of canonicalizing an image config plus its reachable
// layers into the index + manifest pair they would form if we had authored
// them (see NewImage). It carries the config descriptor and each layer
// descriptor so a printer can show sizes and media types.
type Image struct {
	IndexBytes     []byte
	ManifestBytes  []byte
	IndexDigest    Digest
	ManifestDigest Digest
	Config         Descriptor
	Layers         []Descriptor
}

// NewImage builds the canonical index + manifest pair for an image whose
// config blob is configDigest (of configSize bytes, media type
// configMediaType) and whose layers are the given descriptors (each
// MediaType + Digest + Size, in image order). The result is a pure function
// of the inputs: no bytes are read, the config and layers are not re-encoded
// or re-hashed.
//
// A config with zero layers yields a manifest that references the config
// only — the shape docker and podman accept for an empty rootfs. A layer
// whose MediaType is empty is defaulted to MediaTypeImageLayer (the common
// docker/podman save shape). A zero configMediaType defaults to
// MediaTypeImageConfig.
func NewImage(
	configDigest Digest, configSize int64, configMediaType string,
	layers []Descriptor,
) (*Image, error) {
	if configSize <= 0 {
		return nil, &ImageSizeError{Size: configSize}
	}
	if configMediaType == "" {
		configMediaType = MediaTypeImageConfig
	}
	out := make([]Descriptor, len(layers))
	for i := range layers {
		if layers[i].MediaType == "" {
			layers[i].MediaType = MediaTypeImageLayer
		}
		out[i] = layers[i]
	}
	m := Manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeImageManifest,
		Config:        Descriptor{MediaType: configMediaType, Digest: configDigest, Size: configSize},
		Layers:        out,
	}
	mb, err := Marshal(&m)
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
	return &Image{
		IndexBytes:     ib,
		ManifestBytes:  mb,
		IndexDigest:    DigestOf(ib),
		ManifestDigest: md,
		Config:         Descriptor{MediaType: configMediaType, Digest: configDigest, Size: configSize},
		Layers:         out,
	}, nil
}

// ImageSizeError is returned when configSize is non-positive.
type ImageSizeError struct{ Size int64 }

func (e *ImageSizeError) Error() string {
	return fmt.Sprintf("oci: non-positive config size: %d", e.Size)
}

// Config is the subset of an OCI image config this package needs: the OS and
// the rootfs (layer type + diff_ids). Other config fields (history, created,
// author, ...) are ignored — the image this package rebuilds references the
// config blob as an opaque unit, and what matters for its layers is only the
// rootfs.
type Config struct {
	OS     string `json:"os"`
	Rootfs *struct {
		Type    string   `json:"type"`
		DiffIDs []Digest `json:"diff_ids"`
	} `json:"rootfs"`
}

// ParseConfig decodes an OCI image config and returns the subset this
// package uses: the OS and the rootfs (its type and diff_ids). A config with
// no rootfs (or a rootfs with no diff_ids) is not an image config for our
// purposes — callers that want to try it and fall through on "not a config"
// match on ErrNotImageConfig with errors.Is. The rootfs.type is not
// validated here; the caller resolves it to a media type with LayerMediaType.
func ParseConfig(r io.Reader) (*Config, error) {
	var c Config
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotImageConfig, err)
	}
	if c.Rootfs == nil || len(c.Rootfs.DiffIDs) == 0 {
		return nil, fmt.Errorf("%w: no rootfs.diff_ids", ErrNotImageConfig)
	}
	for i, d := range c.Rootfs.DiffIDs {
		if d == "" {
			return nil, fmt.Errorf("oci: config rootfs.diff_ids[%d] is empty", i)
		}
	}
	return &c, nil
}

// LayerMediaType maps a config's `rootfs.type` to the layer media type a
// manifest would carry for its layers. The default ("") and "layers" map to
// MediaTypeImageLayer (the common shape: docker and podman `docker save`);
// "layers.gzip" maps to MediaTypeImageLayerGzip. Other values return an
// error — callers must not silently guess.
func LayerMediaType(rootfsType string) (string, error) {
	switch rootfsType {
	case "", "layers":
		return MediaTypeImageLayer, nil
	case "layers.gzip":
		return MediaTypeImageLayerGzip, nil
	default:
		return "", fmt.Errorf("oci: unsupported rootfs.type %q (want \"layers\" or \"layers.gzip\")", rootfsType)
	}
}
