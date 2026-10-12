package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"blob/storage"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/multiformats/go-multicodec"
	"github.com/multiformats/go-multihash"
	"github.com/spf13/cobra"
)

var (
	storageType string
	storagePath string
	outputPath  string
	hashOnly    bool
)

func defaultStorageType() string {
	if v := os.Getenv("BLOB_STORAGE_TYPE"); v != "" {
		return v
	}
	return "dir"
}

func defaultStoragePath() string {
	if v := os.Getenv("BLOB_STORAGE_PATH"); v != "" {
		return v
	}
	return "./.blob-store"
}

func main() {
	rootCmd := &cobra.Command{
		Use:     "blob",
		Short:   "A simple content-addressable blob store",
		Version: "0.1.0",
	}

	rootCmd.PersistentFlags().StringVarP(&storageType, "storage", "s", defaultStorageType(), "storage backend (dir or sql)")
	rootCmd.PersistentFlags().StringVarP(&storagePath, "path", "p", defaultStoragePath(), "storage path")

	addCmd := &cobra.Command{
		Use:   "add <file>",
		Short: "Add a file to the blob store",
		Args:  cobra.ExactArgs(1),
		RunE:  runAdd,
	}

	addCmd.Flags().BoolVarP(&hashOnly, "only-hash", "n", false, "only compute and print the hash, do not store")

	rootCmd.AddCommand(addCmd)

	hashCmd := &cobra.Command{
		Use:   "hash <file>",
		Short: "Compute and print the CID of a file without storing it",
		Args:  cobra.ExactArgs(1),
		RunE:  runHash,
	}

	rootCmd.AddCommand(hashCmd)

	getCmd := &cobra.Command{
		Use:   "get <cid|name>",
		Short: "Retrieve a file from the blob store (by CID or indexed name)",
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

func supportedCodecs() map[uint64]bool {
	return map[uint64]bool{
		uint64(multicodec.Raw):  true,
		uint64(multicodec.Json): true,
	}
}

func codecName(code uint64) string {
	return multicodec.Code(code).String()
}

func isJSON(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	return data[0] == '{' || data[0] == '['
}

func computeDigest(filePath string) (multihash.Multihash, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return nil, err
	}

	mh, err := multihash.Encode(h.Sum(nil), multihash.SHA2_256)
	if err != nil {
		return nil, err
	}

	return mh, nil
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

func computeCID(mh multihash.Multihash, isJSON bool) (storage.Digest, string, error) {
	codec := multicodec.Raw
	if isJSON {
		codec = multicodec.Json
	}

	c := cid.NewCidV1(uint64(codec), mh)

	s, err := c.StringOfBase(multibase.Base32hex)
	if err != nil {
		return nil, "", err
	}

	return storage.Digest(mh), s, nil
}

func keyFromCID(c cid.Cid) (storage.Digest, error) {
	if !supportedCodecs()[c.Type()] {
		return nil, fmt.Errorf("unsupported codec %q: this store only holds %q",
			codecName(c.Type()), "raw and json")
	}

	mh := c.Hash()
	decoded, err := multihash.Decode(mh)
	if err != nil {
		return nil, err
	}

	if decoded.Code != multihash.SHA2_256 {
		return nil, fmt.Errorf("unsupported hash algorithm: %d", decoded.Code)
	}

	// The key is the multihash itself (algorithm + digest), self-describing.
	return mh, nil
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

	digest, err := computeDigest(filePath)
	if err != nil {
		return err
	}

	hash, cidStr, err := computeCID(digest, isJSON)
	if err != nil {
		return err
	}

	if hashOnly {
		fmt.Printf("added %s %s\n", cidStr, args[0])
		return nil
	}

	storageBackend, err := getStorageBackend()
	if err != nil {
		return err
	}
	defer storageBackend.Close()

	has, err := storageBackend.Has(hash)
	if err != nil {
		return err
	}

	if !has {
		stat, err := file.Stat()
		if err != nil {
			return err
		}

		if err := storageBackend.Put(hash, stat.Size(), file); err != nil {
			return err
		}
	}

	name := filepath.Base(filePath)
	if err := storageBackend.Index(name, hash); err != nil {
		return err
	}

	if has {
		fmt.Printf("File already exists in store: %s\n", cidStr)
		return nil
	}

	fmt.Printf("Added: %s\n", cidStr)
	return nil
}

func runHash(cmd *cobra.Command, args []string) error {
	filePath := args[0]

	isJSON, err := isJSONFile(filePath)
	if err != nil {
		return err
	}

	digest, err := computeDigest(filePath)
	if err != nil {
		return err
	}

	_, cidStr, err := computeCID(digest, isJSON)
	if err != nil {
		return err
	}

	fmt.Printf("%s  %s\n", cidStr, filePath)
	return nil
}

func runGet(cmd *cobra.Command, args []string) error {
	arg := args[0]

	c, err := cid.Parse(arg)
	if err != nil {
		backend, berr := getStorageBackend()
		if berr != nil {
			return berr
		}
		defer backend.Close()

		hash, lerr := backend.Lookup(arg)
		if lerr != nil {
			return fmt.Errorf("%q: name not found in index", arg)
		}

		if outputPath == "" {
			outputPath = arg
		}

		return writeBlob(cmd, backend, hash, arg)
	}

	hash, err := keyFromCID(c)
	if err != nil {
		return err
	}

	if outputPath == "" {
		outputPath = arg
	}

	backend, err := getStorageBackend()
	if err != nil {
		return err
	}
	defer backend.Close()

	return writeBlob(cmd, backend, hash, arg)
}

func writeBlob(cmd *cobra.Command, backend storage.StorageBackend, hash storage.Digest, label string) error {
	reader, _, err := backend.Get(hash)
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

	fmt.Printf("Retrieved: %s -> %s\n", label, outputPath)
	return nil
}

func getStorageBackend() (storage.StorageBackend, error) {
	return storage.NewStorage(storage.StorageType(storageType), storagePath)
}
