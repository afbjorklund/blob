package main

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

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
	onlyMtime   bool
	hashOnly    bool
	listenAddr  string
	httpUser    string
	httpPass    string
	// OCI import/export
	ociName string

	// cid
	cidBase string
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

	cidCmd := &cobra.Command{
		Use:   "cid <cid|name>",
		Short: "Inspect a CID: version, codec, multihash and digest",
		Args:  cobra.ExactArgs(1),
		RunE:  runCid,
	}

	cidCmd.Flags().StringVarP(&cidBase, "base", "", "base32hex", "render the first-line CID in this multibase (a name like base32hex, base16, base64url, base58btc)")

	rootCmd.AddCommand(cidCmd)

	getCmd := &cobra.Command{
		Use:   "get <cid|name>",
		Short: "Retrieve a file from the blob store (by CID or indexed name)",
		Args:  cobra.ExactArgs(1),
		RunE:  runGet,
	}

	getCmd.Flags().StringVarP(&outputPath, "output", "o", "", "output file path")
	getCmd.Flags().BoolVarP(&onlySize, "only-size", "", false, "print only the blob size in bytes, do not retrieve the blob")
	getCmd.Flags().BoolVarP(&onlyMtime, "only-mtime", "", false, "print only the blob's last-modified time in Unix seconds, do not retrieve the blob")

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

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the names recorded in the index",
		Args:  cobra.NoArgs,
		RunE:  runList,
	}

	rootCmd.AddCommand(listCmd)

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
	skip, err := checkName(name, digest)
	if err != nil {
		return err
	}
	if !skip {
		if err := backend.Index(name, digest); err != nil {
			return err
		}
	}

	if has {
		fmt.Printf("File already exists in store: %s\n", cidStr)
		return nil
	}

	if skip {
		fmt.Printf("Added: %s (name %q skipped: it is already the blob's own digest handle)\n", cidStr, name)
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

// checkName decides what happens when the index is about to record `nm`,
// which resolves to the blob `target`. The invariant behind the rule: a name
// that *looks* like a native handle (an OCI digest the store converts, or a
// CID the store holds) is intercepted by the resolution order (CID →
// sha256-digest → name) before the name index is ever consulted — a name
// entry for it would be unreachable and misleading. So: such a name may be
// recorded only if it resolves to exactly the blob being stored (and then it
// is *skipped*, since the entry would be redundant with the direct address);
// in every other form it is rejected. Names that the resolver will never
// intercept — sha256 digests of other algorithms ("sha1:" etc., which no
// resolver path converts) and CIDs for codecs the store doesn't hold (e.g. a
// dag-pb CID from `ipfs add`, the "ipfs get → blob add" interop handle) —
// are allowed to record as ordinary names.
//
// Separately: '/' is not a legal name character yet (it would make a
// hierarchy: index/ subdirectories for dir, nested object keys for s3, path
// segments for http — the FQDN-name story, deferred, see the plan in
// AGENTS.md; see docs/names.md for the deferred plan); control
// characters (\0 \n \r DEL) are rejected because no
// backend can hold them.
//
// Returns (skip, err): skip=true means the blob is stored but no name entry
// is written (the name is already the blob's own direct address); a plain
// name is (false, nil) — record it.
func checkName(nm string, target []byte) (bool, error) {
	if nm == "" || nm == "." || nm == ".." {
		return false, fmt.Errorf("invalid index name %q", nm)
	}
	if strings.ContainsRune(nm, '/') {
		return false, fmt.Errorf("name %q contains a path separator; names in the index are flat (one level) — FQDN-style names need escaping that is not implemented yet", nm)
	}
	for _, c := range nm {
		if c == 0 || c == '\n' || c == '\r' || c == 0x7f {
			return false, fmt.Errorf("name %q contains a control character; names in the index cannot hold \\0, \\n or \\r", nm)
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
		// A CID for an unsupported codec (e.g. dag-pb): the resolver does not
		// intercept it, so it may be recorded as a name — the cross-store
		// handle (see the function doc). `get`/`oci ls` of such a string
		// resolves through the name index and cross-checks the digest.
	}
	return false, nil
}

// resolveDigest turns a get/cat argument into the blob's digest
// (multihash): parsed as a CID when possible (no backend needed), or
// resolved via the read-only backend's name index otherwise.
//
// A CID whose codec the store does NOT hold (a dag-pb CID from `ipfs add`,
// say) is *not a direct handle* in this store, but it still needs to be a
// usable name — that's the interop path "ipfs get → blob add" produces. So
// it goes through the name index; if a name entry was recorded (by
// `add`/`oci import`), the digest must match the CID's own (or the store is
// in an inconsistent state and we refuse to lie about it). Any other
// argument that isn't a direct handle is resolved as a name, as before.
func resolveDigest(arg string) (storage.Digest, error) {
	var foreignCid cid.Cid
	var isForeign bool
	if c, err := cid.Parse(arg); err == nil {
		if cid.Supported(cid.Codec(c)) {
			return digestFromCID(c)
		}
		foreignCid = c
		isForeign = true
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
		if isForeign {
			return nil, fmt.Errorf("unsupported codec %q: this store only holds raw and json (and the name %q is not recorded)", cid.CodecName(cid.Codec(foreignCid)), arg)
		}
		return nil, fmt.Errorf("%q: name not found in index", arg)
	}
	if isForeign && !bytes.Equal(digest, cid.Key(foreignCid)) {
		return nil, fmt.Errorf("name %q resolves to a different digest than the CID it is named after (store/index inconsistency)", arg)
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

	reader, size, mtime, err := backend.Get(digest)
	if err != nil {
		return err
	}
	defer reader.Close()

	if onlySize {
		fmt.Println(size)
		return nil
	}

	if onlyMtime {
		fmt.Println(mtime)
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

	reader, _, _, err := backend.Get(digest)
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

// runList prints the recorded index names, one per line. It is the only
// command that lists the index (not the CAS): a store with one blob and
// two names prints two lines; a store with stored blobs but no names
// prints nothing.
func runList(cmd *cobra.Command, args []string) error {
	backend, err := getStorageBackend(storage.ReadOnly)
	if err != nil {
		return err
	}
	defer backend.Close()

	names, err := backend.Names()
	if err != nil {
		return err
	}

	for _, name := range names {
		fmt.Println(name)
	}
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
