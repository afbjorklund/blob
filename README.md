# blob

A content-addressable blob store (CAS) using CID (Content ID) addressing, similar to `ipfs` but local-only, with pluggable storage backends.

See [DESIGN.md](DESIGN.md) for the rationale behind the design choices.

## Features

- **Content addressing with dedup** - files are stored by their content hash (CIDv1, base32hex encoding), so identical content is stored only once, no matter how many times it is added and under which names
- **Content-based codec** - the CID's codec byte is chosen from the content, not the extension: files starting with `{` or `[` are stored as the `Json` codec, everything else as `Raw` (see [DESIGN.md](DESIGN.md) for the rationale)
- **Pluggable storage backends**:
  - Directory (`--path` dir; files sharded across up to 3 one-byte fanout levels: `<byte0>/<byte1>/<byte2>/<rest of hex>`, e.g. `12/20/...`)
  - SQLite (single database file at `--path`)
- **Streaming I/O** - Files are streamed to and from the backend without materializing the entire blob in memory. The `sqlite` backend reads via SQLite's blob API (`db.OpenBlob` in a read-only handle) instead of loading the whole row
- **Read-only HTTP gateway** - A built-in `blob serve` command (like the IPFS HTTP gateway, but for this local store) serves `GET /blob/<CID>`, so a stable `blob://<CID>` scheme can be mapped to `http://<host>/blob/<CID>` without the client knowing the backend or storage path. Response carries `Content-Type` (from the CID codec), `Content-Length`, and `Content-Digest: sha-256=…` (the stored multihash digest)
- **Name index** - A separate per-name index in each backend (symlink in the dir backend, foreign-keyed table in the sql backend) lets you retrieve a blob by its original filename (e.g. `ubuntu_latest.sif` and `ubuntu_26.04.sif` both resolve to the same blob). Last add wins, so `latest`-style aliases update as new content is stored

## Usage

### Compute hash only

```bash
blob hash <file>
```

### Add to blob store

```bash
blob add <file>
```

Options:
- `--storage dir|sql` - Storage backend (default: `dir`)
- `--path <dir|dbfile>` - Storage location

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

### HTTP gateway

```bash
blob serve [--listen/-l <host:port>]
```

Starts a read-only HTTP gateway (similar to the IPFS HTTP gateway, but for this
local store) on the given address, default `127.0.0.1:8080`. It exposes a single
read path:

```
GET /blob/<CID>
```

This lets a stable `blob://<CID>` scheme be mapped to `http://<host:port>/blob/<CID>`
without the client learning about backends, storage paths, etc. The response is the
blob bytes, streamed (never buffered whole), with these headers:

- `Content-Type` — `application/json` if the CID's codec is `Json`, else `application/octet-stream`
- `Content-Length`
- `Content-Digest: sha-256=<base64(digest)>` — the stored multihash digest, for the client to verify against

The gateway is **read-only** (no upload); `add` covers writes. `/blob/<name>` is
deliberately not supported: a name has no CID, so there is no codec for
`Content-Type`/`Content-Digest` — use `blob get <name>` for that.

```bash
# serve in one terminal (dir backend, default path)
blob serve

# fetch in another
curl http://127.0.0.1:8080/blob/v060084h0p8uhcetb0l9o30ji4oa0aq7jnrrulb0ofjlteq3os2r3t7i44db0
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

## License

MIT
