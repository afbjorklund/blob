package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"blob/cid"
	"blob/identity"
	"blob/index"
	"blob/storage"

	"github.com/cheggaaa/pb/v3"
	"github.com/mattn/go-isatty"
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
	getVerify   bool
	hashOnly    bool
	hashCheck   bool
	hashIgnore  bool
	listenAddr  string
	httpUser    string
	httpPass    string
	// OCI import/export
	ociNames []string

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
		Use:   "hash <file>...",
		Short: "Compute and print the CID of each file without storing it",
		Long: `Compute and print the CID of each given file, one per line (like sha256sum):
  <CID>  <file>

With -c/--check the argument is instead a checksum list (one <CID>  <file> per
line, as produced by hash) and each line is verified against the local file
named on it, reporting one line per entry (<file>: OK, <file>: FAILED, or
<file>: FAILED open or read for a file that could not be read). -i/--ignore-
missing skips files that cannot be read instead of failing with a WARNING.
The store is never consulted: files are only read and hashed in place.

The checksum list records the full CID (not a bare digest), so it is self-
describing — it carries the algorithm and codec. It therefore does not read a
digest the way sha256sum does (which must assume sha256); no --base or
--strict-style parsing is needed, and there are no 'improperly formatted'
lines.`,
		Args: cobra.MinimumNArgs(1),
		RunE: runHash,
	}

	hashCmd.Flags().BoolVarP(&hashCheck, "check", "c", false, "verify the lines of a checksum list (each <CID>  <file>) against the local files named on them")
	hashCmd.Flags().BoolVarP(&hashIgnore, "ignore-missing", "i", false, "with -c, skip files that cannot be read (no WARNING, not counted as a failure)")

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
	getCmd.Flags().BoolVarP(&getVerify, "verify", "", false, "re-hash the downloaded data against the resolved multihash and fail the get on a mismatch (the file is not completed)")

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

// progress wraps r in a pb progress bar for the transfer of `size` bytes, but
// only when stdout is a TTY (pb renders to stdout; we never want to pollute a
// piped / redirected stream) — otherwise r is passed through and bar is nil.
// The bar tracks the actual transfer (the reads of r), the slow half of add /
// get; the hash step is assumed fast and is never barred.
//
// It returns the reader to read for the transfer AND the bar itself: the
// storage backends drive the read with io.Copy and never Close the reader
// (they close their own destination), so pb's own Finish is never triggered —
// the caller owns completing the bar (call bar.Finish() once the transfer is
// done). Finish is idempotent and synchronous: it writes the final frame and
// stops the refresh goroutine. A transfer fast enough to finish before pb's
// first 200 ms tick paints only that single final line (or less), so a quick
// op never flashes a bar.
func progress(r io.Reader, size int64) (io.Reader, *pb.ProgressBar) {
	fd := os.Stdout.Fd()
	if !isatty.IsTerminal(fd) && !isatty.IsCygwinTerminal(fd) {
		return r, nil
	}
	bar := pb.New64(size).Start()
	return bar.NewProxyReader(r), bar
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

	c, err := cidForFile(filePath)
	if err != nil {
		return err
	}

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

		pr, bar := progress(file, stat.Size())
		putErr := backend.Put(digest, stat.Size(), pr)
		if bar != nil {
			bar.Finish()
		}
		if putErr != nil {
			return putErr
		}
	}

	name := filepath.Base(filePath)
	skip, err := index.Check(name, digest)
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

// cidForFile computes (but does not store) the file's CID: peek the first byte
// for the codec (cidForFile's content-based rule), hash the rest, compose the
// CID. It is the pure content→CID half shared by hash (hash many files),
// hash -c (verify a checksum list against local files), and add's --only-hash.
func cidForFile(filePath string) (cid.Cid, error) {
	mc, err := detectCodec(filePath)
	if err != nil {
		return cid.Cid{}, err
	}

	mh, err := computeHash(filePath)
	if err != nil {
		return cid.Cid{}, err
	}

	return cid.Build(mc, mh), nil
}

func runHash(cmd *cobra.Command, args []string) error {
	if hashCheck {
		return runHashCheck(args)
	}

	// hash: one line per file, <CID>  <file>, like sha256sum.
	failed := false
	for _, filePath := range args {
		c, err := cidForFile(filePath)
		if err != nil {
			failed = true
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			continue
		}
		cidStr, err := cid.Render(c)
		if err != nil {
			failed = true
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			continue
		}
		fmt.Printf("%s  %s\n", cidStr, filePath)
	}
	if failed {
		return fmt.Errorf("at least one file could not be hashed")
	}
	return nil
}

// runHashCheck verifies the lines (each <CID>  <file>) of one or more checksum
// lists (the arguments) against the local files named on them, reporting one
// line per entry. It never touches the store: each entry hashes the named
// local file and compares the CID it recomputes to the CID on the line.
//
// The CID on the line is canonical (base32hex, raw/json) — the exact output of
// `hash` — so we simply parse it and CID-compare it to the local one; there is
// no digest-style parsing or an assumed algorithm (sha256sum's weakness: it
// only says the hash is *a* 64-hex number, and must assume it is SHA-256). A
// file that cannot be read reports FAILED open or read (or is silently skipped
// under -i), and a non-zero exit is returned when any entry fails or is
// unreadable (missing files under -i do not fail).
func runHashCheck(args []string) error {
	var (
		mismatch   int
		unreadable int
	)

	for _, listFile := range args {
		entries, err := readChecksumList(listFile)
		if err != nil {
			return err
		}

		for _, e := range entries {
			wantCID, name, ok := splitChecksumLine(e)
			if !ok {
				return fmt.Errorf("bad checksum-list line (expected <CID>  <file>): %s", strings.TrimSpace(e))
			}
			want, err := cid.Parse(wantCID)
			if err != nil {
				return fmt.Errorf("bad CID on checksum-list line %q: %v", strings.TrimSpace(e), err)
			}

			cLocal, err := cidForFile(name)
			if err != nil {
				unreadable++
				if hashIgnore {
					continue // -i: a file we cannot read is simply not present here
				}
				fmt.Printf("%s: FAILED open or read\n", name)
				fmt.Fprintf(os.Stderr, "blob: %s: %v\n", name, err)
				continue
			}

			if cLocal.Equals(want) {
				fmt.Printf("%s: OK\n", name)
			} else {
				mismatch++
				fmt.Printf("%s: FAILED\n", name)
			}
		}
	}

	if mismatch > 0 {
		fmt.Fprintf(os.Stderr, "blob: WARNING: %d computed checksum%s did NOT match\n", mismatch, plural(mismatch))
	}
	if unreadable > 0 && !hashIgnore {
		fmt.Fprintf(os.Stderr, "blob: WARNING: %d listed file%s could not be read\n", unreadable, plural(unreadable))
	}

	if mismatch > 0 || (unreadable > 0 && !hashIgnore) {
		return fmt.Errorf("checksum verification failed")
	}
	return nil
}

// plural returns "s" when n != 1, "" otherwise (a tiny local helper rather than
// importing a pluralizer for one word).
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// readChecksumList returns the non-blank, non-comment lines of a checksum list.
func readChecksumList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		ln := sc.Text()
		trimmed := strings.TrimLeft(ln, " \t")
		if trimmed == "" || trimmed[0] == '#' {
			continue
		}
		out = append(out, ln)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// splitChecksumLine splits a checksum-list line "<CID>  <file>" into its CID
// and file name. The separator is the whitespace run between them; the CID is
// a single token (no spaces) and the name is the rest of the line (it may
// contain spaces and is not path-escaped, the same convention sha256sum uses).
func splitChecksumLine(line string) (cidStr string, name string, ok bool) {
	line = strings.TrimLeft(line, " \t")
	i := 0
	for i < len(line) && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	if i == 0 || i == len(line) {
		return "", "", false
	}
	cidStr = line[:i]
	name = strings.TrimLeft(line[i:], " \t")
	if name == "" {
		return "", "", false
	}
	return cidStr, name, true
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

	// Download to a sibling <target>.tmp and rename it into place only
	// once the data is in and (if --verify) checked — the same tmp+rename
	// atomicity as the dir backend and the name index, so a failed or
	// interrupted get never leaves a partial file at the target path.
	tmp := outputPath + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}

	pr, bar := progress(reader, size)
	_, err = io.Copy(out, pr)
	if bar != nil {
		bar.Finish()
	}
	if err != nil {
		_ = out.Close()
		removeTmp(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		removeTmp(tmp)
		return err
	}

	if getVerify {
		if err := checkRetrieved(tmp, digest); err != nil {
			removeTmp(tmp)
			return err
		}
	}

	if err := os.Rename(tmp, outputPath); err != nil {
		removeTmp(tmp)
		return err
	}

	if getVerify {
		fmt.Printf("Retrieved: %s -> %s (content verified against the resolved multihash)\n", arg, outputPath)
	} else {
		fmt.Printf("Retrieved: %s -> %s\n", arg, outputPath)
	}
	return nil
}

// checkRetrieved re-hashes a downloaded file and fails if its bytes do not
// match the resolved multihash. The authority is the *resolved* digest — the
// argument may have been a name or an OCI digest, only the digest is a
// promise. It is the second read of the file (like add's hash-then-store);
// SHA-2 256 is hardware-accelerated, so the re-check is cheap. The caller
// removes the file on failure — the target path is never touched.
func checkRetrieved(path string, want storage.Digest) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if !identity.Check(f, identity.Identity(want)) {
		return fmt.Errorf("verification failed: %s does not match the resolved multihash", path)
	}
	return nil
}

// removeTmp best-effort deletes a leftover .tmp file, warning on stderr if it
// cannot be removed (so a stuck file is noticed, not silently leaked).
func removeTmp(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "blob: leftover %s: %v; keeping the file\n", path, err)
	}
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
