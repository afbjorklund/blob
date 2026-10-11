package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/multiformats/go-multihash"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <file>\n", os.Args[0])
		os.Exit(1)
	}

	file, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening file: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		fmt.Fprintf(os.Stderr, "Error reading file: %v\n", err)
		os.Exit(1)
	}

	digest := hash.Sum(nil)

	mh, err := multihash.Encode(digest, multihash.SHA2_256)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating multihash: %v\n", err)
		os.Exit(1)
	}

	c := cid.NewCidV1(cid.Raw, mh)

	cidBase32hex, err := c.StringOfBase(multibase.Base32hex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error encoding CID: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(cidBase32hex)
}
