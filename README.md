# blob

A content-addressable blob store using CID (Content ID) addressing with pluggable storage backends.

## Features

- **CIDv1 with base32hex encoding** - IPFS-compatible content addressing
- **Pluggable storage backends**:
  - Directory (`--path` dir; files sharded under `<first 2 hex chars of SHA-256>/<rest of hex>`)
  - SQLite (single database file at `--path`)
- **Streaming writes** - Files are streamed to the backend without loading the entire file into memory (the SQLite backend does buffer full blobs in memory on read)

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

Files are stored in `.blob-store/<prefix of hash>/<rest of hash>`.

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
