# blob

A content-addressable blob store (CAS) using CID (Content ID) addressing,
similar to `ipfs` but local-only, with pluggable storage backends — and OCI
import/export (images and artifacts, from the layouts `docker save`/`podman save`
produce).

See [DESIGN.md](DESIGN.md) for the rationale behind the design choices.

## Build

`blob` is a local Go module (no import path to install from a registry), so
build it from the checkout, in the module directory:

```bash
go build      # builds the `blob` binary into the current directory
go install    # installs it into GOBIN / ~/go/bin (on PATH, so just `blob`)
blob --help
```

## Features

- **Content addressing with dedup** - files are stored by their content hash
  (CIDv1, base32hex encoding), so identical content is stored only once, no
  matter how many times it is added and under which names
- **Content-based codec** - the CID's codec byte is chosen from the content, not
  the extension: files starting with `{` or `[` are stored as the `Json` codec,
  everything else as `Raw` (see [DESIGN.md](DESIGN.md) for the rationale)
- **OCI import / export** - `oci import` reads a modern OCI layout (a directory,
  or a `docker save`/`podman save` tar — the v25+ "hybrid" is read by its OCI
  half — or its gzip-compressed form, a `.tgz`/`.tar.gz`) or wraps a bare file
  as a single-layer OCI **artifact**, storing every reachable blob; `oci export`
  writes the layout back out (dir or tar)
  byte-for-byte. Digests in OCI (`sha256:<hex>`) are the same bytes as the
  store's sha2-256 multihash, so an OCI import and a plain `add` of identical
  content deduplicate for free. `oci add`/`oci get` work as aliases. No registry
  I/O — local layouts only (see [DESIGN.md](DESIGN.md))
- **Pluggable storage backends**:
  - Directory (`--path` dir; files sharded across up to 3 one-byte fanout
    levels: `<byte0>/<byte1>/<byte2>/<rest of hex>`, e.g. `12/20/...`)
  - SQLite (single database file at `--path`)
  - HTTP (`--path` base URL; a thin GET/PUT proxy to a remote server, blobs at
    `<base>/<hex>`)
  - S3 (`--path` endpoint URL, `--bucket` name; primary target Ceph RADOS
    Gateway; a thin S3-object proxy, blobs at `<bucket>/<hex>`)
- **Streaming I/O** - Files are streamed to and from the backend without
  materializing the entire blob in memory. The `sqlite` backend reads via
  SQLite's blob API (`db.OpenBlob` in a read-only handle) instead of loading the
  whole row
- **Read-only HTTP gateway** - A built-in `blob serve` command (like the IPFS
  HTTP gateway, but for this local store) serves `GET /blob/<CID>`, so a stable
  `blob://<CID>` scheme can be mapped to `http://<host>/blob/<CID>` without the
  client knowing the backend or storage path. Response carries `Content-Type`
  (from the CID codec), `Content-Length`, `Content-Digest: sha-256=…` (the
  stored multihash digest), and `Last-Modified` (RFC 7231, the store's
  last-modified time) so HTTP clients can use conditional requests and
  maintenance tooling can see when a blob was added. `GET /name/<name>` resolves
  the name and `302`-redirects to the canonical `/blob/<CID>`
- **OCI distribution registry (pull)** - `blob serve` also speaks the OCI
  distribution *read* API under `/v2/`, so an already-imported layout can be
  pulled by stock clients (`podman pull docker://…`, `oras pull oras://…`,
  `skopeo`, buildkit) with no `oci export` step. The repository name is the CID
  the store already printed: `docker://host/blob/<CID>` (primary, the exact CID
  from `oci import`) or `docker://host/name/<name>` (secondary, the name index).
  The only tag is `latest` (→ the layout index); inner components are addressed
  by their `sha256:<hex>` digest, exactly as the OCI spec expects. Read-only (no
  push), no auth/HTTPS — treat the base as a trusted plain-HTTP registry
  (localhost by default)
- **Name index** - A separate per-name index in each backend (symlink in the dir
  backend, foreign-keyed table in the sql backend, an `index/<name>` object
  holding the hash in the http/s3 backends) lets you resolve a name to the exact
  stored blob CID (e.g. `ubuntu_latest.sif` and `ubuntu_26.04.sif` both resolve
  to the same blob). Last add wins, so `latest`-style aliases update as new
  content is stored. `blob list` prints the recorded names (the index, not the
  CAS)
  - **Registry-style names** - a name may contain `/`, `@`, `:`, `.` etc. (`docker.io/library/busybox`,
    `docker.io/library/busybox@sha256:…`). All backends keep the index one flat
    level for it: the name is stored as an *encoded segment* (`/` → `%2F`, `%` →
    `%%`), and the clean name is what every command takes and prints.
    `oci import` of a layout records the basename plus any `--name`
    (repeatable), all resolving to the layout's index CID; of a bare file the
    same names resolve to the *file's own bytes* (the artifact's single layer,
    the same key `blob add` uses) — the file is named consistently regardless of
    which command stores it

## Usage

### Compute / verify hashes (like `sha256sum`, but CIDs)

```bash
blob sum <file>...             # print one line per file:  <cid>  <file>    (hash is an alias)
blob sum -c <list> [<list>...] # verify the lines of a checksum list against local files
blob sum -c -i <list>          # -i/--ignore-missing: skip files that are not present
```

**Compute.** `blob sum <file>...` (alias `hash`) prints each file's CID `sha256sum`-style —
`<cid>  <file>` (two spaces), one line per file — without storing anything. A
convenience so you don't need a shell wrapper to turn files into addresses
(same family as `lookup` and `ipfs add --only-hash`). Files are hashed
independently: an unreadable file reports its own `Error:` line and a
non-zero exit, but the other files still produce their lines.

**Verify.** `blob sum -c <list>` reads one or more checksum lists whose lines
are `<cid>  <file>` (exactly the shape of `blob sum` output) and, for each
line, recomputes the *local* `file`'s CID and reports

- `<file>: OK` — the local file hashes to the CID on the line;
- `<file>: FAILED` — the file is there but its content differs;
- `<file>: FAILED open or read` — the file could not be read (missing, etc.)

and exits non-zero if any entry fails or is unreadable. Blank and `#`-comment
lines are skipped. It never touches the store: verification hashes the named
local file in place (we do not yet verify against the blob store).

Unlike `sha256sum`, each line records the **full CID** — it carries the hash
algorithm *and* the codec — so `-c` needs no digest-style parsing and has no
"improperly formatted" lines; `sha256sum` only says "64 hex chars" and must
assume they are SHA-256.

`-i` / `--ignore-missing` skips entries whose file cannot be read instead of
failing with a `FAILED open or read` line — the common case of a checksums file
covering every arch while only one arch's file is locally present. A genuine
content mismatch is still reported and still fails.

### Add to blob store

```bash
blob add <file>
```

Options:

- `--storage dir|sql|http|s3` - Storage backend (default: `dir`)
- `--only-hash` / `-n` - print `added <cid> <file>` like `blob sum`, but do
  **not** store (and record no index name)
- `--path <dir|dbfile|url>` - Storage location (a local dir/dbfile for `dir`/`sql`;
  a base URL for `http`; an endpoint URL for `s3`)
- `--bucket <name>` (or `BLOB_BUCKET`) - S3 bucket (only for `s3`)
- `--region <region>` (or `BLOB_S3_REGION`) - S3 SigV4 region (only for `s3`;
  blank defers to the server, set for AWS-like regions)
- `--user` / `--pass` (or `BLOB_USER`/`BLOB_PASS`) - remote-backend credentials
  (Basic auth user/password for `http`; access key / secret key for `s3`)

All of these are global flags (any subcommand accepts them), and each can be
pre-set via an environment variable the flag falls back to: `BLOB_STORAGE_TYPE`
(`--storage`), `BLOB_STORAGE_PATH` (`--path`), `BLOB_BUCKET` (`--bucket`),
`BLOB_S3_REGION` (`--region`), and `BLOB_USER`/`BLOB_PASS`. An explicit flag
always wins over the environment.

### Get from blob store

```bash
blob get <cid|name> -o <output-file>
```

Retrieves by CID, or falls back to the name index if the argument is not a valid
CID:

```bash
blob get ubuntu_latest.sif -o output.sif
```

Options:

- `-o <output-file>` - Output file path (defaults to the argument as filename)
- `--force` / `-f` - Overwrite the output file if it already exists. Without
  it, a `get` whose target path exists fails before downloading anything
- `--only-size` - Print only the blob size in bytes (do not retrieve the blob)
- `--only-mtime` - Print only the blob's last-modified time in Unix seconds (do
  not retrieve the blob)
- `--verify` - Re-hash the retrieved data against the blob's resolved multihash
  and fail if the content does not match. The data is downloaded to a temporary
  file and renamed into place only after the check passes, so a failed
  verification leaves no partial or mismatched file at the output path

### Print a blob to stdout

```bash
blob cat <cid|name>
```

Resolves by CID (or the name index, exactly like `get`) and writes the blob's
bytes to stdout — nothing else. A `get` to stdout, for piping:

```bash
blob cat ubuntu_latest.sif | tar -x
```

### List names

```bash
blob list
```

Prints the names currently recorded in the index, one per line. It lists the **index**,
not the CAS: blobs with no recorded name are invisible, and a blob under several
names lists once per name. An empty store prints nothing.

```console
$ blob list
ubuntu_26.04.sif
ubuntu_latest.sif
```

`--long` / `-l` prints one tab-separated line per name with the name's CID and
the blob's size in bytes (`name<TAB>cid<TAB>size`) — the same values `lookup`
and `get --only-size` report, for every name in one place. A blob shared by
several names is resolved once, not per name.

```console
$ blob list -l
ubuntu_26.04.sif    v05ah...j8dg    52428800
ubuntu_latest.sif   v05ah...j8dg    52428800
```

### Look up a name's CID (convenience)

```bash
blob lookup <name>
```

Convenience wrapper: resolves an index name to the stored blob's CID by peeking
the blob's first byte (the same content-based codec rule as `add`/`sum`) — no
re-hash, bounded by one byte regardless of blob size. Not required (the CID is
on disk / in the index), but handy:

```bash
blob lookup ubuntu_latest.sif
v05ah4859p5vrofmfpdv5i8om998c9k4gd0ge2shctr5jofc6ufj3ghgf8k
```

### Inspect a CID

```bash
blob cid <cid|name>
```

Decodes a CID (any multibase) or resolves an index name to that CID — and prints
a breakdown of its CIDv1 parts: multibase, version, codec (name + hex code),
multihash (name + hex code, size in bits/bytes), and the raw digest in lower-case
base16. A pure decode: a CID argument never opens a backend, a name argument
resolves it read-only and derives the codec the same way `lookup` does.

```console
$ blob cid v060084h005dbqvqsolt2rmabem8f0imo1117742uscvcbjlulph2eqknv1h0
v060084h005dbqvqsolt2rmabem8f0imo1117742uscvcbjlulph2eqknv1h0

multibase:  base32hex (v)
version  :  cidv1 (0x01)
codec    :  json (0x200)
multihash:  sha2-256 (0x12), 256 bits (32 bytes = 0x20)
digest   :  015abd7f5cc57a2dd94b7590f04ad8084273905ee33ec5cebeae62276a97f862

store:      yes — json, usable with add / get / cat
```

A CID of a codec this store does not hold (say a `dag-pb` CID from `ipfs add`)
still decodes fine — the last line notes `store: no …`, but it is the same
multihash the store would address. `--base <name>` re-renders the first line in
another multibase (`base16`, `base64url`, `base58btc`, …) as a learning aid.

### OCI import / export

Import and export OCI layouts (images and artifacts). Input is the local OCI
layout that `docker save`/`podman save` produce — a directory, or a tar
(including the v25+ "hybrid" that is both a docker archive and an OCI archive;
the OCI half is read, the docker half ignored), or its gzip-compressed form
(a `.tgz`/`.tar.gz`), which is decompressed transparently. A bare file is
wrapped as a single-layer OCI artifact.

```bash
# Import an OCI layout (dir or tar) — stores the reachable blobs, names it:
blob oci import oci-save/busybox       # a directory layout
blob oci import busybox.tar            # a docker/podman save tar (hybrid OK)
blob oci import busybox.tgz            # gzip-compressed layout (decompressed)

# Import a bare file as a single-layer OCI artifact (config = {}, one layer):
# the name (basename + any --name) points at the file's bytes (same key as
# `blob add`), and the printed CID is the OCI scaffold's address:
blob oci import myapp.sif

# Give the imported layout a registry-style name (or several). The basename
# (busybox) is always recorded too, and each --name is recorded alongside it;
# all of them resolve to the same index CID:
blob oci import docker.io/library/busybox.tar \
  --name 'docker.io/library/busybox' \
  --name 'docker.io/library/busybox@sha256:365a…'

# Export a stored layout back out to a directory or a tar:
blob oci export busybox -o oci-out     # directory (oci-layout/ + index.json/ + blobs/)
blob oci export busybox -o busybox.tar # tar archive

# List a stored layout's index, manifests, config and layers (read-only):
blob oci ls busybox
```

`oci import` and `oci export` are the primary verbs; `oci add`/`oci get` work as
aliases to match the rest of the CLI. Digests on the two sides are the *same
bytes* (`sha256:<hex>` ≡ the store's sha2-256 multihash), so content imported
via OCI is the same CAS entry as the identical content added with `add` — it
deduplicates for free. There is no registry I/O **in these commands** (input and
output are local layouts); serving an already-imported layout as a registry is
`blob serve`, which — like the plain-HTTP `curl`-able gateway — also speaks the
OCI distribution *read* API under `/v2/` (see the "OCI distribution pull"
section below).

### HTTP gateway

```bash
blob serve [--listen/-l <host:port>]
```

Starts a read-only HTTP gateway (similar to the IPFS HTTP gateway, but for this
local store) on the given address, default `127.0.0.1:8080`. It exposes two read
paths:

```text
GET /blob/<CID>     stream the blob
GET /name/<name>    resolve the name, then 302 → /blob/<CID>
```

`/blob/<CID>` lets a stable `blob://<CID>` scheme be mapped to
`http://<host:port>/blob/<CID>` without the client learning about backends,
storage paths, etc. The response is the blob bytes, streamed (never buffered
whole), with these headers:

- `Content-Type` — `application/json` if the CID's codec is `Json`, else
  `application/octet-stream`
- `Content-Length`
- `Content-Digest: sha-256=<base64(digest)>` — the stored multihash digest, for
  the client to verify against
- `Last-Modified` — the store's last-modified time for the blob (RFC 7231), i.e.
  when it was added to the store; useful for conditional requests and
  maintenance (e.g. finding and pruning old blobs). Omitted by a backend that
  does not track one

`/name/<name>` does not serve the bytes itself: it resolves the name to the
stored blob, derives its CID (peeking the first byte for the codec), and `302`-redirects
to the canonical `/blob/<CID>` — the same name→blob pointer the dir backend's
symlink is on disk. A 302 (not 301) so the mutable name→CID mapping (last add
wins) is not cached. Clients that follow redirects (`curl -L`, browsers) end up
at the identical `/blob/<CID>` response; those that don't still get the CID in
the `Location` header.

The gateway is **read-only** (no upload); `add` covers writes.

```bash
# serve in one terminal (dir backend, default path)
blob serve

# fetch by CID in another
curl http://127.0.0.1:8080/blob/v060084h0p8uhcetb0l9o30ji4oa0aq7jnrrulb0ofjlteq3os2r3t7i44db0

# or by name — 302 → /blob/<CID>, follow with -L
curl -L http://127.0.0.1:8080/name/test.json
```

### OCI distribution pull (`/v2/`)

The same server speaks the OCI distribution *read* API, so a layout you import
is pullable by stock OCI clients without exporting it. The repository name is
the CID the store already prints (`blob oci import` → `index <CID>`) or a stored
name:

```text
GET /v2/                                            # ping
GET /v2/blob/<CID>/manifests/latest                 # primary: the layout index
GET /v2/blob/<CID>/blobs/sha256:...                 # primary: an inner component
GET /v2/name/<name>/manifests/latest                # secondary: same, by name
GET /v2/name/<name>/blobs/sha256:...                # secondary: an inner component
```

- `<CID>` is the outer (index-root) CID from `oci import`; its multihash *is*
  the store key of the layout's `index.json`, so the OCI pull and the `blob://`
  gateway (`/blob/<CID>`) hit the same index blob. Inner components stay
  `sha256:<hex>` (no CID needed) — just find them with `oci ls <name>`.
- The only tag is `latest` (→ the layout index); there is no tag map because the
  store has no name:tag split.
- Read-only (no push), no auth, no HTTPS — the server assumes a **plain-HTTP**
  base on localhost, the way a trusted local registry is configured.
- The manifest response carries the store's CID in a non-standard
  `X-Blob-Cid` header (base32hex) — the OCI spec's `Docker-Content-Digest` is
  `sha256:<hex>` and carries no codec, so it cannot pin the CID. Clients that
  don't know about it ignore it.

```bash
# terminal 1: import + serve
blob oci import busybox.tar --name busybox     # prints the index CID
blob serve                                      # http://127.0.0.1:8080

# terminal 2: pull the same content two ways over the same /v2/ surface
CID=<the CID oci import printed>
podman pull docker://127.0.0.1:8080/blob/$CID:latest        # primary (CID)
podman pull docker://127.0.0.1:8080/name/busybox:latest     # secondary (name)

# or, without an OCI client:
curl http://127.0.0.1:8080/v2/blob/$CID/manifests/latest
```

### Name index

Every non-`--only-hash` `add` records `basename(file) → blob digest` in the
backend's own index, so a name later resolves to the exact stored blob (dir
backend: a `index/<name>` symlink to the blob; sql backend: a `names` row keyed
on the blob's hash).

- The index key is the file's **basename** (expected to be a readable name like
  `ubuntu.sif`, not a CID), so paths and aliases with the same name all map to
  the same blob
- **Last add wins**: re-adding a name points it at the newest blob, which is how
  `latest`-style aliases stay current
- Names are not unique per blob — two names can map to the same content (e.g.
  `ubuntu_26.04.sif` and `ubuntu_latest.sif`)
- `--only-hash` skips the index entirely

## Examples

### Directory backend (default)

```console
$ blob add test.txt
Added: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8

$ blob get test.txt -o output.txt
Retrieved: test.txt -> output.txt

$ blob get v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8 -o output.txt
Retrieved: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8 -> output.txt

$ cat output.txt
test
```

Files are stored in
`.blob-store/<byte0>/<byte1>/<byte2>/<rest of the multihash, hex>` (fanned out
by up to 3 key bytes) and indexed in `.blob-store/index/<basename>`.

### Aliasing names to the same blob

```console
$ blob add ubuntu_26.04.sif
Added: v05ah48...j8dg

$ cp ubuntu_26.04.sif ubuntu_latest.sif
$ blob add ubuntu_latest.sif
File already exists in store: v05ah48...j8dg

$ blob get ubuntu_latest.sif -o latest.sif
Retrieved: ubuntu_latest.sif -> latest.sif
```

### SQLite backend

```console
$ blob --storage sql --path blobs.db add test.txt
Added: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8

$ blob --storage sql --path blobs.db get v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8 -o output.txt
Retrieved: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8 -> output.txt

$ cat output.txt
test
```

### HTTP backend

Point `--path` at a remote server and the store (blobs and name index) lives
there — `add` issues `PUT`, `get`/`lookup` issue `GET` (and `HEAD` for the
existence check the CLI uses before storing). Blobs are addressed by the hex of
the multihash, the name index lives at `index/<name>` (the body is the hex
multihash — the server-side counterpart of the dir backend's symlink / sql
foreign key). `blob list` needs the server to serve `GET <base>/index/` as a
JSON autoindex (see [BACKEND.md](BACKEND.md) for the nginx config):

```console
# server on http://localhost:8080 storing blobs at /<hex> and names at /index/<name>
# (on port 80 you would drop the :8080, e.g. http://localhost/blob)
$ blob --storage http --path http://localhost:8080 add test.txt
Added: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8

$ blob --storage http --path http://localhost:8080 get v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8 -o output.txt
Retrieved: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8 -> output.txt

$ blob --storage http --path http://localhost:8080 get test.txt -o output.txt
Retrieved: test.txt -> output.txt
```

This is a *storage* backend (the client talks hash-based over GET/PUT); it is
distinct from `blob serve`, which is the read-only user-facing CID gateway (`/blob/<CID>`,
`/name/<name>`).

### S3 backend (Ceph RGW)

Point `--path` at an S3-compatible endpoint (Ceph RADOS Gateway is the primary
target) and `--bucket` at a named store within it. The client issues S3 `PUT`/`GET`/`HEAD`
— blobs at `<bucket>/<hex>` and the name index at `<bucket>/index/<name>` (body
the hex multihash, the same last-add-wins link as the other backends). `--user`/`--pass`
(or `BLOB_USER`/`BLOB_PASS`) carry the access key / secret key:

```console
# Ceph RGW at 127.0.0.1:7480 (the default RGW S3 port), bucket `blobstore`
$ blob --storage s3 --path 127.0.0.1:7480 --bucket blobstore add test.txt
Added: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8

$ blob --storage s3 --path 127.0.0.1:7480 --bucket blobstore get test.txt -o output.txt
Retrieved: test.txt -> output.txt
```

This is a *storage* backend (the client talks hash-based over the S3 object
API); it is distinct from `blob serve`, which is the read-only user-facing CID
gateway.

For server-side setup (nginx for the `http` backend, Ceph RGW via cephadm for
the `s3` backend) and the opt-in smoke tests, see [BACKEND.md](BACKEND.md).

## License

[MIT](LICENSE)
