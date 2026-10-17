package oci

import "fmt"

// ConfigMediaType, LayerMediaType, and ArtifactType are the media types the
// manifest will use, so a caller (or a test) can inspect them.
type Artifact struct {
	IndexBytes     []byte
	ManifestBytes  []byte
	IndexDigest    Digest
	ManifestDigest Digest
	LayerDigest    Digest
	ConfigDigest   Digest // digest of `{}`

	ConfigMediaType string
	LayerMediaType  string
	ArtifactType    string
}

// NewArtifact builds the index + manifest pair for a single-layer artifact.
// layerDigest and layerSize are supplied by the caller (streamed, not
// buffered). layerMediaType and artifactType may be ""; configMediaType is
// always set here (the caller supplies it via Classify or explicitly).
func NewArtifact(
	layerDigest Digest, layerSize int64,
	layerMediaType, artifactType, configMediaType string,
) (*Artifact, error) {
	if layerSize <= 0 {
		return nil, &ArtifactSizeError{Size: layerSize}
	}
	if layerMediaType == "" {
		layerMediaType = MediaTypeGenericBinary
	}
	if artifactType == "" {
		artifactType = layerMediaType
	}
	if configMediaType == "" {
		configMediaType = MediaTypeUnknownConfig
	}

	const emptyConfig = `{}`
	cfgD := DigestOf([]byte(emptyConfig))

	m := Manifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeImageManifest,
		ArtifactType:  artifactType,
		Config:        Descriptor{MediaType: configMediaType, Digest: cfgD, Size: int64(len(emptyConfig))},
		Layers:        []Descriptor{{MediaType: layerMediaType, Digest: layerDigest, Size: layerSize}},
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

	return &Artifact{
		IndexBytes:      ib,
		ManifestBytes:   mb,
		IndexDigest:     DigestOf(ib),
		ManifestDigest:  md,
		LayerDigest:     layerDigest,
		ConfigDigest:    cfgD,
		ConfigMediaType: configMediaType,
		LayerMediaType:  layerMediaType,
		ArtifactType:    artifactType,
	}, nil
}

// EmptyConfig is the canonical `{}` config every bare artifact shares — the
// same 2 bytes `podman save` writes.
var EmptyConfig = []byte(`{}`)

// ArtifactSizeError is returned when layerSize is non-positive.
type ArtifactSizeError struct{ Size int64 }

func (e *ArtifactSizeError) Error() string {
	return fmt.Sprintf("oci: non-positive layer size: %d", e.Size)
}
