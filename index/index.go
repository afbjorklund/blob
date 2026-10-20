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
//     (no control characters, the encoded-segment length bound, native handles
//     reserved);
//   - Default derives the name to record from an argument (the basename,
//     .tar/.tgz/.tar.gz suffix stripped);
//   - Encode/Decode are the single source of truth for the clean-name ↔
//     encoded-segment step. The index stays a single flat level in every
//     backend (no index/ subdirectories, no nested S3 keys, no URL hierarchy),
//     so a name that would otherwise need path separators — a registry-style
//     name like `docker.io/library/busybox` or `busybox@sha256:…` — is stored
//     as an *encoded segment* and exposed as a *clean name*. Encode is the
//     identity on the names that were legal before it existed (no `/`, no `%`),
//     so existing stores are untouched until such a name first appears.
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
// '/' and '%' are legal name characters: they are the registry-style names
// this store is meant to hold (docker.io/library/busybox, busybox@sha256:…),
// and Encode turns them into a single flat segment so the backends never build
// a hierarchy. Control characters (\0 \n \r DEL) are rejected because no
// backend can hold them; the encoded segment length is bounded
// (maxSegmentLen) so a dir-backend name stays within one POSIX name.
//
// Returns (skip, err): skip=true means the blob is stored but no name entry
// is written (the name is already the blob's own direct address); a plain
// name is (false, nil) — record it.
func Check(nm Name, target identity.Identity) (skip bool, err error) {
	if nm == "" || nm == "." || nm == ".." {
		return false, fmt.Errorf("invalid index name %q", nm)
	}
	if seg := Encode(nm); len(seg) > maxSegmentLen {
		return false, fmt.Errorf("name %q encodes to a %d-byte segment; index names in this store must be at most %d bytes once encoded", nm, len(seg), maxSegmentLen)
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

// maxSegmentLen is the cap on the *encoded* form of a name (not on the clean
// name). Rationale: POSIX NAME_MAX is 255 on essentially every Linux
// filesystem (ext4, xfs, btrfs, zfs, f2fs, tmpfs), so the dir backend cannot
// host a longer single file entry anyway; 255 is also well within sql's TEXT,
// s3's 1024-byte key limit, and any URL segment. Enforced once in Check, so
// every backend is held to the same bound by the one package that owns the
// convention.
const maxSegmentLen = 255

// Encode is the clean name → encoded segment this store uses in its physical
// index entries (a dir filename, a sql column, an http URL path segment, an s3
// object key). It escapes '/' and '%' only — the two characters that are not
// flat — using the standard URI percent convention, so the result is a single
// filesystem/URL/key-safe segment and stays recognisable when eyeballed while
// debugging:
//
//	'/' -> %2F   (docker.io/library/busybox -> docker.io%2Flibrary%2Fbusybox)
//	'%' -> %%    (kept unambiguous on decode: the name a%2Fb != the name a/b)
//	other -> itself   (':' '@' '.' digits and letters are fine in every backend)
//
// It is byte-wise (the escape chars are ASCII, so multibyte UTF-8 never
// collides with an escape) and the identity on the names legal before it
// existed (no '/', no '%') — so pre-existing stores are unaffected until a '/'
// or '%' name first appears. It always round-trips exactly through Decode.
func Encode(nm Name) string {
	if !strings.ContainsAny(nm, "/%") {
		return nm
	}
	var b strings.Builder
	b.Grow(len(nm))
	for _, r := range nm {
		switch r {
		case '/':
			b.WriteString("%2F")
		case '%':
			b.WriteString("%%")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Decode is the tolerant encoded segment → clean name. It unescapes the only
// encodings Encode produces — %2F -> / and %% -> % — and passes every other
// byte through *literally*. In particular a '%' that does not begin %2F or %%
// is treated as a literal '%', which is deliberate: it keeps pre-escape stores
// (which may hold a raw '%' in a name, e.g. from before this step) readable
// after the upgrade, instead of turning such a store into a wall of
// "invalid name" errors.
//
// Decode(Encode(nm)) == nm for every name nm (strict round-trip); Decode is
// the one direction where two different segments can map to one name (a raw
// '%2F' stored before Encode existed decodes to 'a/b'), which is exactly the
// legacy-tolerance contract and is why Encode stays strict about which bytes
// it emits.
func Decode(seg string) Name {
	var b strings.Builder
	b.Grow(len(seg))
	for i := 0; i < len(seg); i++ {
		if seg[i] != '%' {
			b.WriteByte(seg[i])
			continue
		}
		if i+1 < len(seg) {
			switch seg[i+1] {
			case '2':
				if i+2 < len(seg) && seg[i+2] == 'F' {
					b.WriteByte('/')
					i += 2
					continue
				}
			case '%':
				b.WriteByte('%')
				i++
				continue
			}
		}
		// A '%' not followed by the exact escape: pass through literally.
		b.WriteByte('%')
	}
	return b.String()
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
