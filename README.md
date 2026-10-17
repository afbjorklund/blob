# blob

A content-addressable blob store (CAS) using CID (Content ID) addressing, similar to `ipfs` but local-only, with pluggable storage backends — and OCI import/export (images and artifacts, from the layouts `docker save`/`podman save` produce).

See [DESIGN.md](DESIGN.md) for the rationale behind the design choices.

## Features

- **Content addressing with dedup** - files are stored by their content hash (CIDv1, base32hex encoding), so identical content is stored only once, no matter how many times it is added and under which names
- **Content-based codec** - the CID's codec byte is chosen from the content, not the extension: files starting with `{` or `[` are stored as the `Json` codec, everything else as `Raw` (see [DESIGN.md](DESIGN.md) for the rationale)
- **OCI import / export** - `oci import` reads a modern OCI layout (a directory, or a `docker save`/`podman save` tar — the v25+ "hybrid" is read by its OCI half) or wraps a bare file as a single-layer OCI **artifact**, storing every reachable blob; `oci export` writes the layout back out (dir or tar) byte-for-byte. Digests in OCI (`sha256:<hex>`) are the same bytes as the store's sha2-256 multihash, so an OCI import and a plain `add` of identical content deduplicate for free. `oci add`/`oci get` work as aliases. No registry I/O — local layouts only (see [DESIGN.md](DESIGN.md))
- **Pluggable storage backends**:
  - Directory (`--path` dir; files sharded across up to 3 one-byte fanout levels: `<byte0>/<byte1>/<byte2>/<rest of hex>`, e.g. `12/20/...`)
  - SQLite (single database file at `--path`)
  - HTTP (`--path` base URL; a thin GET/PUT proxy to a remote server, blobs at `<base>/<hex>`)
  - S3 (`--path` endpoint URL, `--bucket` name; primary target Ceph RADOS Gateway; a thin S3-object proxy, blobs at `<bucket>/<hex>`)
- **Streaming I/O** - Files are streamed to and from the backend without materializing the entire blob in memory. The `sqlite` backend reads via SQLite's blob API (`db.OpenBlob` in a read-only handle) instead of loading the whole row
- **Read-only HTTP gateway** - A built-in `blob serve` command (like the IPFS HTTP gateway, but for this local store) serves `GET /blob/<CID>`, so a stable `blob://<CID>` scheme can be mapped to `http://<host>/blob/<CID>` without the client knowing the backend or storage path. Response carries `Content-Type` (from the CID codec), `Content-Length`, and `Content-Digest: sha-256=…` (the stored multihash digest). `GET /name/<name>` resolves the name and `302`-redirects to the canonical `/blob/<CID>`
- **Name index** - A separate per-name index in each backend (symlink in the dir backend, foreign-keyed table in the sql backend, an `index/<name>` object holding the hash in the http backend) lets you resolve a name to the exact stored blob CID (e.g. `ubuntu_latest.sif` and `ubuntu_26.04.sif` both resolve to the same blob). Last add wins, so `latest`-style aliases update as new content is stored

## Usage

### Compute hash only

```bash
blob hash <file>
```

Prints the CID `sha256sum`-style (`<cid>  <file>`) without storing. A
convenience so you don't need a shell wrapper to turn a file into an address
(same family as `lookup` and `ipfs add --only-hash`).

### Add to blob store

```bash
blob add <file>
```

Options:
- `--storage dir|sql|http|s3` - Storage backend (default: `dir`)
- `--path <dir|dbfile|url>` - Storage location (a local dir/dbfile for `dir`/`sql`; a base URL for `http`; an endpoint URL for `s3`)
- `--bucket <name>` (or `BLOB_BUCKET`) - S3 bucket (only for `s3`)
- `--region <region>` (or `BLOB_S3_REGION`) - S3 SigV4 region (only for `s3`; blank defers to the server, set for AWS-like regions)
- `--user` / `--pass` (or `BLOB_USER`/`BLOB_PASS`) - remote-backend credentials (Basic auth user/password for `http`; access key / secret key for `s3`)

### Get from blob store

```bash
blob get <cid|name> -o <output-file>
```

Retrieves by CID, or falls back to the name index if the argument is not a valid CID:

```bash
blob get ubuntu_latest.sif -o output.sif
```

Options:
- `-o <output-file>` - Output file path (defaults to the argument as filename)

### Look up a name's CID (convenience)

```bash
blob lookup <name>
```

Convenience wrapper: resolves an index name to the stored blob's CID by
peeking the blob's first byte (the same content-based codec rule as
`add`/`hash`) — no re-hash, bounded by one byte regardless of blob size.
Not required (the CID is on disk / in the index), but handy:

```bash
blob lookup ubuntu_latest.sif
v05ah4859p5vrofmfpdv5i8om998c9k4gd0ge2shctr5jofc6ufj3ghgf8k
```

### OCI import / export

Import and export OCI layouts (images and artifacts). Input is the local OCI layout that `docker save`/`podman save` produce — a directory, or a tar (including the v25+ "hybrid" that is both a docker archive and an OCI archive; the OCI half is read, the docker half ignored). A bare file is wrapped as a single-layer OCI artifact.

```bash
# Import an OCI layout (dir or tar) — stores the reachable blobs, names it:
blob oci import oci-save/busybox       # a directory layout
blob oci import busybox.tar            # a docker/podman save tar (hybrid OK)

# Import a bare file as a single-layer OCI artifact (config = {}, one layer):
blob oci import myapp.sif --name myapp

# Export a stored layout back out to a directory or a tar:
blob oci export busybox -o oci-out     # directory (oci-layout/ + index.json/ + blobs/)
blob oci export busybox -o busybox.tar # tar archive

# List a stored layout's index, manifests, config and layers (read-only):
blob oci ls busybox
```

`oci import` and `oci export` are the primary verbs; `oci add`/`oci get` work as aliases to match the rest of the CLI. Digests on the two sides are the *same bytes* (`sha256:<hex>` ≡ the store's sha2-256 multihash), so content imported via OCI is the same CAS entry as the identical content added with `add` — it deduplicates for free. There is no registry I/O (no `docker://`/`oras://` pull, no tags): in and out are local layouts.

### HTTP gateway

```bash
blob serve [--listen/-l <host:port>]
```

Starts a read-only HTTP gateway (similar to the IPFS HTTP gateway, but for this
local store) on the given address, default `127.0.0.1:8080`. It exposes two read
paths:

```
GET /blob/<CID>     stream the blob
GET /name/<name>    resolve the name, then 302 → /blob/<CID>
```

`/blob/<CID>` lets a stable `blob://<CID>` scheme be mapped to
`http://<host:port>/blob/<CID>` without the client learning about backends,
storage paths, etc. The response is the blob bytes, streamed (never buffered
whole), with these headers:

- `Content-Type` — `application/json` if the CID's codec is `Json`, else `application/octet-stream`
- `Content-Length`
- `Content-Digest: sha-256=<base64(digest)>` — the stored multihash digest, for the client to verify against

`/name/<name>` does not serve the bytes itself: it resolves the name to the
stored blob, derives its CID (peeking the first byte for the codec), and
`302`-redirects to the canonical `/blob/<CID>` — the same name→blob pointer the
dir backend's symlink is on disk. A 302 (not 301) so the mutable name→CID
mapping (last add wins) is not cached. Clients that follow redirects
(`curl -L`, browsers) end up at the identical `/blob/<CID>` response; those that
don't still get the CID in the `Location` header.

The gateway is **read-only** (no upload); `add` covers writes.

```bash
# serve in one terminal (dir backend, default path)
blob serve

# fetch by CID in another
curl http://127.0.0.1:8080/blob/v060084h0p8uhcetb0l9o30ji4oa0aq7jnrrulb0ofjlteq3os2r3t7i44db0

# or by name — 302 → /blob/<CID>, follow with -L
curl -L http://127.0.0.1:8080/name/test.json
```

### Name index

Every non-`--only-hash` `add` records `basename(file) → blob digest` in the backend's own index, so a name later resolves to the exact stored blob (dir backend: a `index/<name>` symlink to the blob; sql backend: a `names` row keyed on the blob's hash).

- The index key is the file's **basename** (expected to be a readable name like `ubuntu.sif`, not a CID), so paths and aliases with the same name all map to the same blob
- **Last add wins**: re-adding a name points it at the newest blob, which is how `latest`-style aliases stay current
- Names are not unique per blob — two names can map to the same content (e.g. `ubuntu_26.04.sif` and `ubuntu_latest.sif`)
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

Files are stored in `.blob-store/<byte0>/<byte1>/<byte2>/<rest of the multihash, hex>` (fanned out by up to 3 key bytes) and indexed in `.blob-store/index/<basename>`.

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

Point `--path` at a remote server and the store (blobs and name index) lives there — `add` issues `PUT`, `get`/`lookup` issue `GET` (and `HEAD` for the existence check the CLI uses before storing). Blobs are addressed by the hex of the multihash, the name index lives at `index/<name>` (the body is the hex multihash — the server-side counterpart of the dir backend's symlink / sql foreign key):

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

This is a *storage* backend (the client talks hash-based over GET/PUT); it is distinct from `blob serve`, which is the read-only user-facing CID gateway (`/blob/<CID>`, `/name/<name>`).

### S3 backend (Ceph RGW)

Point `--path` at an S3-compatible endpoint (Ceph RADOS Gateway is the primary target) and `--bucket` at a named store within it. The client issues S3 `PUT`/`GET`/`HEAD` — blobs at `<bucket>/<hex>` and the name index at `<bucket>/index/<name>` (body the hex multihash, the same last-add-wins link as the other backends). `--user`/`--pass` (or `BLOB_USER`/`BLOB_PASS`) carry the access key / secret key:

```console
# Ceph RGW at 127.0.0.1:7480 (the default RGW S3 port), bucket `blobstore`
$ blob --storage s3 --path 127.0.0.1:7480 --bucket blobstore add test.txt
Added: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8

$ blob --storage s3 --path 127.0.0.1:7480 --bucket blobstore get test.txt -o output.txt
Retrieved: test.txt -> output.txt
```

This is a *storage* backend (the client talks hash-based over the S3 object API); it is distinct from `blob serve`, which is the read-only user-facing CID gateway.

For server-side setup (nginx for the `http` backend, Ceph RGW via cephadm for the `s3` backend) and the opt-in smoke tests, see [BACKEND.md](BACKEND.md).

## License

MIT
