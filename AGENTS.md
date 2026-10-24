# AGENTS.md

Content-addressable blob store (CIDv1, base32hex) with `dir`, `sql` (SQLite),
`http` and `s3` backends, plus OCI import/export (`oci import`/`export`/`ls`,
aliases `add`/`get`) and a read-only HTTP gateway + OCI-distribution read
surface (`blob serve`). Single Go module, no CI. The reusable logic lives in
testable sub-packages — `identity/` (multihash ↔ OCI-digest bridge), `cid/` (the
store's CID conventions), `index/` (the name-index conventions: recordability
check + default name), `oci/` (layout + artifact + layer, pure go-digest +
stdlib), `daemon/` (the read/serving layer: a `Store`/`Backend`-agnostic
`Server` exposing the store in both the store's own language — CID — and OCI,
over HTTP; see below) — each with table-driven tests (`identity/identity_test.go`,
`cid/cid_test.go`, `index/index_test.go`, `oci/oci_test.go`,
`daemon/daemon_test.go`, plain `go test`); the `storage/` backends have their
own tests (table-driven round-trip + a `-race` concurrent-read guard, run
against in-memory `httptest` servers for the `http` and `s3` backends). Opt-in *smoke*
tests hit real targets and skip unless their env var is set: `TestDirSmoke` /
`TestHTTPSmoke` / `TestS3Smoke` (storage) and `TestServeSmoke` (daemon: the
whole read surface over a real socket, both image and artifact, both methods and
CID and name, gated by `BLOB_SERVE_SMOKE` — needs `curl` on PATH). `main` has no
tests by convention (it is the CLI / adapters).

## Go toolchain

```bash
go build -o blob
golangci-lint run          # govet + gocyclo; gofmt/goimports formatters
go test ./...              # run the package tests (add -race for the concurrency guard)
```

- Inspect a dependency's API with `go doc <pkg> [symbol]` (e.g.
  `go doc github.com/multiformats/go-multicodec DagPb`) — it resolves the *selected*
  version from go.mod. Do not grep the module cache: the versioned directory
  names have `@` in them, globs mangle it, and `ls $(go env GOMODCACHE)/...` is
  the fragile path `go doc` exists to replace
- A human editor formats on save, so it catches non-gofmt code immediately; an
  AI edit often mangles whitespace/indentation silently — so right after editing
  any `.go` file, re-format: `gofmt -w <file>` (or `goimports -w` for import
  grouping). Formatting is light — do it per edit; the full `golangci-lint run`
  is heavier and is the final check, after `go build`/`go test` pass

## blob commands

- `blob add <file>` — store file (prints CID). Records
  `basename(file) → multihash` in the name index
- `blob add --only-hash/-n <file>` — like `ipfs add --only-hash`: print
  `added <cid> <file>` (one space), do not store
- `blob hash <file>...` — print `sha256sum`-style `<cid>  <file>` (two spaces)
  for **each** given file, one line per file, without storing (equivalent to
  what `ipfs add --only-hash` gives after the `added` prefix is stripped).
  Each file is opened, hashed and closed independently, so one unreadable file
  does not stop the others — it prints its own `Error:` line to stderr and the
  exit code is non-zero if *any* file failed (coreutils `sha256sum` does the
  same)
  - `blob hash -c <list>` — *verify* instead of compute: each line of `<list>`
    (one or more checksum lists) is `<cid>  <file>`; `blob` recomputes the
    local `<file>`'s CID and compares, printing `<file>: OK`, `<file>: FAILED`
    (content differs) or `<file>: FAILED open or read` (the file is not there /
    not readable). Exit non-zero if any entry fails or is unreadable. Never
    touches the store — it hashes the named local file and compares the CIDs.
    Blank and `#`-leading lines are skipped
  - `blob hash -c -i <list>` — like `-c` but `-i`/`--ignore-missing` silently
    skips entries whose file cannot be read (no `FAILED open or read`, no
    WARNING, not counted as a failure) — the "one checksums list for all
    arches, but I only downloaded one" case. A content mismatch still fails.
  The list records the **full CID** (not a bare digest), so it is
  self-describing — it carries the algorithm and the codec, and `-c` needs no
  digest-style parsing and has no "improperly formatted" lines (sha256sum's
  weakness: it only says the hash, not that it is `sha256`). In `main.go`
  (`runHash` → `runHashCheck`, sharing `cidForFile` with `runAdd`)
- `blob cid <cid|name>` — inspect a CID (any multibase) or an index name. Prints
  the CID **as given** on its own first line (a name argument is resolved to the
  CID, the one `lookup` would return) — then, after a blank line, a *breakdown*
  of the multibase, version, codec (name + hex code), multihash (name + hex
  code + size in bits/bytes), and raw digest (lowercase base16) — then the
  `store:` line (codec in raw/json → `yes, usable with add / get / cat`; any
  other codec →
  <!-- markdownlint-disable-next-line MD013 -->
  `no — <codec> is not held (this store only holds raw and json; the CID still decodes to the same multihash)`).
  Pure decode, no hashing/storing: a CID argument never opens a backend, a name
  argument resolves it read-only and derives the codec via first-byte `Peek`
  (the same rule as `lookup`). Every multiformat spelling comes from the
  go-multibase / go-multicodec / go-multihash tables — nothing is hard-coded,
  and the tool never re-renders a CID you passed (it echoes the argument, base
  unchanged, and only shows the store's base32hex when it had to derive a CID
  from a name). It is the decode half of `ipfs cid format` / the cid.ipfs.tech
  inspector, narrowed to the spec's CIDv1 parts. In `main_cid.go`
- `blob get <cid|name> -o <out>` — retrieve; argument is parsed as a CID first,
  falling back to the name index (so `get` round-trips a name passed to `add`);
  `-o` defaults to using the argument as filename; `--only-size` and
  `--only-mtime` print the blob's size (bytes) or mtime (Unix seconds) without
  retrieving it — the mtime is the store's last-modified time for the blob (when
  it was added), 0 if the backend does not track one
- `blob list` — print the stored names, one per line. Lists the **index**, never
  the CAS: blobs with no name are invisible, blobs with several names list once
  per name. Backend-specific mechanics: dir reads the `index/` directory, sql
  does `SELECT name FROM names ORDER BY name`, http GETs the JSON autoindex of
  `<base>/index/` (the server must have autoindex on **and** JSON format — see
  BACKEND.md), s3 lists the bucket with prefix `index/`. Results are sorted
  lexicographically on every backend (the contract says sorted, so a server that
  does not sort is still correct on the wire)
- `blob lookup <name>` — convenience: name → the stored blob's CID (peek first
  byte for the codec, then compose + render; not required, the CID is
  recomputable from the store). Same `Peek`-based derivation the `/name` gateway
  uses
- `blob serve [--listen/-l <host:port>]` — start a read-only HTTP gateway;
  default `127.0.0.1:8080`. The serving logic lives in the `daemon` package (a
  testable `Server`; `main` only opens a read-only backend, builds the
  `daemon.Server`, and listens until a signal — see `main_serve.go`). Serves
  `GET /blob/<CID>` → the blob, so a stable `blob://<CID>` scheme maps to
  `http://<host>/blob/<CID>` without the client knowing the backend. Serves
  `GET /name/<name>` additionally, which `Peek`s the first byte to derive the
  CID and `302`-redirects to the canonical `/blob/<CID>` (302 not 301, since
  last-add-wins makes the mapping mutable); the `Location` header is the
  discoverable "real path" (like a dir symlink / sql FK). Read-only: only `Get`/`Peek`
  are used, there is no upload/`add` endpoint. Also serves the OCI distribution
  read API under `/v2/`: `GET /v2/` (ping),
  `GET|HEAD /v2/blob/<CID>/manifests/<ref>` +
  `GET|HEAD /v2/blob/<CID>/blobs/<sha256:...>` (primary), and the same two paths
  with `blob/<CID>` → `name/<name>` (secondary). `<ref>` is `latest` (the only
  tag; we have no name:tag split yet) or an OCI digest (`sha256:<hex>`); the
  inner blobs are always addressed by their `sha256` digest, never by a CID. **On
  the OCI manifest response the store's CID is discoverable via the `X-Blob-Cid`
  header** (base32hex, the same value the `/name/<name>` 302 `Location` carries)
  — needed because the spec's `Docker-Content-Digest` is `sha256:<hex>` and
  carries no codec, so it does not pin the CID (Json vs Raw is decided by
  first-byte); the `X-` prefix keeps it outside the distribution-spec namespace,
  and OCI clients ignore unknown response headers so it can't break a pull. The
  primary repo (`blob/<CID>`) is the *exact* CID that `oci import` printed — the
  same CID the HTTP surface serves at `/blob/<CID>`. This is the design behind
  nerdctl's IPFS serving (they use `/ipfs/<CID>`, we use `/blob/<CID>`). No
  push, no tags beyond `latest`, no auth, no HTTPS (the server only listens on
  localhost by default); the client is expected to treat the base as a trusted
  plain-HTTP registry. See `daemon/`.
- `blob oci import <layout-dir | layout.tar | file> [--name N]` — import an OCI
  layout (a dir, a `docker save`/`podman save` tar — the v25+ "hybrid"'s OCI
  half is read, its docker half ignored; or a bare file, wrapped as a
  single-layer OCI artifact) into the store, storing every reachable blob (index
  → manifest → config → layers). Naming is by the object handed us: a *layout*
  (dir/tar) records its basename (`.tar` stripped) plus each repeatable `--name`
  against its **index CID**; a *bare file* records its basename plus each
  `--name` against the **file's own bytes** (the artifact's single layer — the
  same key `blob add` would record, so `oci import f` and `blob add f` share a
  name, and `blob get f` keeps returning the file, not the OCI scaffold). The
  printed CID is always the layout's index — the OCI scaffold's address and the
  handle for `oci export`/`oci ls`/`oras pull` of the layout (for a bare file,
  `oci export <file-name>` of the wrapped artifact would 404 since the name no
  longer resolves to the scaffold; use the CID). Alias `oci add`
- `blob oci export <cid|name|digest> -o <dir | file.tar>` — export a stored OCI
  layout to a directory or a tar (the layout's `index.json` + reachable blobs).
  Alias `oci get`. Argument is a CID, an index name, or an OCI digest (`sha256:<hex>`)
- `blob oci ls <cid|name>` — list a stored layout's index, manifests, config and
  layer descriptors (read-only; no blobs written)
- Global flags: `--storage`/`-s dir|sql|http|s3` (default `dir`), `--path`/`-p`
  (default `./.blob-store`); for the `http` backend `--path` is the base URL of
  the server (e.g. `http://localhost:8080`) and the store lives on the remote
  side; for the `s3` backend `--path` is the S3/RGW endpoint (host:port or an
  `http(s)://` URL) plus `--bucket`/`-b` (the named store; or `BLOB_BUCKET`)
  and, for region-aware S3, `--region`/`-r` (or `BLOB_S3_REGION`), left blank to
  defer to the server. Remote backends additionally take `--user`/`-u` +
  `--pass` (or `BLOB_USER`/`BLOB_PASS`; http also `user:pass@` in `--path` or
  `~/.netrc`) — Basic-auth for `http`, access/secret key for `s3`
- Env defaults: `BLOB_STORAGE_TYPE` (type), `BLOB_STORAGE_PATH` (path), and for
  s3 `BLOB_BUCKET` / `BLOB_S3_REGION` are used as the flag defaults when the
  flag is not explicitly set; the flag always wins; `BLOB_USER`/`BLOB_PASS` feed
  the remote-backend credentials

## Non-Obvious Details

- CID codec depends on content, not extension: file stored with `Json` codec
  only if first byte is `{` or `[` (`detectCodec` in main.go); anything else is
  `Raw` (why: DESIGN.md)
- The storage key is the **multihash** (algorithm + digest bytes), opaque
  `[]byte` (`storage.Digest`). The storage package has no multiformats
  dependency — it never decodes the multihash, so adding a different hash
  algorithm (or a different codec) is a `main.go` change only, never a storage
  change. `computeHash(path) → multihash.Multihash` produces the bytes;
  `detectCodec(path) → multicodec.Code` picks the content-based codec (Json if
  first byte is `{`/`[`, else Raw); `computeCID(mh, mc) → cid.Cid` builds the
  CID (version + codec + multihash — a CID carries no base, that is a
  render-time parameter); `stringFromCID(c) → base32hex string` is the single
  point where this store's presentation base is chosen (`StringOfBase(multibase.Base32hex)`)
  — our choice: the `base32hex` alphabet `0-9a-v` (5 bits/char; first 16 chars
  `0-9a-f` = base16hex) instead of the `base32` alphabet `a`–`z` + `2`–`7`
  (go-cid's `String()` default for v1, the one IPFS uses), so CIDs read as an
  extension of hex; the storage key is `c.Hash()` (the multihash itself,
  rendered base16hex `0-9a-f` on disk / the wire), and `digestFromCID` inverts
  that at `get` time.
- Dir backend key is the hex of the multihash bytes (not the base32hex CID),
  fanned out across up to 3 one-byte levels (`fanoutDepth`), the rest as
  filename: `.blob-store/<b0>/<b1>/<b2>/<rest>` (e.g. `12/20/<byte3>/<rest>` for
  a sha2-256 multihash), written via tmp-file + rename. The fanout carves the
  opaque key bytes into 1-byte dir levels — encoding-agnostic, ≤256 per level,
  no knowledge of the hash algorithm
- SQL backend auto-creates the parent dir of `--path` and the
  `blobs (hash BLOB PRIMARY KEY, data BLOB, mtime INTEGER NOT NULL DEFAULT (strftime('%s','now')))`
  table (storage/sql.go); no migration steps to run. `Get` returns that `mtime`
  as Unix seconds alongside the size
- HTTP backend (`storage/http.go`) is a thin proxy over plain GET/PUT — no
  multiformats, no local files. On-wire contract (its own, independent of the
  user-facing `serve` CID gateway): blobs at `<base>/<hex>` (PUT body=blob, GET
  streams it, HEAD for existence → `Has`), name index at `<base>/index/<name>`
  (PUT body = the hex multihash, the same name→hash *link* as the dir symlink /
  sql foreign key, expressed as file content rather than a symlink;
  last-add-wins via PUT-overwrite). The hex encoding is chosen only to keep the
  wire simple and align with the dir backend; the server stores however it likes
  (no fanout here). `--path` is the base URL. The `dir` backend's fanout and
  symlink are its *realization* of the same store/index concepts; http has
  neither (flat path, content-as-link), so the two are similar in concept, not
  in layout. `Close()` drops idle keep-alive connections (the pool analogue of
  sql closing its pool). A shared `*http.Client`/`Transport` makes one
  HTTPStorage safe for concurrent use, so the shared read-only gateway can hand
  it to every request goroutine like the pooled sql backend
- S3 backend (`storage/s3.go`) speaks the S3 object API via
  `github.com/minio/minio-go/v7` (primary target Ceph RADOS Gateway; MinIO and
  AWS also work). Like http it is a thin remote proxy — no multiformats, no
  local files — and the multihash stays opaque: rendered to hex only to form
  object keys (blob `<bucket>/<hex>`, index `<bucket>/index/<name>` with body =
  the hex hash), so it mirrors the http on-wire contract with one level of
  indirection (the bucket). `--path` is the endpoint (`host:port` → http,
  `https://...` → TLS), `--bucket` the named store, `--user`/`--pass` the
  access/secret key (SigV4), and `--region` the SigV4 region (blank = minio-go
  defers to the server's bucket location). `Get` streams via `GetObject` (an
  `io.ReadCloser`) and reads the size with a follow-up `Stat` (surfacing the 404
  there, since `GetObject` defers the request to the first read); `Peek` is a
  ranged `GET` (bytes=0..n-1). Not-found is detected from
  `minio.ToErrorResponse(err).Code == NoSuchKey` (a bare 404 with no S3 XML
  already maps there, which is why the test shim can return a plain 404).
  `Close()` is a no-op (minio-go owns its own client/transport); a single
  `*S3Storage` is safe for concurrent use since minio-go internally
  serializes/token-buckets, so the shared gateway can hand it to every request
  goroutine
- **`index/` holds the name-index conventions** (`index/index.go`): `Name` is an
  alias of `string` (like `identity.Identity` = `[]byte`, the name carries no
  value of its own — the package carries the conventions around it),
  `index.Check(nm, target) → (skip, err)` is the single place where
  recordability is decided (no control characters; a name that *looks like a
  native handle* — a sha256 OCI digest, or a CID of a held codec — is reserved
  as that blob's direct address: recorded only if it is exactly `target`, in
  which case it is *skipped* since the entry would be redundant; a CID of a
  non-held codec, e.g. `cid.CodeDagPb` from `ipfs add`, may name a blob because
  the resolver never intercepts it — the interop handle), and
  `index.Default(arg)` derives the name from an argument (basename, `.tar`/`.tgz`/`.tar.gz`
  stripped). **Registry-style names are legal** (a name may contain `/`, `@`,
  `:`, `.`, Unicode…): `index.Encode(nm) → string` and
  `index.Decode(seg) → Name` are the single source of truth for the clean-name ↔
  encoded-segment step, so every backend keeps the index a *single flat level*
  (the segment is a dir filename, a sql column, an http URL segment, an s3 key)
  — `Encode` escapes `/` → `%2F`, `%` → `%%`, everything else literal (URI-style
  percent convention, byte-wise, the identity on names with no `/`/`%`, so
  pre-escape stores are untouched); `Decode(Encode(nm)) == nm` strictly (the
  only escape pair), and is tolerant on the decode direction (a stray `%` is
  passed through literally, so a store predating `Encode` still reads). The
  encoders/backends *never* name a `index/` subdirectory — a name with `/` never
  creates a hierarchy, it becomes one flat segment (`docker.io/library/busybox`
  → `docker.io%2Flibrary%2Fbusybox`). A name's **encoded segment cap** is 255
  bytes (POSIX `NAME_MAX`); `Check` enforces `len(Encode(nm)) ≤ 255` once so
  every backend holds the same bound by the one package that owns the
  convention. Backends apply the escape at their boundary: `Index`/`Lookup` call
  `index.Encode(name)`, `Names` calls `index.Decode(seg)` on each row/entry —
  `main`, `daemon`, and the CLI's `list`/`get`/`lookup` commands never see the
  segment. The http backend additionally puts the segment on the wire with
  `url.PathEscape(Encode(name))` (layer B, the URI percent-escape on top of
  ours): `a%2Fb` in our layer becomes `a%252Fb` in flight, and the server
  decodes once to recover the flat segment (a static server serving the flat
  file `a%2Fb` decodes `a%252Fb` to `a%2Fb`, no hierarchy). `Encode` is the
  identity on names that need no escaping, so plain names address exactly the
  URL/filename/column/key they did before. `main` no longer knows the rule —
  `runAdd` and `oci import` both call `index.Check`; `oci import` additionally
  records the basename *plus* each repeatable `--name` (de-duplicated against
  the basename) all pointing at the same index CID.
- Name index is separate from the CAS and stores the blob **key** (the multihash
  bytes — `storage.Digest` = `[]byte`), *not* a CID string, so each backend
  resolves a name to a blob with nothing but its own data: dir backend uses
  `.blob-store/index/<name>` symlinks pointing `../<b0>/<b1>/<b2>/<rest>` (i.e.
  `../` + relPath) at the blob (written as symlink + rename on a `.tmp` name),
  sql backend uses a
  `names (name TEXT PRIMARY KEY, hash BLOB NOT NULL REFERENCES blobs(hash))`
  table (foreign key, `PRAGMA foreign_keys = ON` on each connection), http
  backend stores the hex multihash as the body of a single object at
  `<base>/index/<name>` (a PUT-overwrite — last add wins), and the s3 backend
  does the same at `<bucket>/index/<name>`. Key is the file basename (expected
  to be a human-readable name like `ubuntu.sif`, not a CID, so `get`'s CID-first
  lookup is unambiguous); two names can map to one key and **last add wins** (`Index`
  overwrites), so `latest`-style aliases stay current. `runAdd` indexes after
  confirming the blob is stored (so a `Put` failure leaves no dangling name),
  and still indexes when the blob already exists. The index is also *listable*
  via `StorageBackend.Names()` (the `list` command): it returns the recorded
  names — the key set of the name→blob index, and never the blob keys — as a
  sorted slice, empty (not an error) when there are no names; each backend reads
  its own form (dir `ReadDir`s `index/`, sql
  `SELECT name FROM names ORDER BY name`, http GETs the JSON autoindex of
  `<base>/index/` — the server must have autoindex on **and** JSON format, see
  BACKEND.md; s3 lists with prefix `index/` and strips the prefix). The
  multihash is the key only — the CID (codec + multibase) is derived and is not
  stored; the dir index symlink follows to content directly, while sql needs a
  join on `hash` (the FK exists for integrity). Symlinks are Linux-only here;
  Windows fallback (git's `core.symlinks`-style text target in the file) is the
  intended migration if ever needed. Per-name files (one row in sql) instead of
  a shared `index.json` (as OCI registries use): each update is an atomic
  single-key write (tmp + rename / primary-key upsert), no read-modify-write of
  one JSON document. This is fine for this store's scale; IPFS avoids any index
  at this layer since it is content-addressed and distributed, and only gains
  name resolution via IPNS/DNSLINK — a much larger mechanism (signatures, keys,
  global lookup) that local `add`/`get` round-tripping does not need
- `StorageBackend` exposes `Close()`: the `sql` backend closes its connection
  pool (interrupting in-flight reads), the `http` backend's `Close()` drops the
  client's idle keep-alive connections (in-flight transfers are owned by their
  request/response), the `dir` backend's `Close()` is a no-op, and the `s3`
  backend's `Close()` is a no-op too (minio-go owns its client/transport).
  `main.go` calls `defer backend.Close()` right after constructing the backend
  in `runAdd`, `runGet`, `runLookup`, and `main_serve.go`'s `runServe` (and the
  `daemon` package never Closes a backend — the caller does, once, after the
  server shut down)
- The `sqlite` backend streams reads via `db.OpenBlob(..., write=false)`
  (returned as the `io.ReadCloser` from `Get`) instead of `ColumnBytes`, so a
  blob is never materialized in memory. `Get` opens the blob read-only and
  closes it through `reader.Close()` before the backend's `Close()` runs
  (declared in that order)
- Backends open in an explicit `Mode` (`storage.ReadOnly` / `storage.ReadWrite`):
  `add` opens ReadWrite, `get`/`lookup`/`serve` open ReadOnly (a read-only
  backend refuses `Put`/`Index` with `storage.ErrReadOnly`, and neither creates
  the store). `main_serve.go`'s `runServe` opens **one shared ReadOnly backend**
  for the server's life (single open + `defer Close` in `runServe`), handed to
  the `daemon.Server` (which then hands it to every request goroutine). This
  works because the `sql` backend is now a `zombiezen.com/go/sqlite/sqlitex` **`Pool`**
  (size 10), not one `*sqlite.Conn` (which is a single C handle, not safe
  concurrently — sharing it SIGSEGV'd, verified): each op takes a conn, does its
  work, puts it back; `Get`'s reader keeps its conn checked out until
  `reader.Close()` (`connReader` in sql.go), so streaming blobs across the pool
  is safe. Read/write overlap is via WAL (`OpenWAL` on ReadWrite opens), so an
  `add` does not block concurrent readers. The `dir` backend needs no locking
  (one file handle per op, atomic tmp+rename), and the `http` and `s3` backends
  are both safe to share across goroutines (http via its pooled client's
  keep-alive connections, s3 via minio-go's internal request serialization) —
  each `Put`/`Index` writes a distinct key, so there is no cross-key contention
  to guard.
- **Five thin packages sit between `storage` and `main`** (`main` is the
  CLI/adapters only). `identity/` is the single place that knows both spellings
  of a key — the store's multihash (`go-multihash`) and the OCI digest (`go-digest`):
  `HashReader` (content→key), `FromOCI`/`ToOCI` (digest↔key), and `Check`
  (stream-verify). It is sha256-only *by construction* — the algorithm gate is
  one `case` site each direction, so adding sha3 later is a change confined here
  (the bytes/`Identity` type never change). `cid/` is the single place that
  knows the store's CID conventions over go-cid: `Build`/`Parse`/`Render` (the
  base is chosen once, in `Render` = base32hex), plus the content-based codec
  rule (`CodecForFirstByte`, `Supported` — which also names `CodeDagPb`, the
  known-but-not-held codec whose CIDs are the interop handles, `MediaType`,
  `ContentDigestHeader`) and `DigestFromCid`. `index/` is the single place that
  knows the name-index conventions: `Name` (an alias of `string`), `Check`
  (recordability: no control characters, the encoded-segment cap, digest/CID
  handles reserved or allowed per the resolution order), `Default` (basename,
  tar suffixes stripped), and `Encode`/`Decode` (the clean-name ↔
  encoded-segment step for registry-style names; backends apply it at their
  boundary) — built over `identity` and `cid`, so the digest-shape and CID-shape
  decisions live beside the ones they bridge. `oci/` is the pure OCI half: types
  (`Index`/`Manifest`/`Descriptor`), `Parse*`, `Reachable`, `Copy`,
  `NewArtifact`, the `DirLayout`/`TarSink`/`TarFileSource` adapters, and
  `ListMembers`/`ReadMember` — built only on `go-digest` + stdlib (`archive/tar`,
  `compress/gzip`). It never names a multihash, a CID, or a storage backend, so
  it is tested in isolation against in-memory layouts. `daemon/` is the fourth:
  the testable serving layer that sits over `storage` and speaks **both** the
  store's CID and OCI (like `identity` and `oci` already do, so there is no
  artificial boundary — only the one natural format seam, which is why the
  `daemon` package may name both a multihash and a go-digest). It exposes a
  `Server` (open a `ReadOnly` backend, `New(backend)` once, then `Handler()` for
  the mux or `ListenAndServe(ctx, addr)` to run), serving the CID gateway (`/blob/<CID>`
  streaming with `Content-Digest`/`Content-Type`/`Content-Length`/`Last-Modified`;
  `/name/<name>` resolving and 302-redirecting to the canonical `/blob/<CID>`)
  and the OCI distribution read surface (`/v2/` ping, manifest by ref, blob pull
  — see the `blob serve` bullet for the endpoint list and the `X-Blob-Cid`
  header for the CID). Because it only reads (`Get`/`Peek`/`Lookup`) and speaks
  the `storage.StorageBackend` interface, it is tested against an in-memory
  backend (`daemon_test.go`) and, opt-in, over a real socket + `curl` (`daemon_smoke_test.go`)
  — the daemon is the `storage` analogue of "the CLI minus the CLI", and `main`
  keeps only the adapters: `main_serve.go` opens a backend from the storage
  flags, hands it to `daemon.New`, and runs `ListenAndServe` until a signal.
- **OCI import/export (`oci import|export|ls`, aliases `add`/`get`, in
  `main_oci.go`; mirrors `ctr images import`/`export` and Docker/Podman `load`/`save`).**
  An OCI layout is the same CAS idea the store already speaks: an `index.json`
  naming a manifest, which names a config + layers, each a blob addressed by its
  `sha256:<hex>` digest. Because `sha256:<hex>` is exactly the store's sha2-256
  multihash key minus the 2-byte `12 20` prefix, the `identity` bridge is a
  re-encode, **not a re-hash** — so an OCI import and a plain `add` of the same
  bytes land on the same store key and dedupe for free. `oci import` walks
  `index → manifest → config → layers` (`oci.Reachable`) and stores the
  reachable set: **unreferenced** blobs are left out, and a v25+ `docker save`
  "hybrid" / `podman save` tar is read by its OCI half — the docker archive
  fields (`manifest.json`, `repositories`) are simply never walked (and
  `TarFileSource` only remembers layout-shaped entries). A bare file (not a
  layout) is wrapped as a single-layer **artifact**: config = the canonical `{}`
  blob, one layer, `artifactType` = the layer's type, and the basename (plus any
  `--name`) record against the *file's bytes* — its single layer — the same key
  `blob add` uses, so `oci import f` and `blob add f` share a name and cannot
  clobber each other. `oci export` is the inverse —
  `Copy(src=store, sink=layout)`, writing a dir (`oci-layout`/`index.json`/`blobs/sha256/*`)
  or a tar. **Blobs are streamed end-to-end** (a layer may be GB/TB); only
  `index.json` and manifests are small enough to buffer. There is **no registry
  I/O in these commands** (no `oras`-style `docker://`/`oras://` fetch/push, no
  tags): input is the OCI layout on a file/dir, output the same — serving that
  layout over the wire is `serve`'s read-only `/v2/` surface (see the
  `blob serve` bullet above), a separate concern. `oci import`/`export`/`ls`/`get`
  all accept `<cid|name|digest>` (`sha256:<hex>`), resolved in that order via
  `identity.FromOCIString`. **Media types** are sniffed, not invented: the
  store's one recognized special case is a **SIF** (`SIF_MAGIC\x00` →
  `vnd.sylabs.sif.config.v1+json` + `vnd.sylabs.sif.layer.v1.sif`); everything
  else is generic (`vnd.unknown.config.v1+json` config +
  `application/octet-stream` layer) — the OCI artifact "config" is `{}` and its
  content is ignored, only the media type carries the meaning.

## Conventions

- Go 1.26; `blob` binary and `.blob-store/` are gitignored
- Verify changes in order, cheapest first: gofmt/goimports the edited files →
  `go build ./...` → `go test ./...` (for package changes) → `golangci-lint run`
  (heaviest; the final gate, if available)
