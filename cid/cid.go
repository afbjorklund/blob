// Package cid is this store's thin facade over go-cid (and its friends
// go-multibase, go-multicodec, go-multihash). It fixes the store's CID
// conventions in one place so the rest of the program (main, the gateway, the
// OCI import/export path) never names go-cid directly:
//
//   - Cid is an alias of go-cid's Cid (so the value type and its methods,
//     .Hash(), .Type(), ..., are unchanged — we only add conventions around it);
//   - Render is the single point where the user-facing base is chosen: base32hex
//     (a multibase alphabet whose first 16 letters are base16hex), so CIDs read
//     like an extension of hex rather than IPFS's default base32;
//   - the content-based codec rule (Json iff the blob's first byte is '{' or '[',
//     else Raw) and the set of supported codecs (raw, json) live here.
//
// The storage key is a multihash — an identity.Identity. A Cid is that key plus
// a codec. go-cid is imported locally as gocid so the package's surface reads
// cid.Cid, cid.Build, ...
package cid

import (
	"encoding/base64"
	"fmt"

	"blob/identity"

	gocid "github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
	godigest "github.com/opencontainers/go-digest"
)

// Code is a multicodec value, exposed so callers do not need to import
// go-multicodec directly.
type Code = multicodec.Code

// CodeRaw and CodeJson are the two codecs the store holds; CodeDagPb (IPFS's
// default codec, `dag-pb`) is one the store *knows* — a CID of it may name a
// blob (it is the "ipfs get → blob add" interop handle) but is not a direct
// address, since the store does not hold it.
const (
	CodeRaw   = multicodec.Raw
	CodeJson  = multicodec.Json
	CodeDagPb = multicodec.DagPb
)

// Cid is a content identifier: a codec + a multihash, carrying no base (the
// base is a render-time parameter, see Render). It is an alias of go-cid's Cid,
// so values passed to and from go-cid need no conversion.
type Cid = gocid.Cid

// Build composes a CIDv1 from a codec and the multihash key (an identity). The
// key is already the algorithm-qualified digest, so nothing is hashed here.
func Build(mc multicodec.Code, key identity.Identity) Cid {
	return gocid.NewCidV1(uint64(mc), multihash.Multihash(key))
}

// Parse parses a user-facing CID string (base32hex, as produced by Render — go-cid
// accepts any multibase).
func Parse(s string) (Cid, error) {
	return gocid.Parse(s)
}

// Render renders a Cid in this store's user-facing base: base32hex. This is the
// single point where the presentation base is chosen (see the package doc).
func Render(c Cid) (string, error) {
	return c.StringOfBase(multibase.Base32hex)
}

// Key returns the multihash (algorithm + digest bytes) inside a Cid — the
// storage key. Since both are aliases of []byte this is a no-allocation cast.
func Key(c Cid) identity.Identity {
	return identity.Identity(c.Hash())
}

// Codec returns the multicodec of a Cid.
func Codec(c Cid) multicodec.Code {
	return multicodec.Code(c.Type())
}

// CodecName renders a multicodec as its conventional name ("raw", "json", ...).
func CodecName(code multicodec.Code) string {
	return code.String()
}

// Supported reports whether the store holds a Cid of the given codec. Today that
// is raw and json (the content-based split). Adding a codec is a change here and
// in identity — nothing in storage.
func Supported(code multicodec.Code) bool {
	switch code {
	case multicodec.Raw, multicodec.Json:
		return true
	}
	return false
}

// CodecForFirstByte returns the content-based codec for a blob's first byte —
// Json for '{' or '[', Raw otherwise (0-for-empty guards the not-expected case).
// This is the rule `add` stores by and `lookup` re-derives by peeking.
func CodecForFirstByte(first byte) multicodec.Code {
	if first == '{' || first == '[' {
		return multicodec.Json
	}
	return multicodec.Raw
}

// MediaType is the Content-Type a Cid's content is served as: application/json
// for the Json codec, application/octet-stream otherwise.
func MediaType(c Cid) string {
	if Codec(c) == multicodec.Json {
		return "application/json"
	}
	return "application/octet-stream"
}

// ErrUnsupportedCodec is returned when a Cid's codec is outside what the store
// holds.
type ErrUnsupportedCodec struct{ Code Code }

func (e ErrUnsupportedCodec) Error() string {
	return fmt.Sprintf("cid: unsupported codec %q (this store holds raw and json)", CodecName(e.Code))
}

// DigestFromCid returns the CID's content as an OCI digest (a godigest.Digest,
// e.g. "sha256:<hex>"). It is the single point where a Cid is spelled in OCI
// terms — used by the import/export adapters to bridge a store key to an OCI
// layout entry name. Err if the codec is unsupported.
func DigestFromCid(c Cid) (godigest.Digest, error) {
	if !Supported(Codec(c)) {
		return "", ErrUnsupportedCodec{Code: Codec(c)}
	}
	return identity.ToOCI(Key(c))
}

// ContentDigestHeader returns the Content-Digest header value for a Cid:
// "sha-256=<base64(inner digest)>", the form the HTTP gateway sends.
func ContentDigestHeader(c Cid) (string, error) {
	inner, err := identity.DigestBytes(Key(c))
	if err != nil {
		return "", err
	}
	return "sha-256=" + base64.StdEncoding.EncodeToString(inner), nil
}
