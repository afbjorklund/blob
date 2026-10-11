package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
	"github.com/spf13/cobra"

	"blob/storage"
)

var (
	storageType string
	storagePath string
	outputPath  string
	hashOnly    bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "blob",
		Short:   "A simple content-addressable blob store",
		Version: "0.1.0",
	}

	rootCmd.PersistentFlags().StringVarP(&storageType, "storage", "s", "dir", "storage backend (dir or sql)")
	rootCmd.PersistentFlags().StringVarP(&storagePath, "path", "p", "./.blob-store", "storage path")

	addCmd := &cobra.Command{
		Use:   "add <file>",
		Short: "Add a file to the blob store",
		Args:  cobra.ExactArgs(1),
		RunE:  runAdd,
	}

	addCmd.Flags().BoolVarP(&hashOnly, "only-hash", "n", false, "only compute and print the hash, do not store")

	rootCmd.AddCommand(addCmd)

	getCmd := &cobra.Command{
		Use:   "get <cid>",
		Short: "Retrieve a file from the blob store",
		Args:  cobra.ExactArgs(1),
		RunE:  runGet,
	}

	getCmd.Flags().StringVarP(&outputPath, "output", "o", "", "output file path")

	rootCmd.AddCommand(getCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func isJSON(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	return data[0] == '{' || data[0] == '['
}

func computeHash(filePath string) ([]byte, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return nil, err
	}

	return hash.Sum(nil), nil
}

func isJSONFile(filePath string) (bool, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return false, err
	}
	defer file.Close()

	buf := make([]byte, 1)
	_, err = file.Read(buf)
	if err != nil {
		return false, err
	}

	return isJSON(buf), nil
}

func computeCID(hash []byte, isJSON bool) (string, error) {
	mh, err := multihash.Encode(hash, multihash.SHA2_256)
	if err != nil {
		return "", err
	}

	codec := multicodec.Raw
	if isJSON {
		codec = multicodec.Json
	}

	c := cid.NewCidV1(uint64(codec), mh)

	s, err := c.StringOfBase(multibase.Base32hex)
	if err != nil {
		return "", err
	}

	return s, nil
}

func runAdd(cmd *cobra.Command, args []string) error {
	filePath := args[0]

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}

	isJSON, err := isJSONFile(filePath)
	if err != nil {
		return err
	}

	hash, err := computeHash(filePath)
	if err != nil {
		return err
	}

	cidBase32hex, err := computeCID(hash, isJSON)
	if err != nil {
		return err
	}

	if hashOnly {
		fmt.Printf("%s  %s\n", cidBase32hex, args[0])
		return nil
	}

	storageBackend, err := getStorageBackend()
	if err != nil {
		return err
	}

	has, err := storageBackend.Has(hash)
	if err != nil {
		return err
	}

	if has {
		fmt.Printf("File already exists in store: %s\n", cidBase32hex)
		return nil
	}

	stat, err := file.Stat()
	if err != nil {
		return err
	}

	if err := storageBackend.Put(hash, stat.Size(), file); err != nil {
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

	if outputPath == "" {
		outputPath = args[0]
	}

	mh := c.Hash()
	decoded, err := multihash.Decode(mh)
	if err != nil {
		return err
	}

	if decoded.Code != multihash.SHA2_256 {
		return fmt.Errorf("unsupported hash algorithm: %d", decoded.Code)
	}

	hash := decoded.Digest

	storageBackend, err := getStorageBackend()
	if err != nil {
		return err
	}

	reader, _, err := storageBackend.Get(hash)
	if err != nil {
		return err
	}
	defer reader.Close()

	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, reader); err != nil {
		return err
	}

	fmt.Printf("Retrieved: %s -> %s\n", args[0], outputPath)
	return nil
}

func getStorageBackend() (storage.StorageBackend, error) {
	return storage.NewStorage(storage.StorageType(storageType), storagePath)
}
