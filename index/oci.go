package index

import (
	"fmt"
	"strings"
)

// OciPrefix is the first path component of an OCI reference name. It marks the
// name as a provenance record (not a free label, not a direct handle), and
// groups every such entry under a single top directory in the mount surface
// ("~/.blob-mount/oci/") and in the /name/ HTTP surface.
//
// The rest of the name carries the variant (what the leaf names), the
// platform (os/arch), and the upstream digest (algorithm/hex). The leaf is a
// content address held in this store; the digest and platform are foreign
// identifiers the store carries as labels, not things it resolves against.
const OciPrefix = "oci"

// OciVariants are the values the variant component takes. Each names a
// different *kind* of leaf for the same (upstream digest, platform), so they
// are separate path components rather than one last-write-wins name:
//
//   - blob: a blob — the raw file bytes (a SIF, a tar, a layer, a config).
//     The main purpose: serve a ready file to a client (apptainer's
//     --basepath target). Determined by the input shape: `oci import` of a
//     bare file records here.
//   - json: the index/manifest JSON — the entrypoint a client (or `oci ls`)
//     peeks at to discover what else a layout references. Secondary, for
//     introspection. Determined by the input shape: `oci import` of a layout
//     records here. An OCI "image" and an OCI "artifact" are the same
//     structure (an image.index / image.manifest JSON), so they share this
//     one value; the distinction is what the producer meant, not something
//     the store can or should assert.
//
// The component is a naming convention the *writer* picks from the input
// shape; the store itself never branches on it. The codec is always derived
// from content (first byte), so a JSON leaf under blob/ or a raw leaf under
// json/ still resolves and serves correctly — the directory name is metadata
// for humans, not a claim the store enforces.
const (
	OciVariantBlob = "blob"
	OciVariantJson = "json"
)

// OciDigestAlgo is the only digest algorithm this store bridges (via
// identity.FromOCI). Reference names record it as a path component, so the
// set of supported algorithms can grow without a name-shape change.
const OciDigestAlgo = "sha256"

// OciLevels is the number of components after the "oci/" prefix:
// variant, os, arch, algo, hex. The FUSE tree walks one level per component.
const OciLevels = 5

// IsOciRef reports whether nm is an OCI reference name (starts with "oci/").
// This is a shape check only — it does not validate the components; use
// ParseOciRefName for that.
func IsOciRef(nm string) bool {
	return strings.HasPrefix(nm, OciPrefix+"/")
}

// ParseOciRefName splits an OCI reference name into its five components:
// variant, OS, architecture, digest algorithm, and digest hex (all non-empty;
// the hex is lowercase hex — the upstream digest of record).
//
// The expected shape is:
//
//	oci/<variant>/<os>/<arch>/<algo>/<hex>
//
// For example "oci/blob/linux/amd64/sha256/3f4d6b32e267".
//
// This is a *shape* check for the resolver and the FUSE tree; it does not
// reject an unknown variant or a non-sha256 algorithm (the shape allows any
// non-empty components), so a store could carry a new variant or algorithm.
// The write path (NormalizeOciDigest) is the gate that keeps the digest
// closed today.
//
// ok is false for any shape that does not match exactly — the caller can then
// treat the name as an ordinary free label.
func ParseOciRefName(nm string) (variant, os, arch, algo, hex string, ok bool) {
	if !strings.HasPrefix(nm, OciPrefix+"/") {
		return "", "", "", "", "", false
	}
	rest := nm[len(OciPrefix)+1:] // strip "oci/"
	parts := strings.Split(rest, "/")
	if len(parts) != OciLevels {
		return "", "", "", "", "", false
	}
	for _, p := range parts {
		if p == "" {
			return "", "", "", "", "", false
		}
	}
	if !isLowerHex(parts[OciLevels-1]) {
		return "", "", "", "", "", false
	}
	return parts[0], parts[1], parts[2], parts[3], parts[4], true
}

// BuildOciName composes the canonical OCI reference name from its components.
// It is the single point where the shape is fixed on the write path, so a
// name recorded by `add` and one by `oci import` are byte-identical for the
// same (variant, os, arch, algo, hex).
func BuildOciName(variant, os, arch, algo, hex string) string {
	return OciPrefix + "/" + variant + "/" + os + "/" + arch + "/" + algo + "/" + hex
}

// NormalizeOciDigest accepts an upstream digest in the forms users type —
// "sha256:<hex>" (the OCI digest spelling) or a bare lowercase hex — and
// splits it into (algorithm, hex). Only sha256 is accepted: it is the sole
// algorithm this store bridges (identity.FromOCI), and the "reject at import"
// decision keeps the name table free of digests the store cannot bridge. Any
// other algorithm, an empty piece, or a non-hex character is an error.
func NormalizeOciDigest(d string) (algo, hex string, err error) {
	if d == "" {
		return "", "", fmt.Errorf("empty OCI digest")
	}
	if strings.Contains(d, ":") {
		i := strings.IndexByte(d, ':')
		a, h := d[:i], d[i+1:]
		a = strings.ToLower(a)
		h = strings.ToLower(h)
		if a != OciDigestAlgo {
			return "", "", fmt.Errorf("unsupported digest algorithm %q in %q (only %q is accepted)", a, d, OciDigestAlgo)
		}
		if h == "" || !isLowerHex(h) {
			return "", "", fmt.Errorf("bad digest hex in %q", d)
		}
		return a, h, nil
	}
	// A bare hex (no "algo:" prefix) is assumed to be sha256 — the default in
	// every tool we interop with (docker, podman, skopeo) and the only one we
	// bridge.
	h := strings.ToLower(d)
	if !isLowerHex(h) {
		return "", "", fmt.Errorf("bad digest hex in %q", d)
	}
	return OciDigestAlgo, h, nil
}

// NormalizePlatform accepts an OCI platform in the forms users type —
// "os/arch" (e.g. "linux/amd64") or a bare arch (e.g. "amd64", which is
// normalized to "linux/amd64"). The os defaults to "linux" because this store
// today holds only linux images; the shape allows any os, so a non-linux
// reference can be recorded and, later, resolved without a format change.
func NormalizePlatform(p string) (os, arch string, err error) {
	if p == "" {
		return "", "", fmt.Errorf("empty platform")
	}
	if i := strings.IndexByte(p, '/'); i >= 0 {
		o, a := p[:i], p[i+1:]
		if o == "" || a == "" {
			return "", "", fmt.Errorf("bad platform %q (want os/arch)", p)
		}
		return o, a, nil
	}
	return "linux", p, nil
}

func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !isLowerHexByte(c) {
			return false
		}
	}
	return true
}

// isLowerHexByte reports whether c is a lowercase hex digit.
func isLowerHexByte(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}
