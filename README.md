# blob

A content-addressable blob store (CAS) using CID (Content ID) addressing,
similar to `ipfs` but local-only, with pluggable storage backends — and OCI
import/export (images and artifacts, from the layouts `docker save`/`podman save`
produce).

![blob cyber structural logo](assets/blob.png)

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
- **Pluggable storage backends** - the same CAS, stored where you point
  `--path`: Directory, SQLite (a single file), HTTP (a remote `GET`/`PUT`
  server), and S3 (primary target Ceph RADOS Gateway). See
  [BACKEND.md](BACKEND.md) for how to point at each one
- **Streaming I/O** - Blobs are streamed to and from the backend without
  materializing the whole blob in memory
- **FUSE mount** - A built-in `blob mount` command (Linux; like `ipfs mount`)
  exposes the store read-only as a local filesystem: the recorded index names
   appear under `name/` as symlinks to their blob's CID, and the content is
   readable as regular files under `blob/<CID>` or `blob/sha256:<hex>` (the
   go-digest spelling — bare hex is not accepted there), streamed and never
   buffered whole. It uses `bazil.org/fuse` (pure Go — no CGO) and is read-only
- **Read-only HTTP gateway** - `blob serve` serves the blobs over plain HTTP —
  `GET /blob/<CID>` and `GET /name/<name>` (a `302`-redirect to the canonical
  `/blob/<CID>`) — so a stable `blob://<CID>` maps to `http://<host>/blob/<CID>`
  without the client knowing the backend. See Usage, HTTP gateway, for `curl`
- **OCI distribution registry (pull)** - `blob serve` also speaks the OCI
  distribution *read* API under `/v2/`, so podman / oras / skopeo can pull an
  imported layout with no `oci export` step; read-only, pull by CID or name,
  tag `latest`. See Usage, OCI distribution pull
- **Name index** - a separate per-name index, so a readable name resolves to
  the exact stored blob CID (`ubuntu_latest.sif` and `ubuntu_26.04.sif` can both
  resolve to the same blob); last add wins, so `latest`-style aliases update as
  new content is stored. Names may be registry-style
  (`docker.io/library/busybox`); the clean name is what every command
  takes and prints. `blob list` prints the recorded names (the index, not
  the CAS). See [CONCEPTS.md](CONCEPTS.md) "Name index"

## Usage

### Compute / verify hashes (like `sha256sum`, but CIDs)

```bash
blob sum <file>...             # print one line per file:  <cid>  <file>    (hash is an alias)
blob sum -                     # print the CID of stdin:   cat file | blob sum -
blob sum -c <list> [<list>...] # verify the lines of a checksum list against local files
blob sum -c -i <list>          # -i/--ignore-missing: skip files that are not present
```

Prints each file's CID in the `sha256sum` shape (`<cid>  <file>`), without
storing anything. Because each line records the **full CID** — the hash
algorithm *and* the codec — a check needs no digest-style parsing and has no
"improperly formatted" lines ([CONCEPTS.md](CONCEPTS.md) "Content
addressing (CID)").

`-c <list>` verifies one or more such lists against the local files they name,
reporting `<file>: OK`, `FAILED`, or `FAILED open or read` — it hashes the local
file in place and never touches the store. `-i` / `--ignore-missing` skips entries
whose file is not present (a mismatch still fails). `-` reads stdin in one streaming
pass, like `sha256sum -`.

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
- `--only-path` - Print only the blob's local filesystem path (do not retrieve
  the blob). **dir backend only** — on sql/s3/http the blob is a row, an
  object, or a remote body, not a local file, so the command errors out with a
  hint to use `get` to materialize a local file first. `apptainer run <path>`
  then runs straight from the store's dir.
- `--verify` - Re-hash the retrieved data against the blob's resolved multihash
  and fail if it does not match (a failed check leaves no partial file at the
  output path)

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
and `get --only-size` report, for every name in one place.

```console
$ blob list -l
ubuntu_26.04.sif    v05ah...j8dg    52428800
ubuntu_latest.sif   v05ah...j8dg    52428800
```

OCI origin references (`oci/<variant>/<os>/<arch>/<algo>/<hex>` provenance
names, recorded by `oci import --digest`/`--arch`) are **not** listed by
default — they are slash-bearing machine-shaped identifiers and not very
readable next to the ordinary free names. `--oci` includes them in the listing
(under either or both of the above forms):

```console
$ blob list
ubuntu_26.04.sif
ubuntu_latest.sif

$ blob list --oci
ubuntu_26.04.sif
ubuntu_latest.sif
oci/blob/linux/amd64/sha256/a46257e06c82ad...
oci/blob/linux/arm64/sha256/a46257e06c82ad...
```

### Look up a name's CID (convenience)

```bash
blob lookup <name>
```

Convenience wrapper: resolves an index name to the stored blob's CID. Not required
(the CID is on disk / in the index), but handy:

```bash
blob lookup ubuntu_latest.sif
v05ah4859p5vrofmfpdv5i8om998c9k4gd0ge2shctr5jofc6ufj3ghgf8k
```

`--oci` prints the blob's OCI digest (`sha256:<hex>`) instead of its CID — the
same form the layer digests in `oci ls` output use, so the two can be compared
directly:

```bash
blob lookup --oci ubuntu_latest.sif
sha256:d79f2e37784e5cd8631963896ebc6c9c66934af94a1854504717eaec04bc3d09
```

### Inspect a CID

```bash
blob cid <cid|name>
```

Decodes a CID (any multibase) and prints its parts — multibase, version, codec,
multihash, and the raw digest; a name argument is resolved to its CID first, then
printed the same way.

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
# `blob add`), and the printed CID is the OCI scaffold's address (its
# index.json), which is what `oci ls` / `oci export` use:
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
blob oci export myapp.sif -o out       # a bare file: wrapped as its artifact

# List a stored layout's index, manifests, config and layers (read-only):
blob oci ls busybox
blob oci ls myapp.sif                  # a bare file's artifact (if `oci import`-ed)
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

For a bare file, `oci ls` **reports what the store holds** (it works only when
that file was `oci import`-ed, which persisted its scaffold), while
`oci export` **writes the layout out** for any stored blob — so a `blob add`-ed
file still exports to a valid artifact ([DESIGN.md](DESIGN.md) "OCI import /
export").

Two behaviors follow. A *name* is any string the user chose — `add` and
`oci import` also accept a foreign digest or reference (an `ipfs add` CID,
an `registry@digest`, even a Git OID) and file it as a plain name the store
never resolves or pulls; the CID stays the single stable address for the
content, and a name may drift or be remapped while it holds. See
[CONCEPTS.md](CONCEPTS.md) "What names what" and
[DESIGN.md](DESIGN.md) "Foreign digests as names".

### Record an origin OCI reference

For an image or a SIF built from one, record *where it came from* (the
multi-arch origin digest plus the platform it was built for) as a
structured name a human or a tool can rely on:

```console
oci/<variant>/<os>/<arch>/<algo>/<hex>
```

- `<hex>` is the multi-arch origin digest `docker pull` prints (the
  `Digest:` line, the **same for every arch** of the image). It is a
  *foreign identifier* — it identifies the upstream image, not a content
  address the store holds (the store holds the SIF or the OCI index, whose
  digests differ). This is what `apptainer push` / ORAS also record as the
  `org.opencontainers.image.base.digest` of an overlay base.
- `<os>/<arch>` is the platform the local artifact was built for — the
  `--platform` / `--arch` flag (defaults to the local arch under linux).
- `<algo>` is `sha256` (the only digest the store bridges); it is a path
  component, not a `sha256:`-prefixed filename, so the shape can grow
  (sha512, ...) without a rename.
- `<variant>` is what the leaf names: `blob` (a blob — the raw file bytes,
  a SIF, a tar, a layer, or a config — the client's `--basepath` target) or
  `json` (the OCI index / manifest — the entrypoint `oci ls` and a client
  peek at to discover what else the layout references). It is not a flag: it
  follows the input shape, and is set automatically — a bare file is always
  `blob`, a layout always `json`. An OCI "image" and an OCI "artifact" are
  the same JSON structure (both an image.index / image.manifest), so they
  share the one `json` value; the distinction is what the producer meant,
  not something the store can or should assert.

The record is a *name* — a flat index entry that resolves to a content
CID — and it can be looked up like any other (`get`, `cat`, `lookup`, or
the `oci/` directory in the FUSE view). Different `(variant, os, arch,
hex)` under the same origin digest are distinct names (a SIF under
`linux/amd64` and another under `linux/arm64` share the upstream digest
but are different stored blobs).

```bash
B=sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4
# a SIF built for amd64 (bare file => blob)
blob oci import busybox.sif      --digest "$B" --arch amd64
blob oci import busybox-arm.sif  --digest "$B" --arch arm64
# the OCI layout for amd64 (layout => json)
blob oci import busybox.tgz --digest "$B" --arch amd64
# resolve any of them back to a CID
blob lookup oci/blob/linux/amd64/sha256/a46257e0...
blob lookup oci/json/linux/amd64/sha256/a46257e0...
cat mount/oci/blob/linux/amd64/sha256/a46257e0...
```

The flags live only on `oci import` — a plain `blob add` records only the
basename as a name and never touches the `oci/` namespace (origin
references are an OCI concern; record them with `oci import <file>`,
which imports the same key into the CAS).

See [DESIGN.md](DESIGN.md) "Origin references as names".

### HTTP gateway

```bash
blob serve [--listen/-l <host:port>]
```

Starts a read-only HTTP gateway (like the IPFS HTTP gateway, but for this local
store) on the given address, default `127.0.0.1:8080`. Two read paths:

```text
GET /blob/<CID>     stream the blob
GET /name/<name>    resolve the name, then 302 → /blob/<CID>
```

`/blob/<CID>` streams the bytes (never buffered whole) so a stable
`blob://<CID>` maps to `http://<host:port>/blob/<CID>` without the client
knowing the backend; the response carries `Content-Length`, `Content-Type`,
`Content-Digest: sha-256=…` (the stored digest, for the client to verify
against), and — where the backend tracks it — `Last-Modified`
([CONCEPTS.md](CONCEPTS.md) "HTTP gateway").
`/name/<name>` resolves the name and `302`-redirects to the canonical
`/blob/<CID>` (a 302, not 301, because the name→CID mapping is mutable — last add
wins; `curl -L` follows it, a redirect-averse client gets the CID in
`Location`). The gateway is **read-only** (no upload); `add` covers writes.

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
is pullable by stock OCI clients (`podman`, `oras`, `skopeo`) without an
`oci export` step. The repository name is the CID `oci import` printed or a
stored name, the only tag is `latest`, and a stored **bare file is pullable by
name too** (its OCI artifact is reconstructed on demand, never added to the
store). Read-only (no push), no auth, no HTTPS — treat the base as a trusted
plain-HTTP registry (localhost by default).

The `/v2/…` paths and the `X-Blob-Cid` header are in
[IMPLEMENTATION.md](IMPLEMENTATION.md) "daemon"; the tag policy, in
[CONCEPTS.md](CONCEPTS.md) "OCI, the closest cousin".

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

### FUSE mount (read-only filesystem view)

```bash
blob mount [--mount/-m <dir>]
```

Mounts the store read-only as a local FUSE filesystem (Linux only; pure Go via
`bazil.org/fuse`, so no CGO or libfuse install), the filesystem analogue of
`serve` — like `ipfs mount`. It runs in the foreground for the life of the
process and unmounts on `SIGINT`/`SIGTERM`. The mount point defaults to
`$HOME/.blob-mount` (or `BLOB_MOUNT_PATH`); pass `--mount`/`-m` for another —
a `~` or `~/...` is expanded to the real home path.

The backend is chosen by the usual `--storage`/`--path` flags (a *read-only*
open, the same as `serve`), so `name/` and `blob/` present whichever backends
you point at:

```text
name/<name>          one symlink per recorded index name, linking to the blob's CID
blob/<CID>           the content as a regular file, streamed on read
blob/sha256:<hex>    the same content, addressed by its OCI (SHA-2 256) digest
```

- `name/` **is listable** — it is the store's index (the `blob list` set).
  `ls name/` shows the names; `name/<n>` is a POSIX symlink to `../blob/<CID>`,
  so `name/<n>` and `blob/<CID>` are the same "name → canonical blob" pointer,
  rendered as a link instead of the gateway's `302`. The target is a relative
  `../blob/<CID>`, so it resolves correctly no matter where the mount lives.
  OCI origin references (`oci/<variant>/<os>/<arch>/<algo>/<hex>`) are **not
  listed under `name/`** (they live under the dedicated `oci/` tree, which is
  their structured, walkable form in the mount).
- `blob/` is **addressable by CID or OCI digest, not enumerable**: `blob/<CID>`
  and `blob/sha256:<hex>` (the go-digest spelling; bare hex is *not* accepted
  there) open and `cat`/`stat` normally, but `ls blob/` is empty — the store
  exposes no "list all blobs" (the index, not the CAS, is what `Names()`
  covers), so the directory is read by path only. This is the same property as
  the OCI surface, which addresses inner blobs by digest and never lists them.
- Reads are **streaming and bounded** (the kernel page cache serves re-reads);
  a multi-GB blob is served page-by-page, never buffered whole in the mount.
- It is **read-only** end to end: the mount is opened `ro` and the backend is
  opened read-only, so a write from the shell (`touch`, `rm`, `cp` into the
  mount) is refused.

```console
$ blob mount                        # mounts at $HOME/.blob-mount
$ ls $HOME/.blob-mount/name/
busybox  busybox.sif  ubuntu_latest.sif  …
$ cat $HOME/.blob-mount/name/busybox    # follows the link: same bytes as blob get
$ stat -c '%s' $HOME/.blob-mount/blob/v060084h…   # size, no download
$ cat $HOME/.blob-mount/blob/$(sha256sum base.sif | cut -d' ' -f1 | sed 's/^/sha256:/') \
        # the same blob, addressed by its sha256 — e.g. a local-file --base hash
$ ls $HOME/.blob-mount/blob/              # empty — the CAS is not listable
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

Point `--path` at a remote server that stores the blobs — typically one an
operator has already provisioned. This client does nothing but send
`PUT`/`GET`/`HEAD`; nothing is created on the server:

```console
$ blob --storage http --path http://localhost:8080 add test.txt
Added: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8

$ blob --storage http --path http://localhost:8080 get test.txt -o output.txt
Retrieved: test.txt -> output.txt
```

A *storage* backend (the client talks content-addressed over `GET`/`PUT`),
distinct from `blob serve` — the read-only user-facing CID gateway. Where the
server must serve (blob + `index/` paths, and the JSON autoindex `list` needs)
and a complete nginx setup: [BACKEND.md](BACKEND.md).

### S3 backend (Ceph RGW)

Point `--path` at an S3-compatible endpoint (Ceph RADOS Gateway is the primary
target; MinIO and AWS S3 work too) and `--bucket` at a store within it. As
with the other backends this client provisions nothing — point it at a bucket
an operator has already created:

```console
# Ceph RGW at 127.0.0.1:7480 (the default RGW S3 port), bucket `blobstore`
$ blob --storage s3 --path 127.0.0.1:7480 --bucket blobstore add test.txt
Added: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8

$ blob --storage s3 --path 127.0.0.1:7480 --bucket blobstore get test.txt -o output.txt
Retrieved: test.txt -> output.txt
```

A *storage* backend (the client talks content-addressed over the S3 object
API), distinct from `blob serve` — the read-only user-facing CID gateway.

For server-side setup (nginx for the `http` backend, Ceph RGW via cephadm for
the `s3` backend) and the opt-in smoke tests, see [BACKEND.md](BACKEND.md).

## License

[MIT](LICENSE)
