# AGENTS.md

Content-addressable blob store (CIDv1, base32hex) with `dir` and `sql` (SQLite) backends. Single Go module, no tests, no CI.

## Build & Commands

```bash
go build -o blob
golangci-lint run          # govet + gocyclo; gofmt/goimports formatters
```

- `blob add <file>` — store file (prints CID); `--only-hash`/`-n` to hash only
- `blob get <cid> -o <out>` — retrieve; `-o` defaults to using the CID string as filename
- Global flags: `--storage`/`-s dir|sql` (default `dir`), `--path`/`-p` (default `./.blob-store`)

## Non-Obvious Details

- CID codec depends on content, not extension: file stored with `Json` codec only if first byte is `{` or `[` (`isJSON` in main.go); anything else is `Raw`
- Dir backend key is hex (not the base32hex CID): `.blob-store/<first 2 hex chars>/<rest of hex>`, written via tmp-file + rename
- SQL backend auto-creates the parent dir of `--path` and the `blobs` table (storage/sql.go); no migration steps to run
- Storage backends never close the SQLite conn automatically from the CLI (no `Close` call in main.go)

## Conventions

- Go 1.26; `blob` binary and `.blob-store/` are gitignored
- Verify changes with `go build ./...` and, if available, `golangci-lint run`
