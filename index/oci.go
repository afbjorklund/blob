package index

import (
	"fmt"
	"strings"
)

// OciPrefix is the first path component of an OCI upstream reference name.
// It marks the name as a provenance record (not a free label, not a direct
// handle), and groups every such entry under a single top directory in the
// mount surface ("~/.blob-mount/oci/") and in the /name/ HTTP surface.
//
// The rest of the name carries the platform (os/arch) and the upstream
// image-index digest (algorithm/hex). Both are foreign identifiers for the
// store; we do not store the upstream index itself, only its digest.
const OciPrefix = "oci"

// sha256 is the only digest algorithm this store bridges (identity.FromOCI).
// Upstream reference names are recorded under this algorithm component only.
const OciDigestAlgo = "sha256"

// IsOciRef reports whether nm is an OCI upstream reference name (starts with
// "oci/"). This is a shape check only — it does not validate the platform or
// digest components; use ParseOciRefName for that.
func IsOciRef(nm string) bool {
	return nm == OciPrefix || strings.HasPrefix(nm, OciPrefix+"/")
}

// ParseOciRefName splits an OCI upstream reference name into its four
// components: the OS, the architecture, the digest algorithm, and the digest
// hex (all non-empty; the hex is lowercase hex — the digest of the upstream
// OCI image index).
//
// The expected shape is:
//
//	oci/<os>/<arch>/<algo>/<hex>
//
// For example "oci/linux/amd64/sha256/3f4d6b32e267".
//
// This is a *shape* check for the resolver and write-path validation. It does
// not reject a non-sha256 algorithm — the name shape allows any algorithm
// component — so a store could name such a reference even though
// NormalizeOciDigest (the write path) would not. That distinction is
// intentional: the shape is a record, the write path is a gate.
//
// ok is false for any shape that does not match exactly — the caller can then
// treat the name as an ordinary free label (which `Check` would accept if not
// intercepted, or reject if it pointed at another blob's digest).
func ParseOciRefName(nm string) (os, arch, algo, hex string, ok bool) {
	if !strings.HasPrefix(nm, OciPrefix+"/") {
		return "", "", "", "", false
	}
	rest := nm[len(OciPrefix)+1:] // strip "oci/"
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return "", "", "", "", false
	}
	if !isLowerHex(parts[3]) {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[2], parts[3], true
}

// BuildOciName composes the canonical OCI upstream reference name from its
// components. It is the single point where the shape is fixed on the write
// path, so a name recorded by `add` and one recorded by `oci import` are
// byte-identical for the same (os, arch, algo, hex).
func BuildOciName(os, arch, algo, hex string) string {
	return OciPrefix + "/" + os + "/" + arch + "/" + algo + "/" + hex
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
