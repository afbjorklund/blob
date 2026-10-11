package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/multiformats/go-multihash"
	"github.com/spf13/cobra"

	"blob/storage"
)

var (
	storageType string
	storagePath string
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "blob",
		Short:   "A simple content-addressable blob store",
		Version: "0.1.0",
	}

	rootCmd.PersistentFlags().StringVarP(&storageType, "storage", "s", "dir", "storage backend (dir or sqlite)")
	rootCmd.PersistentFlags().StringVarP(&storagePath, "path", "p", "./.blob-store", "storage path")

	hashCmd := &cobra.Command{
		Use:   "hash [file]",
		Short: "Compute and print the CID hash of a file",
		Args:  cobra.ExactArgs(1),
		RunE:  runHash,
	}

	rootCmd.AddCommand(hashCmd)

	addCmd := &cobra.Command{
		Use:   "add <file>",
		Short: "Add a file to the blob store",
		Args:  cobra.ExactArgs(1),
		RunE:  runAdd,
	}

	rootCmd.AddCommand(addCmd)

	getCmd := &cobra.Command{
		Use:   "get <cid> <output>",
		Short: "Retrieve a file from the blob store",
		Args:  cobra.ExactArgs(2),
		RunE:  runGet,
	}

	rootCmd.AddCommand(getCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runHash(cmd *cobra.Command, args []string) error {
	file, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}

	digest := hash.Sum(nil)

	mh, err := multihash.Encode(digest, multihash.SHA2_256)
	if err != nil {
		return err
	}

	c := cid.NewCidV1(cid.Raw, mh)

	cidBase32hex, err := c.StringOfBase(multibase.Base32hex)
	if err != nil {
		return err
	}

	fmt.Println(cidBase32hex)
	return nil
}

func runAdd(cmd *cobra.Command, args []string) error {
	filePath := args[0]

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		file.Close()
		return err
	}

	digest := hash.Sum(nil)

	mh, err := multihash.Encode(digest, multihash.SHA2_256)
	if err != nil {
		file.Close()
		return err
	}

	c := cid.NewCidV1(cid.Raw, mh)

	cidBase32hex, err := c.StringOfBase(multibase.Base32hex)
	if err != nil {
		file.Close()
		return err
	}

	file.Close()

	storageBackend, err := getStorageBackend()
	if err != nil {
		return err
	}

	has, err := storageBackend.Has(digest)
	if err != nil {
		return err
	}

	if has {
		fmt.Printf("File already exists in store: %s\n", cidBase32hex)
		return nil
	}

	file, err = os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	if err := storageBackend.Put(digest, file); err != nil {
		return err
	}

	fmt.Printf("Added: %s\n", cidBase32hex)
	return nil
}

func runGet(cmd *cobra.Command, args []string) error {
	c, err := cid.Parse(args[0])
	if err != nil {
		return err
	}

	mh := c.Hash()
	digest := mh[2:]

	storageBackend, err := getStorageBackend()
	if err != nil {
		return err
	}

	reader, err := storageBackend.Get(digest)
	if err != nil {
		return err
	}
	defer reader.Close()

	out, err := os.Create(args[1])
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, reader); err != nil {
		return err
	}

	fmt.Printf("Retrieved: %s -> %s\n", args[0], args[1])
	return nil
}

func getStorageBackend() (storage.StorageBackend, error) {
	return storage.NewStorage(storage.StorageType(storageType), storagePath)
}
