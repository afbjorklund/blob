# AGENTS.md

A content-addressable blob store using CID (Content ID) addressing with pluggable storage backends.

## Quick Start

```bash
go build -o blob && ./blob add file.txt
```

## Storage Backends

- **Directory** (`dir`): Files stored in `.blob-store/<2-char-prefix>/<rest-of-hash>`
- **SQLite** (`sql`): Single database file at `--path`

## Verified Commands

- `blob add <file>` — Add file to store (computes CIDv1 with base32hex)
- `blob add <file> --only-hash` — Compute hash only, don't store
- `blob get <cid> -o <output>` — Retrieve file from store
- `blob --storage sql --path dbfile add <file>` — Use SQLite backend

## Key Details

- CID uses SHA2-256; JSON files get Json codec, others Raw codec
- Files stored by hash prefix (first 2 chars) in directory backend
- SQLite requires directory creation for db path
- `.blob-store` is gitignored; binary named `blob`
- Go 1.26, linters: govet, gocyclo; formatters: gofmt, goimports
