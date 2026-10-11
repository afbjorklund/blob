# blob

A content-addressable blob store using CID (Content ID) addressing with pluggable storage backends.

## Features

- **CIDv1 with base32hex encoding** - IPFS-compatible content addressing
- **Pluggable storage backends**:
  - Directory (`--path` dir; files sharded under `<first 2 hex chars of SHA-256>/<rest of hex>`)
  - SQLite (single database file at `--path`)
- **Streaming I/O** - Files are streamed to and from the backend without materializing the entire blob in memory. The `sqlite` backend reads via SQLite's blob API (`db.OpenBlob` in a read-only handle) instead of loading the whole row
- **Name index** - A separate, backend-agnostic name → CID map (symlink-style) lets you recover the CID for an original filename (e.g. `ubuntu_latest.sif` and `ubuntu_26.04.sif` both resolve to the same CID). Last add wins, so `latest`-style aliases update as new content is stored

## Usage

### Compute hash only

```bash
blob add --only-hash <file>
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
blob get <cid> -o <output-file>
```

Options:
- `-o <output-file>` - Output file path (defaults to the CID string as filename)

### Look up a CID by name

Every `add` records `basename(file) → CID` in the index. Use `lookup` to recover the CID for a name:

```bash
blob lookup <name>
```

- The index key is the file's **basename**, so paths and aliases with the same name all map to the same CID
- **Last add wins**: re-adding a name points it at the newest CID, which is how `latest`-style aliases stay current
- Names are not unique per CID — two names can map to the same blob (e.g. `ubuntu_26.04.sif` and `ubuntu_latest.sif`)
- `--only-hash` skips the index entirely

## Examples

### Directory backend (default)

```console
$ blob add test.txt
Added: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8

$ blob get v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8 -o output.txt
Retrieved: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8 -> output.txt

$ cat output.txt
test
```

Files are stored in `.blob-store/<prefix of hash>/<rest of hash>` and indexed in `.blob-store/index/<basename>`.

### Aliasing names to the same blob

```console
$ blob add ubuntu_26.04.sif
Added: v05ah48...j8dg

$ cp ubuntu_26.04.sif ubuntu_latest.sif
$ blob add ubuntu_latest.sif
File already exists in store: v05ah48...j8dg

$ blob lookup ubuntu_latest.sif
v05ah48...j8dg
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
