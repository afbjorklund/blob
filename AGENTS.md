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
- `StorageBackend` exposes `Close()`: the `sql` backend closes its SQLite conn, while the `dir` backend's `Close()` is a no-op. `main.go` calls `defer storageBackend.Close()` right after constructing the backend in both `runAdd` and `runGet`
- The `sqlite` backend streams reads via `db.OpenBlob(..., write=false)` (returned as the `io.ReadCloser` from `Get`) instead of `ColumnBytes`, so a blob is never materialized in memory. `Get` opens the blob read-only and closes it through `reader.Close()` before the backend's `Close()` runs (declared in that order)

## Conventions

- Go 1.26; `blob` binary and `.blob-store/` are gitignored
- Verify changes with `go build ./...` and, if available, `golangci-lint run`
