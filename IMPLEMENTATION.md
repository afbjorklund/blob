# Implementation

How the code is organized and how the pieces fit together. Usage:
[README.md](README.md). The ideas (content addressing, CAS, name index,
gateway): [CONCEPTS.md](CONCEPTS.md). Why the design is the way it is:
[DESIGN.md](DESIGN.md). The backends and their on-wire contracts:
[BACKEND.md](BACKEND.md).

## Package map

Six thin packages sit between `storage` and the `main` package
(`main` is the CLI and the adapters only):

| package     | knows                            | role                                                             |
|-------------|----------------------------------|------------------------------------------------------------------|
| `storage/`  | bytes only (no multiformats)     | the four backends + the `StorageBackend` interface they all fill |
| `identity/` | the multihash and the OCI digest | the single place that knows *both* spellings of a key            |
| `cid/`      | go-cid + `identity`              | this store's CID conventions over go-cid                         |
| `index/`    | `identity` + `cid`               | the name-index conventions: recordability, default name, escape  |
| `oci/`      | go-digest + stdlib only          | the OCI layout half: types, walk, copy, layout adapters          |
| `daemon/`   | `storage` (read-only)            | the testable serving layer: CID gateway + OCI distribution read  |
| `mount/`    | `storage` (read-only)            | the FUSE read surface: `name/` (symlinks) + `blob/<CID>` (files) |

The split is per *library each package may touch*: `storage/` never decodes
the multihash, `oci/` never names a CID or a backend, `identity/` is the one
point where the two worlds (multihash ⇄ OCI digest) meet, and `cid/` holds
this store's CID conventions in one place. Each is a thin layer over its
library — the same way go-cid, go-multihash, and go-digest are each thin
over their spec. Adding a second hash algorithm is a one-`case` change in
`identity/` only; the `Identity` bytes never change.

## identity/

The multihash ⇄ OCI digest bridge over `[]byte` (`identity.Identity` is an
alias of `[]byte`; the type carries no value of its own).

- `HashReader(r) → Identity` — content → key (streams `io.Reader`, sha256).
- `FromOCI(d digest.Digest)` / `FromOCIString(s)` / `ToOCI(id)` — digest ↔
  key re-encode, **not a re-hash**: `sha256:<hex>` is this store's sha2-256
  multihash minus the 2-byte `12 20` code/size prefix. So an OCI import and a
  plain `add` of identical bytes land on the same CAS key and dedupe for free.
- `Check(r, expected) bool` — stream-verify a reader against a key.

The algorithm gate lives here (one `case` each direction): sha256 today, a
second algorithm confined to this package.

## cid/

This store's CID conventions over go-cid, built on `identity/`:

- `Build(codec, key)` / `Parse(s)` / `Render(c)` — the **base is chosen once,
  in `Render`** (= `StringOfBase(multibase.Base32hex)`): the `0-9a-v`
  alphabet, whose first 16 characters are base16hex, so CIDs read as an
  extension of hex (IPFS's default is `base32`, `a`–`z` + `2`–`7`). A CID
  carries no base — that is a render-time parameter.
- `CodecForFirstByte(b)` — the content-based codec rule, single place:
  first byte `{` or `[` → `Json`, anything else → `Raw`
  (`main.go`'s `detectCodec` peeks the byte and calls it).
- `Supported(code)` — the held codecs (`Raw`, `Json`) plus `CodeDagPb` named
  as the *known-but-not-held* codec: a `dag-pb` CID is decodable and may even
  name a blob (the interop handle from `ipfs add`), but its content is not
  what this store holds.
- `MediaType(c)` — codec → `Content-Type` (`Json` → `application/json`).
- `ContentDigestHeader(c)` → `sha-256=<base64>` — the `Content-Digest`
  response header (RFC 8283 form).
- `DigestFromCid(c)` → `godigest.Digest` — CID → the OCI spelling (via
  `identity.ToOCI`).

## index/

The name-index conventions (built on `identity` + `cid`, so the digest-shape
and CID-shape decisions live beside the ones they bridge):

- `Name` (alias of `string`) — a name carries no value of its own; the
  package carries the conventions around it.
- `Check(nm, target) (skip, err)` — the single recordability decision: no
  control characters; the *encoded segment* cap of 255 bytes (POSIX
  `NAME_MAX`) is enforced **here once**, so every backend holds the same
  bound; a name that *looks like a native handle* — a sha256 OCI digest, or a
  CID of a held codec — is reserved as that blob's direct address and is
  only recorded if it is exactly `target` (in which case `skip` — the entry
  would be redundant); a CID of a non-held codec remains legal to record
  (the resolver never intercepts it).
- `Default(arg)` — the name from an argument: basename, `.tar`/`.tgz`/
  `.tar.gz` stripped.
- `Encode(nm) → string` / `Decode(seg) → Name` — the clean-name ⇄
  encoded-segment step, the single source of truth. `Encode` is the identity
  on names that need no escaping; otherwise `/` → `%2F`, `%` → `%%`
  (URI-style percent convention, byte-wise, so `Decode(Encode(nm)) == nm`
  strictly). Backends apply the step at their boundary — `Index`/`Lookup`
  call `Encode`, `Names` calls `Decode` per row — so `main`, `daemon`, and
  the CLI's `list`/`get`/`lookup` never see a segment. The http backend
  puts the segment on the wire with `url.PathEscape(Encode(name))` (a second
  URI layer on top of ours): see [BACKEND.md](BACKEND.md). The store stays a
  *single flat level* on every backend — a name with `/` never creates a
  hierarchy, it becomes one flat segment (`docker.io/library/busybox` →
  `docker.io%2Flibrary%2Fbusybox`). `Decode` is tolerant: a stray `%` is
  passed through literally, so a store predating `Encode` still reads.
- `IsOciRef(nm)` / `ParseOciRefName(nm)` / `BuildOciName(...)` /
  `OciVariantValid(v)` / `NormalizeOciDigest(d)` / `NormalizePlatform(p)`
  (in `oci.go`) — the origin-reference name conventions: the
  `oci/<variant>/<os>/<arch>/<algo>/<hex>` shape a user records with
  `--upstream --arch`. The shape is checked once, here, so the write path
  (`main.go`, `main_oci.go`) and the FUSE tree (`mount.go`) agree on it.
  An origin reference is an ordinary name (one flat segment after
  `Encode`), not a backend-specific construct; the `variant` component is
  what the leaf names (`blob` / `image` / `artifact`), disambiguating e.g.
  a SIF and an OCI layout of the same upstream image.

## oci/

The pure OCI half (go-digest + stdlib — `archive/tar`, `compress/gzip`; it
never names a multihash, a CID, or a storage backend):

- Types + `ParseIndex` / `ParseManifest` — `Index`, `Manifest`, `Descriptor`.
- `Reachable(src, indexBytes)` — the walk `index → manifest(s) → config +
  layers` (the *storeable* set; unreachable blobs are left out — how the
  v25+ "hybrid"'s docker half is simply never read).
- `Copy(src, sink, indexBytes)` — the inverse, streaming end-to-end (a layer
  may be GB/TB; only `index.json` and manifests are small enough to buffer).
- `NewArtifact(...)` — wrap a bare file as a single-layer OCI artifact: the
  canonical `{}` config, one layer, *no* `artifactType` field (the image-spec
  lets tooling fall back to `config.mediaType`, which is always set). Media
  types are **sniffed, not invented**: one recognized special
   case — SIF (`SIF_MAGIC\x00`, optionally behind the 32-byte
   `#!/usr/bin/env run-singularity\n\x00` launcher stub that Apptainer and
   Singularity prepend to executable images → `vnd.sylabs.sif.config.v1+json`
   config + `vnd.sylabs.sif.layer.v1.sif` layer); everything else generic
   (`vnd.unknown.config.v1+json` config + `application/octet-stream` layer).
   *If we ever adopt the empty-config media type* (whose fallback would be
   meaningless), we would have to set `artifactType` again.
- `NewDir` (`DirLayout`) / `NewTarSink` / `NewTarFileSource` — the
  layout adapters (`oci.Source`/`oci.Sink`).
- `ListMembers` / `ReadMember` — layer membership (tar listing / one member
  out).

## daemon/

The testable read/serving layer over `storage.StorageBackend` (read-only —
only `Get`/`Peek`/`Lookup` are used; `main` keeps the adapters:
`main_serve.go` opens the backend, builds `daemon.New(backend)`, listens
until a signal; the daemon never Closes a backend):

- `New(backend) → Server`, then `Handler() http.Handler` or
  `ListenAndServe(ctx, addr)`.
- The CID gateway: `GET /blob/<CID>` streams the blob with `Content-Type`
  (from the CID codec), `Content-Length`, `Content-Digest`, `Last-Modified`;
  `GET /name/<name>` resolves and peeks the first byte to derive the CID and
  `302`-redirects to the canonical `/blob/<CID>` (mutable mapping, hence 302
  not 301).
- The OCI distribution read surface under `/v2/`: ping, manifest by ref, and
  blob pull, by CID (`blob/<CID>`, primary) or name (`name/<name>`,
  secondary) — `<ref>` is `latest` (the only tag) or an OCI digest. Two
  deliberate choices worth knowing from the inside: the inner blobs are
  always addressed by their `sha256` digest, never by a CID (the spec says
  so); and the store's CID is discoverable via the **`X-Blob-Cid`** header on
  the manifest response (base32hex, the same value the `/name` 302
  `Location` carries) — needed because the spec's `Docker-Content-Digest` is
  `sha256:<hex>` and carries no codec, so it does not pin the CID; the `X-`
  prefix keeps it outside the distribution-spec namespace, and OCI clients
  ignore unknown response headers.
- Because it only reads, it is tested against an in-memory backend
  (`daemon_test.go`) and, opt-in, over a real socket + `curl`
  (`daemon_smoke_test.go`, `BLOB_SERVE_SMOKE=1`).

## mount/

The FUSE read surface — the filesystem twin of `daemon/`. It takes the same
`storage.StorageBackend` (read-only), presents the same two namespaces
(`name` and `blob`), but encodes them as a POSIX tree instead of an HTTP mux.
Built on `bazil.org/fuse` (pure Go; no CGO/libfuse) and `bazil.org/fuse/fs`;
Linux-only (the kernel FUSE subsystem is a Linux feature, and `fuse.Mount`
returns an error elsewhere).

- `New(backend) → *FS`, then `Root()` (the `fs.FS` entry) or
  `Mount(dir)`/`Serve`/`ServeContext` for the protocol loop.
- **Two top directories** (the `fs` tree is a fixed skeleton, built in
  `rootNode.Lookup`):
  - `name/` lists the index (one symlink per `Names()` entry) and each
    symlink's target is `../blob/<CID>`. The target is *relative* because
    `name/` and `blob/` are siblings under the mount root, so `..` stays
    inside the mount regardless of where it is mounted. The CID is derived
    exactly like `daemon` derives it for `/name/<name>` (one `Peek` for the
    first byte → `cid.CodecForFirstByte` → `cid.Render`). The `name` entry
    is therefore a FUSE symlink — the filesystem form of the daemon's
    `/name` `302`.
  - `blob/` is *not listable*: it has no `ReadDirAll`; `Lookup(<CID>)`
    returns a `blobFile` that streams on read. This is deliberate and
    mirrors `daemon` (the `/v2/blob/<CID>` surface also addresses by
    CID without listing) and the `storage` interface (`Names()` is the
    index, not the CAS; there is no "list all blobs" method).
- **Streaming, forward-only, page-bounded reads.** `blobFile.Open` does a
  single `backend.Get` and hands the `io.ReadCloser` to a `blobHandle`,
  which serves each kernel `Read` (page-sized) by reading that page off the
  current position of that shared forward reader. The whole blob is never
  buffered: `resp.Data` is `min(page, remaining)` at a time, and
  `io.ReadFull` only pulls up to that — a GB blob costs the kernel a
  page-by-page pull, exactly like `daemon.serveBlob`'s `io.Copy` loop.
   The `blobFile.Attr` (used for `fstat`) calls `backend.Stat` (metadata only,
   no reader opened or left open), so `stat` costs one metadata round trip.
- **Handle lifetime (the subtle part).** FUSE sends a `Flush` on each
  `close(2)` of a fd that refers to the handle, and **`Release` on last
  fd close**. A handle can therefore have several fds per open; only
  `Release` means "open over." We release the backend reader (returning
  the pooled sql connection / dropping the remote transfer) on
  **`Release`**, not `Flush`. `Flush` is a no-op. `Read` and the two
  handle ops are serialized by a mutex and a `closed` flag so a post-
  `Release` in-flight `Read` returns `EIO` (not a nil deref).
- **Writes are refused end-to-end.** The mount is opened with the FUSE
  `ReadOnly()` option (kernel-side `ro`), and — like `daemon` — the
  backend is opened `storage.ReadOnly`, so *both* layers refuse
  `Put`/`Index`/writes even if the mount flag were dropped.
- **Testing.** `mount_test.go` exercises the tree and handle against an
  in-memory `storage.StorageBackend` (same pattern as `daemon/`): the
  root skeleton, the `name/` symlink targets (asserted against an
  independent CID derivation), and the `blob/` streaming read (assembled
  pages must be byte-identical); plus the `Release`-as-teardown contract
  (a spurious `Read` after `Release` is `EIO`, and a `Read` after a
  `Flush` is still served — i.e. `dd` works, not just `cat`).
  `mount_smoke_test.go` (`BLOB_MOUNT_SMOKE=1`, needs `/dev/fuse` and the
  `fusermount3` setuid helper) runs the same thing over a real dir
  backend and real FUSE connection, probing with `ls` / `stat` / `cat` /
  `touch`, which is the shell-side view a user would exercise.

## Storage internals (per backend)

BACKEND.md describes the *contract*. The Go-side mechanics:

- **Modes.** Backends open in an explicit `Mode`
  (`storage.ReadOnly` / `storage.ReadWrite`) via `NewStorage`: `add` opens
  ReadWrite, `get`/`lookup`/`serve`/`mount` open ReadOnly. A ReadOnly backend
  refuses `Put`/`Index` with `storage.ErrReadOnly` and neither creates the
   store nor touches it. `Get` streams and returns `(io.ReadCloser, Stat,
   err)` — `Stat` carries the size, the store's last-modified time in Unix
   seconds (0 when the backend does not track one), and the local filesystem
   path (dir only; empty on sql/s3/http — the blob is a row / object / remote
   body, not a local file). A caller that wants the probe fields but not the
   content uses `Stat(hash) (Stat, error)` instead: no reader is opened or
   discarded (dir: `os.Stat` with no fd; sql: the metadata SELECT, no blob
   opened; http: `HEAD`, no body; s3: `StatObject`), which is strictly cheaper
   than `Get` + `Close`. `get --only-size` / `--only-mtime` / `--only-path`,
   the mount's `fstat`, `ls`, and the OCI export/list scaffold all take the
   `Stat` path; streaming consumers (`get`'s download, `serve`, `cat`,
   `oci` Copy) keep `Get`.
- **Concurrency, per backend.** `sql` is a `zombiezen.com/go/sqlite/sqlitex`
  **`Pool`** (size 10), not one `*sqlite.Conn` (a single C handle, not safe
  concurrently): each op checks a conn out and back; `Get`'s reader keeps
  its conn checked out until `reader.Close()` (`connReader`). Read/write
  overlap is via WAL (`OpenWAL`). `dir` needs no locking (one file handle
  per op; writes are tmp-file + rename). `http` shares one `*http.Client` —
  a `*HTTPStorage` is safe for concurrent use; each `Put`/`Index` writes a
  distinct key. `s3` is safe the same way via minio-go's internal
  serialization.
- **Not-found, per backend.** sql: the row is absent. dir: `ReadDir` misses.
  http: the 404 from HEAD/GET. s3: `minio.ToErrorResponse(err).Code ==
  minio.NoSuchKey` (`isNoSuchKey` — a bare 404 with no S3 XML body already
  maps to `NoSuchKey`, which is why the test shim can return a plain 404).
- **`Close()`.** sql closes the pool (interrupting in-flight reads — the
  reader's `reader.Close()` runs first, per the defer order); http drops the
  client's idle keep-alive connections; dir and s3 are no-ops (minio-go owns
   its own client). `main` calls `defer backend.Close()` in `runAdd`,
   `runGet`, `runLookup`, `main_serve.go`'s `runServe` (once, after the
   server shut down), and `main_mount.go`'s `runMount` (once, after the
   FUSE connection is torn down).
- **Dir fanout.** The hex key bytes are carved into 1-byte dir levels —
  `fanoutDepth = 3` — the rest as filename (`<b0>/<b1>/<b2>/<rest>`):
  encoding-agnostic, ≤256 per level, no knowledge of the hash algorithm.
- **Platforms.** The dir backend's name index is a symlink — POSIX (any Unix);
  Windows users can keep going through WSL, or use the `sql` backend, whose
  index is a plain table. A non-symlink target fallback is the intended
  native-Windows migration.

## Development

The gates, cheapest first:

1. `gofmt -w` (or `goimports -w`) the edited `.go` files
2. `go build ./...`
3. `go test ./...` (add `-race` for the storage concurrency guard)
4. `golangci-lint run` (govet + gocyclo; gofmt/goimports formatters; the
   final gate)
5. `markdownlint-cli2 *.md` (doc changes)

## Testing

- `identity/`, `cid/`, `index/`, `oci/`, `daemon/`, `mount/` — table-driven
  tests against in-memory fakes (`identity_test.go`, `cid_test.go`,
  `index_test.go`, `oci_test.go`, `daemon_test.go`, `mount_test.go`), plain
  `go test`.
- `storage/` — round-trip tests against in-memory `httptest` servers plus a
  `-race` concurrent-read concurrency guard.
- **Smoke tests** (opt-in via env var, so `go test ./...` stays green in a
  sandbox): `TestDirSmoke` (`BLOB_DIR_PATH`), `TestHTTPSmoke`
  (`BLOB_HTTP_PATH`), `TestS3Smoke` (`BLOB_S3_ENDPOINT`/`BLOB_S3_BUCKET`),
  `TestServeSmoke` (`BLOB_SERVE_SMOKE=1`, needs `curl` on PATH) — the whole
  read surface over a real socket — and `TestMountSmoke`
  (`BLOB_MOUNT_SMOKE=1`, needs `/dev/fuse` and the `fusermount3` setuid
  helper) — the store over a real FUSE connection. Run one:
  `BLOB_DIR_PATH=/mnt/nfs go test -v -run TestDirSmoke ./storage`.
- `main` has no tests by convention (it is the CLI / adapters).
