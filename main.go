package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

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
	listenAddr  string
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

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the blob store over HTTP (read-only gateway)",
		Args:  cobra.NoArgs,
		RunE:  runServe,
	}

	serveCmd.Flags().StringVarP(&listenAddr, "listen", "l", "127.0.0.1:8080", "address to listen on (host:port)")

	rootCmd.AddCommand(serveCmd)

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

func computeHash(filePath string) (multihash.Multihash, error) {
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

// detectCodec picks the content-based codec (DESIGN.md) from the file's
// first byte: Json if it is '{' or '[', otherwise Raw (including for an
// empty file).
func detectCodec(filePath string) (multicodec.Code, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	buf := make([]byte, 1)
	n, err := file.Read(buf)
	if err != nil && err != io.EOF {
		return 0, err
	}

	// Json for a leading '{' or '['; otherwise (or empty) Raw.
	if n > 0 && (buf[0] == '{' || buf[0] == '[') {
		return multicodec.Json, nil
	}
	return multicodec.Raw, nil
}

// computeCID builds a CIDv1 from a multihash and a codec.
func computeCID(mh multihash.Multihash, mc multicodec.Code) (cid.Cid, error) {
	return cid.NewCidV1(uint64(mc), mh), nil
}

// stringFromCID renders a CID as the user-facing base32hex string
// (this store's presentation choice, see DESIGN.md).
func stringFromCID(c cid.Cid) (string, error) {
	return c.StringOfBase(multibase.Base32hex)
}

func digestFromCID(c cid.Cid) (storage.Digest, error) {
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

	// The storage key is the multihash itself (algorithm + digest),
	// self-describing.
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

	mc, err := detectCodec(filePath)
	if err != nil {
		return err
	}

	mh, err := computeHash(filePath)
	if err != nil {
		return err
	}

	c, err := computeCID(mh, mc)
	if err != nil {
		return err
	}
	cidStr, err := stringFromCID(c)
	if err != nil {
		return err
	}

	if hashOnly {
		fmt.Printf("added %s %s\n", cidStr, args[0])
		return nil
	}

	digest := storage.Digest(c.Hash())

	backend, err := getStorageBackend()
	if err != nil {
		return err
	}
	defer backend.Close()

	has, err := backend.Has(digest)
	if err != nil {
		return err
	}

	if !has {
		stat, err := file.Stat()
		if err != nil {
			return err
		}

		if err := backend.Put(digest, stat.Size(), file); err != nil {
			return err
		}
	}

	name := filepath.Base(filePath)
	if err := backend.Index(name, digest); err != nil {
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

	mc, err := detectCodec(filePath)
	if err != nil {
		return err
	}

	mh, err := computeHash(filePath)
	if err != nil {
		return err
	}

	c, err := computeCID(mh, mc)
	if err != nil {
		return err
	}
	cidStr, err := stringFromCID(c)
	if err != nil {
		return err
	}

	fmt.Printf("%s  %s\n", cidStr, filePath)
	return nil
}

func runGet(cmd *cobra.Command, args []string) error {
	arg := args[0]

	var digest storage.Digest
	if c, err := cid.Parse(arg); err != nil {
		backend, berr := getStorageBackend()
		if berr != nil {
			return berr
		}
		defer backend.Close()

		var lerr error
		digest, lerr = backend.Lookup(arg)
		if lerr != nil {
			return fmt.Errorf("%q: name not found in index", arg)
		}
	} else {
		var derr error
		digest, derr = digestFromCID(c)
		if derr != nil {
			return derr
		}
	}

	if outputPath == "" {
		outputPath = arg
	}

	backend, err := getStorageBackend()
	if err != nil {
		return err
	}
	defer backend.Close()

	reader, _, err := backend.Get(digest)
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

	fmt.Printf("Retrieved: %s -> %s\n", arg, outputPath)
	return nil
}

func getStorageBackend() (storage.StorageBackend, error) {
	return storage.NewStorage(storage.StorageType(storageType), storagePath)
}

// runServe starts a read-only HTTP gateway for the store, so a stable
// scheme like blob://<CID> can be mapped to
// http://<listen>/blob/<CID> without the client knowing the backend.
func runServe(cmd *cobra.Command, args []string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/blob/", func(w http.ResponseWriter, r *http.Request) {
		serveBlob(w, r, r.URL.Path[len("/blob/"):])
	})

	srv := &http.Server{Addr: listenAddr, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	fmt.Printf("Serving on http://%s\n", listenAddr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}

// serveBlob streams a single blob to the response, identified by CID.
// The CID carries the codec (→ Content-Type) and the multihash (→ the
// digest, → Content-Digest) — both already known at request time, so no
// content is read for the headers. The body is streamed; the blob is
// never read into memory.
func serveBlob(w http.ResponseWriter, r *http.Request, ref string) {
	// Each request gets its own backend (open + defer Close), matching the
	// per-call pattern in runGet/runAdd. The sqlite *Conn is not safe for
	// concurrent use, so serve must not share one connection across
	// goroutines; SQLite allows many concurrent readers, each on its own
	// connection. (A zombiezen ConnPool would let this share one pooled
	// handle safely — see follow-up notes in DESIGN.md.)
	backend, err := getStorageBackend()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer backend.Close()

	c, err := cid.Parse(ref)
	if err != nil {
		http.Error(w, "invalid CID: "+ref, http.StatusBadRequest)
		return
	}

	digest, err := digestFromCID(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	reader, size, err := backend.Get(digest)
	if err != nil {
		http.Error(w, "not found: "+ref, http.StatusNotFound)
		return
	}
	defer reader.Close()

	decoded, err := multihash.Decode(c.Hash())
	if err != nil {
		http.Error(w, "invalid multihash", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Digest", "sha-256="+base64.StdEncoding.EncodeToString(decoded.Digest))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))

	if c.Type() == uint64(multicodec.Json) {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, reader); err != nil {
		return
	}
}
