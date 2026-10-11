# blob

A content-addressable blob store using CID (Content ID) addressing with pluggable storage backends.

## Features

- **CIDv1 with base32hex encoding** - IPFS-compatible content addressing
- **Pluggable storage backends**:
  - Directory (2-char prefix with full hash filename)
  - SQLite (single database file)
- **Streaming I/O** - Handles large files without loading into memory

## Usage

### Compute hash only

```bash
blob <file>
blob hash <file>
```

### Add to blob store

```bash
blob add <file>
```

Options:
- `--storage dir|sqlite` - Storage backend (default: `dir`)
- `--path <dir|dbfile>` - Storage location

### Get from blob store

```bash
blob get <cid> <output-file>
```

## Examples

### Directory backend (default)

```console
$ blob add test.txt
Added: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8

$ blob get v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8 output.txt
Retrieved: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8 -> output.txt

$ cat output.txt
test
```

Files are stored in `.blob-store/<prefix>/<digest>`.

### SQLite backend

```console
$ blob --storage sqlite --path blobs.db add test.txt
Added: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8

$ blob --storage sqlite --path blobs.db get v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8 output.txt
Retrieved: v05ah487ip8drdhv90v86rbv4d1v5f7ueeqpnsjkjmtg508mqabjcpgjfq8 -> output.txt

$ cat output.txt
test
```

## License

MIT
