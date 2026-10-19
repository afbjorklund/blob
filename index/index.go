// Package index is this store's thin conventions around the name index — the
// separate per-name "name → blob key" mapping that sits beside the CAS (a name
// is user metadata pointing at a blob, not a spelling of the blob's key, which
// is why it lives here rather than in identity). Like the other thin packages
// it fixes the conventions in one place so the callers (main, daemon) never
// re-derive them:
//
//   - Name is an alias of string — a name carries no type of its own, exactly
//     as identity.Identity is an alias of []byte and cid.Cid is an alias of
//     go-cid's Cid; we only add conventions around the underlying value;
//   - Check is the single place where the recordability rule is decided
//     (flat, no control characters, native handles reserved);
//   - Default derives the name to record from an argument (the basename,
//     .tar/.tgz/.tar.gz suffix stripped).
//
// Names are flat today: no '/' (it would be a hierarchy: index/ subdirectories
// for dir, nested object keys for s3, path segments for http). This package
// is where the clean-name ↔ encoded-segment step for registry-style names is
// added next (see docs/names.md), so the backends keep the index a single
// level while users may name a blob `docker.io/library/busybox`.
package index

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"blob/cid"
	"blob/identity"
)

// Name is a human-readable label for a blob in the store — an alias of
// string, so a Name is a plain string and index only carries the conventions
// around it (recordability, derivation) rather than its value.
type Name = string

// Check decides what happens when the index is about to record `nm`, which
// resolves to the blob `target`. The invariant behind the rule: a name that
// *looks* like a native handle (an OCI digest the store converts, or a CID of
// a codec the store holds) is intercepted by the resolution order (CID →
// sha256-digest → name) before the name index is ever consulted — a name
// entry for it would be unreachable and misleading. So: such a name may be
// recorded only if it resolves to exactly the blob being stored (and then it
// is *skipped*, since the entry would be redundant with the direct address);
// in every other form it is rejected. Names that the resolver will never
// intercept — sha256 digests of other algorithms ("sha1:" etc., which no
// resolver path converts) and CIDs for codecs the store doesn't hold (e.g. a
// dag-pb CID, cid.CodeDagPb, from `ipfs add`: the "ipfs get → blob add"
// interop handle) — are allowed to record as ordinary names.
//
// Separately: '/' is not a legal name character yet (it would make a
// hierarchy: index/ subdirectories for dir, nested object keys for s3, path
// segments for http — the FQDN-name story, deferred, see docs/names.md);
// control characters (\0 \n \r DEL) are rejected because no backend can hold
// them.
//
// Returns (skip, err): skip=true means the blob is stored but no name entry
// is written (the name is already the blob's own direct address); a plain
// name is (false, nil) — record it.
func Check(nm Name, target identity.Identity) (skip bool, err error) {
	if nm == "" || nm == "." || nm == ".." {
		return false, fmt.Errorf("invalid index name %q", nm)
	}
	if strings.ContainsRune(nm, '/') {
		return false, fmt.Errorf("name %q contains a path separator; names in the index are flat (one level) — FQDN-style names need escaping that is not implemented yet", nm)
	}
	for _, c := range nm {
		if c == 0 || c == '\n' || c == '\r' || c == 0x7f {
			return false, fmt.Errorf("name %q contains a control character; names in the index cannot hold \\0, \\n, \\r or DEL", nm)
		}
	}
	if id, err := identity.FromOCIString(nm); err == nil {
		if bytes.Equal(id, target) {
			return true, nil
		}
		return false, fmt.Errorf("%q is an OCI digest of another blob; a digest-shaped name is reserved as a direct handle and cannot point elsewhere", nm)
	}
	if c, err := cid.Parse(nm); err == nil {
		if cid.Supported(cid.Codec(c)) {
			if bytes.Equal(cid.Key(c), target) {
				return true, nil
			}
			return false, fmt.Errorf("%q is a CID for a codec this store holds; the resolver intercepts it as that CID, so it cannot also be a name pointing elsewhere", nm)
		}
		// A CID for a codec the store does not hold (e.g. dag-pb, the
		// cid.CodeDagPb code IPFS uses): the resolver does not intercept it, so
		// it may be recorded as a name — the cross-store handle (see the
		// function doc). `get`/`oci ls` of such a string resolves through the
		// name index and cross-checks the digest.
	}
	return false, nil
}

// Default derives the index name to record from an argument path: its
// basename, with a .tar/.tgz/.tar.gz suffix stripped when present.
func Default(arg string) Name {
	base := filepath.Base(arg)
	for _, suffix := range []string{".tar.gz", ".tar", ".tgz"} {
		base = strings.TrimSuffix(base, suffix)
	}
	return base
}
