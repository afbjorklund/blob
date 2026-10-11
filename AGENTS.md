# AGENTS.md

Content-addressable blob store (CIDv1, base32hex) with `dir` and `sql` (SQLite) backends. Single Go module, no tests, no CI.

## Build & Commands

```bash
go build -o blob
golangci-lint run          # govet + gocyclo; gofmt/goimports formatters
```

- `blob add <file>` — store file (prints CID); `--only-hash`/`-n` to hash only. Records `basename(file) → CID` in the name index (skipped with `--only-hash`)
- `blob get <cid> -o <out>` — retrieve; `-o` defaults to using the CID string as filename
- `blob lookup <name>` — print the CID for an indexed name (the file basename)
- Global flags: `--storage`/`-s dir|sql` (default `dir`), `--path`/`-p` (default `./.blob-store`)

## Non-Obvious Details

- CID codec depends on content, not extension: file stored with `Json` codec only if first byte is `{` or `[` (`isJSON` in main.go); anything else is `Raw`. Rationale: this distinguishes JSON OCI manifests/configs (→ `Json`) from binary OCI artifacts/layers like tarballs and SIFs (→ `Raw`) without a separate metadata store. CIDv1 carries the codec (a single multicodec byte) alongside the multihash, so the type travels with the address — no separate object-type/size column (cf. Git objects needed); the CAS stays a pure bytes→bytes mapping
- Dir backend key is hex (not the base32hex CID): `.blob-store/<first 2 hex chars>/<rest of hex>`, written via tmp-file + rename
- SQL backend auto-creates the parent dir of `--path` and the `blobs` table (storage/sql.go); no migration steps to run
- Name index is separate from the CAS and stores the **CID string** (not the internal hash path), keeping it backend-agnostic: dir backend uses `.blob-store/index/<name>` text files (written tmp-file + rename), sql backend uses a `names (name TEXT PRIMARY KEY, cid TEXT)` table created alongside `blobs`. Key is the file basename; two names can map to one CID and **last add wins** (`Index` overwrites), so `latest`-style aliases stay current. `runAdd` indexes after confirming the blob is stored (so a `Put` failure leaves no dangling name), and still indexes when the blob already exists
- `StorageBackend` exposes `Close()`: the `sql` backend closes its SQLite conn, while the `dir` backend's `Close()` is a no-op. `main.go` calls `defer storageBackend.Close()` right after constructing the backend in `runAdd`, `runGet`, and `runLookup`
- The `sqlite` backend streams reads via `db.OpenBlob(..., write=false)` (returned as the `io.ReadCloser` from `Get`) instead of `ColumnBytes`, so a blob is never materialized in memory. `Get` opens the blob read-only and closes it through `reader.Close()` before the backend's `Close()` runs (declared in that order)

## Conventions

- Go 1.26; `blob` binary and `.blob-store/` are gitignored
- Verify changes with `go build ./...` and, if available, `golangci-lint run`
