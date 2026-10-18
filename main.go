package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"blob/cid"
	"blob/identity"
	"blob/storage"

	"github.com/spf13/cobra"
)

var (
	storageType string
	storagePath string
	s3Bucket    string
	s3Region    string
	outputPath  string
	onlySize    bool
	hashOnly    bool
	listenAddr  string
	httpUser    string
	httpPass    string
	// OCI import/export
	ociName string
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

func defaultHTTPUser() string {
	return os.Getenv("BLOB_USER")
}

func defaultHTTPPass() string {
	return os.Getenv("BLOB_PASS")
}

func defaultS3Bucket() string {
	return os.Getenv("BLOB_BUCKET")
}

// defaultS3Region is empty so the caller can leave it unset and minio-go
// defers to the server's bucket location (Ceph RGW accepts a common default
// like us-east-1, but an explicit region override is the more general
// answer — see BACKEND.md).
func defaultS3Region() string {
	return os.Getenv("BLOB_S3_REGION")
}

// resolveCLIAuth decides which Basic-auth credentials (if any) the http backend
// should use, from the places a user would realistically keep them. Precedence,
// highest first:
//
//  1. the --user/--pass pair (the flags default to BLOB_USER/BLOB_PASS, so an
//     explicit flag or the env pair both land here);
//  2. user:pass@ embedded in the --path URL (the quick token-in-a-URL case);
//  3. the user's ~/.netrc entry for the host — the durable, chmod-600 home for
//     secrets, keeping them out of shell history and the process argv.
//
// Only the http backend actually applies the value (dir and sql ignore it). If
// no source yields a user name, the zero Auth is returned and nothing is sent.
func resolveCLIAuth() storage.Auth {
	if httpUser != "" || os.Getenv("BLOB_USER") != "" {
		return storage.Auth{User: httpUser, Password: httpPass}
	}
	if u, p, ok := credsFromURL(storagePath); ok {
		return storage.Auth{User: u, Password: p}
	}
	return netrcAuth(hostOf(storagePath))
}

// credsFromURL extracts user:pass from a URL's userinfo, if present.
func credsFromURL(raw string) (string, string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return "", "", false
	}
	pass, _ := u.User.Password()
	return u.User.Username(), pass, true
}

// hostOf returns the host component of a URL (for netrc keying).
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// netrcPath is where the .netrc is read from, honoring $NETRC and falling back
// to the user's home directory (the same resolution curl and friends use).
func netrcPath() string {
	if p := os.Getenv("NETRC"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".netrc")
}

// netrcAuth returns the credentials for host from the user's .netrc, or a zero
// Auth if the file is absent or has no entry for the host. A specific `machine`
// entry for the host wins over the `default` entry, which is the fallback for
// hosts with no specific entry.
func netrcAuth(host string) storage.Auth {
	data, err := os.ReadFile(netrcPath())
	if err != nil {
		return storage.Auth{}
	}

	blocks := parseNetrc(string(data))
	if c, ok := blocks[host]; ok && c.User != "" {
		return c
	}
	if c, ok := blocks["default"]; ok {
		return c
	}
	return storage.Auth{}
}

// parseNetrc splits a .netrc into per-host credential blocks. Only the
// directives this store needs are understood — `machine`, `default`, `login`
// and `password`; everything else (account, macdef, ...) is ignored. It
// tokenizes the whole file, so the one-token-per-line layout .netrc allows is
// equivalent to the more common one-line-per-block layout.
func parseNetrc(data string) map[string]storage.Auth {
	const defaultKey = "default"
	blocks := map[string]storage.Auth{}

	current, login, pass := "", "", ""

	flush := func() {
		if current != "" {
			blocks[current] = storage.Auth{User: login, Password: pass}
		}
		current, login, pass = "", "", ""
	}

	fields := strings.Fields(data)
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "machine":
			flush()
			if i+1 < len(fields) {
				i++
				current = fields[i]
			}
		case "default":
			flush()
			current = defaultKey
		case "login":
			if i+1 < len(fields) {
				i++
				login = fields[i]
			}
		case "password":
			if i+1 < len(fields) {
				i++
				pass = fields[i]
			}
		}
	}
	flush()

	return blocks
}

func main() {
	rootCmd := &cobra.Command{
		Use:     "blob",
		Short:   "A simple content-addressable blob store",
		Version: "0.1.0",
		// A command that returns an error should not be interrupted by a wall
		// of usage text (that is a "I mistyped the flags" signal, not a "the
		// server said no" one). Silence cobra's own error + usage prints; the
		// error is reported exactly once, from main — so a 401 reads as a plain
		// line rather than error → usage → error.
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	rootCmd.PersistentFlags().StringVarP(&storageType, "storage", "s", defaultStorageType(), "storage backend (dir, sql, http, or s3)")
	rootCmd.PersistentFlags().StringVarP(&storagePath, "path", "p", defaultStoragePath(), "storage path (local dir for dir/sql; base URL for http; endpoint (host:port or http(s):// URL) for s3)")
	rootCmd.PersistentFlags().StringVarP(&s3Bucket, "bucket", "b", defaultS3Bucket(), "s3 backend bucket name (or BLOB_BUCKET)")
	rootCmd.PersistentFlags().StringVarP(&s3Region, "region", "r", defaultS3Region(), "s3 backend SigV4 region (or BLOB_S3_REGION; unset defers to the server)")
	rootCmd.PersistentFlags().StringVarP(&httpUser, "user", "u", defaultHTTPUser(), "remote backend credentials: Basic-auth user for http, access key for s3 (or BLOB_USER, or user:pass@ in --path)")
	rootCmd.PersistentFlags().StringVar(&httpPass, "pass", defaultHTTPPass(), "remote backend credentials: Basic-auth password/token for http, secret key for s3 (or BLOB_PASS, or the pass in user:pass@ --path)")

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
	getCmd.Flags().BoolVarP(&onlySize, "only-size", "", false, "print only the blob size in bytes, do not retrieve the blob")

	rootCmd.AddCommand(getCmd)

	catCmd := &cobra.Command{
		Use:   "cat <cid|name>",
		Short: "Write a blob's content to stdout (nothing else)",
		Args:  cobra.ExactArgs(1),
		RunE:  runCat,
	}

	rootCmd.AddCommand(catCmd)

	lookupCmd := &cobra.Command{
		Use:   "lookup <name>",
		Short: "Look up the CID of a stored blob by its index name",
		Args:  cobra.ExactArgs(1),
		RunE:  runLookup,
	}

	rootCmd.AddCommand(lookupCmd)

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the blob store over HTTP (read-only gateway)",
		Args:  cobra.NoArgs,
		RunE:  runServe,
	}

	serveCmd.Flags().StringVarP(&listenAddr, "listen", "l", "127.0.0.1:8080", "address to listen on (host:port)")

	rootCmd.AddCommand(serveCmd)

	registerOCICmds(rootCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// computeHash returns the Identity (multihash bytes) of a file, computed by
// streaming (content -> key). The OCI path uses identity.FromOCI instead,
// bridging from an existing OCI digest.
func computeHash(filePath string) (identity.Identity, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return identity.HashReader(file)
}

// detectCodec derives the codec a file would be stored under by peeking its
// first byte (the content-based rule, see cid.CodecForFirstByte).
func detectCodec(filePath string) (cid.Code, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	buf := make([]byte, 1)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return 0, err
	}
	if n == 0 {
		return cid.CodeRaw, nil
	}
	return cid.CodecForFirstByte(buf[0]), nil
}

// peekFirstByte returns the blob's first byte (0 when empty) without
// reading the rest.
func peekFirstByte(backend storage.StorageBackend, digest storage.Digest) (byte, error) {
	b, err := backend.Peek(digest, 1)
	if err != nil {
		return 0, err
	}
	if len(b) == 0 {
		return 0, nil
	}
	return b[0], nil
}

// cidForDigest derives the CID of a *stored* blob from its identity by peeking
// the first byte (the content-based codec rule) and composing the CID. It
// reads only a single byte, so it is constant work regardless of blob size and
// never re-hashes — a name resolves to the exact CID that `add` printed.
func cidForDigest(backend storage.StorageBackend, d storage.Digest) (cid.Cid, error) {
	first, err := peekFirstByte(backend, d)
	if err != nil {
		return cid.Cid{}, err
	}
	return cid.Build(cid.CodecForFirstByte(first), identity.Identity(d)), nil
}

// digestFromCID returns the storage key for c, validating that the CID carries
// a codec the store holds (raw / json) and a hash identity understands (SHA-2
// 256 today).
func digestFromCID(c cid.Cid) (storage.Digest, error) {
	if !cid.Supported(cid.Codec(c)) {
		return nil, fmt.Errorf("unsupported codec %q: this store only holds raw and json", cid.CodecName(cid.Codec(c)))
	}
	if _, err := identity.DigestBytes(identity.Identity(c.Hash())); err != nil {
		return nil, err
	}
	return storage.Digest(c.Hash()), nil
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

	c := cid.Build(mc, mh)
	cidStr, err := cid.Render(c)
	if err != nil {
		return err
	}

	if hashOnly {
		fmt.Printf("added %s %s\n", cidStr, args[0])
		return nil
	}

	digest := cid.Key(c)

	backend, err := getStorageBackend(storage.ReadWrite)
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

	c := cid.Build(mc, mh)
	cidStr, err := cid.Render(c)
	if err != nil {
		return err
	}

	fmt.Printf("%s  %s\n", cidStr, filePath)
	return nil
}

// resolveDigest turns a get/cat argument into the blob's digest
// (multihash): parsed as a CID when possible (no backend needed), or
// resolved via the read-only backend's name index otherwise.
func resolveDigest(arg string) (storage.Digest, error) {
	if c, err := cid.Parse(arg); err == nil {
		digest, err := digestFromCID(c)
		if err != nil {
			return nil, err
		}
		return digest, nil
	}
	if id, err := identity.FromOCIString(arg); err == nil {
		return id, nil
	}

	backend, err := getStorageBackend(storage.ReadOnly)
	if err != nil {
		return nil, err
	}
	defer backend.Close()

	digest, err := backend.Lookup(arg)
	if err != nil {
		return nil, fmt.Errorf("%q: name not found in index", arg)
	}
	return digest, nil
}

func runGet(cmd *cobra.Command, args []string) error {
	arg := args[0]

	digest, err := resolveDigest(arg)
	if err != nil {
		return err
	}

	if outputPath == "" {
		outputPath = arg
	}

	backend, err := getStorageBackend(storage.ReadOnly)
	if err != nil {
		return err
	}
	defer backend.Close()

	reader, size, err := backend.Get(digest)
	if err != nil {
		return err
	}
	defer reader.Close()

	if onlySize {
		fmt.Println(size)
		return nil
	}

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

func runCat(cmd *cobra.Command, args []string) error {
	arg := args[0]

	digest, err := resolveDigest(arg)
	if err != nil {
		return err
	}

	backend, err := getStorageBackend(storage.ReadOnly)
	if err != nil {
		return err
	}
	defer backend.Close()

	reader, _, err := backend.Get(digest)
	if err != nil {
		return err
	}
	defer reader.Close()

	if _, err := io.Copy(os.Stdout, reader); err != nil {
		return err
	}
	return nil
}

func runLookup(cmd *cobra.Command, args []string) error {
	name := args[0]

	backend, err := getStorageBackend(storage.ReadOnly)
	if err != nil {
		return err
	}
	defer backend.Close()

	digest, err := backend.Lookup(name)
	if err != nil {
		return fmt.Errorf("%q: name not found in index", name)
	}

	c, err := cidForDigest(backend, digest)
	if err != nil {
		return err
	}

	cidStr, err := cid.Render(c)
	if err != nil {
		return err
	}

	fmt.Println(cidStr)
	return nil
}

// getStorageBackend opens a backend in the requested mode and returns it for
// single use (the caller must Close). mode distinguishes the CLI write path
// (add, which needs to write) from the read path (get / lookup, which only
// reads and so should not create or mutate the store). The resolved credentials
// and S3 bucket/region ride along for the remote backends (no-ops for dir and
// sql).
func getStorageBackend(mode storage.Mode) (storage.StorageBackend, error) {
	return storage.NewStorage(storage.StorageType(storageType), storagePath, s3Bucket, s3Region, resolveCLIAuth(), mode)
}

// runServe starts a read-only HTTP gateway for the store, so a stable
// scheme like blob://<CID> can be mapped to
// http://<listen>/blob/<CID> without the client knowing the backend.
//
// The backend is opened once, read-only, and shared across the whole server.
// A read-only store has nothing to write, so it is the natural fit for a
// shared handle, and the sql backend is backed by a connection pool — hence
// safe to hand to every request goroutine — while the dir backend needs no
// locking at all. Opening a fresh backend per request would instead pay the
// (expensive, and on sql, file-descriptor-and-pool) open cost on every
// request for no benefit; the old per-request design was the fallback taken
// only because a single connection could not be shared, which the pool
// removes. A read-only open also means the server cannot create the store or
// accidentally mutate it (a missing store is a clear error, not a side
// effect), the same reason the dir backend honors the read-only mode.
func runServe(cmd *cobra.Command, args []string) error {
	backend, err := getStorageBackend(storage.ReadOnly)
	if err != nil {
		return err
	}
	defer backend.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/blob/", func(w http.ResponseWriter, r *http.Request) {
		serveBlob(w, r, backend, r.URL.Path[len("/blob/"):])
	})
	mux.HandleFunc("/name/", func(w http.ResponseWriter, r *http.Request) {
		serveName(w, r, backend, r.URL.Path[len("/name/"):])
	})
	// /v2/ is the OCI distribution read surface (see main_oci_serve.go): a layout
	// already in the store can be pulled by stock OCI clients (podman, oras,
	// skopeo, buildkit) with no `oci export` step. It is a distinct path prefix
	// from /blob/ and /name/, so the mux disambiguates by prefix and the three
	// namespaces never shadow each other.
	mux.Handle("/v2/", ociV2Handler(backend))

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
//
// backend is the server's shared read-only backend (opened once in
// runServe). It is safe to call Get on it from many goroutines
// concurrently: the sql backend pools its connections and holds each
// connection for the duration of the read, the dir backend opens a fresh
// file handle per call.
func serveBlob(w http.ResponseWriter, r *http.Request, backend storage.StorageBackend, ref string) {
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

	contentDigest, err := cid.ContentDigestHeader(c)
	if err != nil {
		http.Error(w, "invalid multihash", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Digest", contentDigest)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Type", cid.MediaType(c))

	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, reader); err != nil {
		return
	}
}

// serveName resolves an index name to its blob, derives the blob's CID
// by peeking the first byte (the same content-based codec rule as
// `add`/`hash`), and issues a 302 redirect to /blob/<CID>. The redirect
// is the name→blob pointer (mirroring the dir backend's symlink for the
// same role) and leaves all streaming / header work to /blob/<CID>.
// 302 (not 301): the name→CID mapping is mutable (last add wins), so it
// must not be cached.
func serveName(w http.ResponseWriter, r *http.Request, backend storage.StorageBackend, name string) {
	digest, err := backend.Lookup(name)
	if err != nil {
		http.Error(w, "name not found: "+name, http.StatusNotFound)
		return
	}

	c, err := cidForDigest(backend, digest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cidStr, err := cid.Render(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/blob/"+cidStr, http.StatusFound)
}
