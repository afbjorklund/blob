# Design notes

Rationale for the choices in this store. Usage: README.md. Implementation details: AGENTS.md.

## Content-based codec, not extension-based

The CID's codec byte is chosen from the file's content: a file whose first byte is `{` or `[` is stored with the `Json` codec, everything else with `Raw` (`isJSON` in main.go).

- This distinguishes JSON OCI manifests/configs (`Json`) from binary OCI artifacts/layers such as tarballs and SIFs (`Raw`) without a separate object-type metadata store.
- CIDv1 carries the codec (a single multicodec byte) alongside the multihash, so the type travels with the address — no object-type or size column (which is what Git objects need); the CAS stays a pure bytes→bytes mapping.

## Key

The CAS key is the **multihash** (algorithm code + digest) as opaque `[]byte` — `storage.Digest`. The storage package has no multiformats dependency: it treats this as an opaque key and does not interpret the algorithm byte. The multihash's self-describing nature is a property of the CID, which the *user-facing* layer builds from it; the storage layer itself is algorithm-agnostic.

In the current implementation the multihash wraps a `crypto/sha256` digest with the `multihash.SHA2_256` code (`main.go`'s `computeDigest` + `computeCID`). If a different algorithm were ever used, `computeDigest` in `main.go` would produce that algorithm's multihash bytes directly (no schema change in `storage/`).

## Name index

The name index is separate from the CAS and stores the blob **key** (the multihash bytes) — *not* a CID string. This keeps each backend self-contained: a name resolves to a blob using only that backend's own data (no CID decoding step).

- Key is the file basename, expected to be a human-readable name (`ubuntu.sif`), *not* a CID — which is what keeps `get`'s CID-first resolution unambiguous.
- Two names can map to one key, and **last add wins**, so `latest`-style aliases (e.g. `ubuntu_latest.sif` re-added as new content arrives) stay current.
- The multihash is the key only; the CID (codec + multibase) is *derived*, not stored. If a `lookup`/name→CID command is ever re-added it is **constant time regardless of blob size** and **never re-hashes**: the CAS already streams, so peeking the 1 magic byte (the same `isJSON` rule) is free; the CID is then `cid.NewCidV1(uint64(codec), mh).StringOfBase(multibase.Base32hex)` where `codec` is `multicodec.Raw` or `multicodec.Json`. A 1-byte read + composition replaces both the old "store the string" and the old full-file re-hashing: nothing is lost by dropping the CID string, and a 4 GB SIF costs the same as a 12 B config.
- Dir backend: the index entry is a symlink `index/<name> -> ../<prefix>/<rest>` pointing at the blob, so reading the entry directly yields the content (`cat .blob-store/index/<name>` works). SQL backend: a `names (name, hash)` table where `hash REFERENCES blobs(hash)` — a real foreign key, since the index value is a blob key.
- `get` parses a CID to its multihash first, and only falls back to the name index when the argument is not a CID, so a CID is never shadowed by a name.
- Per-name files / rows instead of a shared `index.json` as OCI registries use: each update is an atomic single-key write (symlink + rename, primary-key upsert), with no read-modify-write of one shared document.
- `add` records the name **after** the blob is confirmed stored, so a failed store leaves no dangling name.

## Deliberately small: what we are *not* doing

- No distributed name resolution. IPFS is also CID-based (`ipfs add`/`ipfs get` are the closest model), but being distributed it has no name index at this layer; names there come from IPNS/DNSLINK — a much larger mechanism (signatures, keys, global lookup) that local `add`/`get` round-tripping does not need.
- No `ipfs add -w`-style directory node: we prefer one content CID resolvable both by CID and by the original filename, rather than a second directory-node CID to memorize.
- No OCI-registry-style shared mutable `index.json`. If metadata beyond name → digest is ever needed, an append-only `index.txt`/`index.jsonl` log is the intended shape. For v1, name and digest are plenty.
