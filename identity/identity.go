// Package identity is the single point in this project that knows how a blob's
// key is spelled in the two formats we bridge: a go-multihash (the store's key)
// and a go-digest (the OCI digest, e.g. "sha256:<hex>").
//
// An Identity is a name for content, not a value we verify: for this store the
// bytes are carried, not checked, and the same content appears under the two
// spellings above. The store keeps its key as an opaque multihash; OCI names
// the same content by digest. identity translates between them so that neither
// side needs to know about the other — `storage` never touches go-digest, `oci`
// never touches go-multihash, and an OCI import of a byte sequence and a plain
// `add` of that same sequence land on the *same* store key and deduplicate for
// free.
//
// Only SHA-2 256 is understood today. Extending to another algorithm (SHA-3, a
// larger SHA-2 size, ...) is a change confined to this package: the bytes and
// the Identity type never change, only the mapping gains a case.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/multiformats/go-multihash"
	"github.com/opencontainers/go-digest"
)

// Identity is the store's key for content: an algorithm-qualified digest,
// carried opaquely (algorithm code + digest bytes). It is an alias of []byte —
// and therefore mutually assignable with storage.Digest, which is likewise a
// `= []byte` alias — so the bytes are shared, not copied; Identity is the
// *semantic* name for them.
type Identity = []byte

// ErrUnsupported is returned when a multihash or digest algorithm is outside
// what this store supports. Today that is exactly SHA-2 256; extend via the
// case sites noted below.
var ErrUnsupported = errors.New("identity: unsupported digest algorithm (only sha256 is supported)")

// HashReader returns the Identity of r's bytes: their SHA-2 256 multihash.
// This is the content→key direction used by a fresh import (`add`, `oci add`).
// It streams, so memory is O(1) regardless of content size (a blob may be MB,
// GB, or TB).
func HashReader(r io.Reader) (Identity, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return nil, err
	}
	return encode(h.Sum(nil), multihash.SHA2_256)
}

// FromOCI returns the Identity (multihash bytes) named by an OCI digest. Only
// sha256 digests are accepted; any other algorithm yields ErrUnsupported.
func FromOCI(d digest.Digest) (Identity, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if d.Algorithm() != digest.SHA256 {
		return nil, ErrUnsupported
	}
	raw, err := hex.DecodeString(d.Encoded())
	if err != nil {
		return nil, fmt.Errorf("identity: bad digest hex in %q: %w", d, err)
	}
	return encode(raw, multihash.SHA2_256)
}

// FromOCIString is FromOCI from a raw string: parse as an OCI digest and
// convert. The one-call bridge for the CLI argument path — a `sha256:<hex>`
// token in the shell becomes a store key.
func FromOCIString(s string) (Identity, error) {
	d, err := digest.Parse(s)
	if err != nil {
		return nil, err
	}
	return FromOCI(d)
}

// ToOCI returns the OCI digest ("sha256:<hex>") named by the Identity
// (multihash bytes). Only SHA-2 256 multihashes are accepted.
func ToOCI(id Identity) (digest.Digest, error) {
	decoded, err := multihash.Decode(id)
	if err != nil {
		return "", fmt.Errorf("identity: not a multihash: %w", err)
	}
	if decoded.Code != multihash.SHA2_256 {
		return "", ErrUnsupported
	}
	return digest.NewDigestFromBytes(digest.SHA256, decoded.Digest), nil
}

// DigestBytes extracts the inner SHA-2 256 digest — the 32 raw digest bytes,
// without the multihash algorithm/size prefix — from an Identity. It is the
// value a Content-Digest header or an OCI digest is built from.
func DigestBytes(id Identity) ([]byte, error) {
	decoded, err := multihash.Decode(id)
	if err != nil {
		return nil, fmt.Errorf("identity: not a multihash: %w", err)
	}
	if decoded.Code != multihash.SHA2_256 {
		return nil, ErrUnsupported
	}
	return decoded.Digest, nil
}

// Check streams r and reports whether its bytes produce the same SHA-2 256
// multihash as expected. It is the "verify" call used on import (we otherwise
// carry, not check) and on `get` to assert the backend returned the right blob.
func Check(r io.Reader, expected Identity) bool {
	id, err := HashReader(r)
	if err != nil {
		return false
	}
	return encodeEqual(id, expected)
}

// encode composes the multihash bytes (varint code, varint size, digest) from a
// raw digest and its algorithm code. For SHA-2 256 this is the 2-byte `12 20`
// prefix followed by the 32 digest bytes — the same bytes the `dir` backend
// renders as `12/20/...` on disk.
func encode(raw []byte, code uint64) (Identity, error) {
	mh, err := multihash.Encode(raw, code)
	if err != nil {
		return nil, err
	}
	return mh, nil
}

func encodeEqual(a, b Identity) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
