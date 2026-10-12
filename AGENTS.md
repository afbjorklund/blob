# AGENTS.md

Content-addressable blob store (CIDv1, base32hex) with `dir` and `sql` (SQLite) backends. Single Go module, no tests, no CI.

## Build & Commands

```bash
go build -o blob
golangci-lint run          # govet + gocyclo; gofmt/goimports formatters
```

- `blob add <file>` — store file (prints CID). Records `basename(file) → multihash` in the name index
- `blob add --only-hash/-n <file>` — like `ipfs add --only-hash`: print `added <cid> <file>` (one space), do not store
- `blob hash <file>` — print `sha256sum`-style `<cid>  <file>` (two spaces) without storing (equivalent to what `ipfs add --only-hash` gives after the `added` prefix is stripped)
- `blob get <cid|name> -o <out>` — retrieve; argument is parsed as a CID first, falling back to the name index (so `get` round-trips a name passed to `add`); `-o` defaults to using the argument as filename
- Global flags: `--storage`/`-s dir|sql` (default `dir`), `--path`/`-p` (default `./.blob-store`)
- Env defaults: `BLOB_STORAGE_TYPE` (type) and `BLOB_STORAGE_PATH` (path) are used as the flag defaults when the flag is not explicitly set; the flag always wins

## Non-Obvious Details

- CID codec depends on content, not extension: file stored with `Json` codec only if first byte is `{` or `[` (`isJSON` in main.go); anything else is `Raw` (why: DESIGN.md)
- The storage key is the **multihash** (algorithm + digest bytes), opaque `[]byte` (`storage.Digest`). The storage package has no multiformats dependency — it never decodes the multihash, so adding a different hash algorithm (or a different codec) is a `main.go` change only, never a storage change. `computeHash(path) → multihash.Multihash` produces the bytes; `computeCID(mh, isJSON) → cid.Cid` builds the CID (version + codec + multihash — a CID carries no base, that is a render-time parameter); we present it as base32hex via `StringOfBase(multibase.Base32hex)` — this store's choice: the `0-9a-f` alphabet of `base32hex` instead of the `yzabcde…` alphabet of `base32` (go-cid's `String()` default for v1, the one IPFS uses); the key stored is `c.Hash()` (the multihash itself), and `digestFromCID` inverts that at `get` time.
- Dir backend key is the hex of the multihash bytes (not the base32hex CID): `.blob-store/<first 2 hex chars>/<rest of hex>`, written via tmp-file + rename
- SQL backend auto-creates the parent dir of `--path` and the `blobs (hash BLOB PRIMARY KEY, data BLOB)` table (storage/sql.go); no migration steps to run
- Name index is separate from the CAS and stores the blob **key** (the multihash bytes — `storage.Digest` = `[]byte`), *not* a CID string, so each backend resolves a name to a blob with nothing but its own data: dir backend uses `.blob-store/index/<name>` symlinks pointing `../<first 2 hex>/<rest>` at the blob (written as symlink + rename on a `.tmp` name), sql backend uses a `names (name TEXT PRIMARY KEY, hash BLOB NOT NULL REFERENCES blobs(hash))` table (foreign key, `PRAGMA foreign_keys = ON` on each connection). Key is the file basename (expected to be a human-readable name like `ubuntu.sif`, not a CID, so `get`'s CID-first lookup is unambiguous); two names can map to one key and **last add wins** (`Index` overwrites), so `latest`-style aliases stay current. `runAdd` indexes after confirming the blob is stored (so a `Put` failure leaves no dangling name), and still indexes when the blob already exists. The multihash is the key only — the CID (codec + multibase) is derived and is not stored; the dir index symlink follows to content directly, while sql needs a join on `hash` (the FK exists for integrity). Symlinks are Linux-only here; Windows fallback (git's `core.symlinks`-style text target in the file) is the intended migration if ever needed. Per-name files (one row in sql) instead of a shared `index.json` (as OCI registries use): each update is an atomic single-key write (tmp + rename / primary-key upsert), no read-modify-write of one JSON document. This is fine for this store's scale; IPFS avoids any index at this layer since it is content-addressed and distributed, and only gains name resolution via IPNS/DNSLINK — a much larger mechanism (signatures, keys, global lookup) that local `add`/`get` round-tripping does not need
- `StorageBackend` exposes `Close()`: the `sql` backend closes its SQLite conn, while the `dir` backend's `Close()` is a no-op. `main.go` calls `defer storageBackend.Close()` right after constructing the backend in `runAdd` and `runGet`
- The `sqlite` backend streams reads via `db.OpenBlob(..., write=false)` (returned as the `io.ReadCloser` from `Get`) instead of `ColumnBytes`, so a blob is never materialized in memory. `Get` opens the blob read-only and closes it through `reader.Close()` before the backend's `Close()` runs (declared in that order)

## Conventions

- Go 1.26; `blob` binary and `.blob-store/` are gitignored
- Verify changes with `go build ./...` and, if available, `golangci-lint run`
