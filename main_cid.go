package main

import (
	"encoding/hex"
	"fmt"

	"blob/cid"
	"blob/storage"

	"github.com/multiformats/go-multibase"
	mh "github.com/multiformats/go-multihash"
	"github.com/spf13/cobra"
)

// `blob cid <cid|name>` is the CLI inspector. First line: the CID itself —
// the very string you entered when the argument was a CID, or the CID that
// `lookup` would return for a name. Below that (separated by a blank line)
// the same CID read in as a breakdown: multibase, version, codec, multihash,
// digest — the four CIDv1 parts per the spec, plus the raw digest in
// lowercase base16. Last line: whether this store holds the codec.
//
// A CID argument is a pure decode — no backend, any multibase, any version,
// any codec (a dag-pb CID straight from `ipfs add` inspects fine, and the
// store line notes the codec is not held by this store). A name argument
// does one Peek through the read-only backend to derive the content-based
// codec (same rule as `lookup`).
// Every multiformat spelling comes from the go-multibase / go-multicodec /
// go-multihash tables — none are hard-coded.
func runCid(cmd *cobra.Command, args []string) error {
	arg := args[0]

	var c cid.Cid
	var echo string
	if p, err := cid.Parse(arg); err == nil {
		c = p
		echo = arg
	} else {
		backend, err := getStorageBackend(storage.ReadOnly)
		if err != nil {
			return err
		}
		defer backend.Close()

		d, err := backend.Lookup(arg)
		if err != nil {
			return fmt.Errorf("%q: not a CID or a name recorded in the index", arg)
		}
		c, err = cidForDigest(backend, d)
		if err != nil {
			return err
		}
		echo, err = cid.Render(c)
		if err != nil {
			return err
		}
	}

	d, err := mh.Decode(mh.Multihash(c.Hash()))
	if err != nil {
		return err
	}

	codec := cid.Codec(c)
	codecName := cid.CodecName(codec)
	mhName, ok := mh.Codes[d.Code]
	if !ok {
		mhName = fmt.Sprintf("0x%x", d.Code)
	}
	digestHex := hex.EncodeToString(d.Digest)

	mbPrefix, mbName := string(echo[0]), "implicit (CIDv0, base58btc)"
	if len(echo) >= 1 {
		if n, ok := multibase.EncodingToStr[multibase.Encoding(echo[0])]; ok {
			mbName = n
		}
	}

	fmt.Println(echo)
	fmt.Println()

	rows := [][2]string{
		{"multibase", fmt.Sprintf("%s (%s)", mbName, mbPrefix)},
		{"version", fmt.Sprintf("cidv%d (0x%02x)", c.Version(), c.Version())},
		{"codec", fmt.Sprintf("%s (0x%x)", codecName, uint64(codec))},
		{"multihash", fmt.Sprintf("%s (0x%02x), %d bits (%d bytes = 0x%02x)", mhName, d.Code, d.Length*8, d.Length, d.Length)},
		{"digest", digestHex},
	}
	width := 0
	for _, r := range rows {
		if len(r[0]) > width {
			width = len(r[0])
		}
	}
	for _, r := range rows {
		fmt.Printf("%-*s:  %s\n", width, r[0], r[1])
	}

	fmt.Println()
	if cid.Supported(codec) {
		fmt.Printf("store:  yes — %s, usable with add / get / cat\n", codecName)
	} else {
		fmt.Printf("store:  no — %s is not held (this store only holds raw and json)\n", codecName)
	}
	return nil
}
