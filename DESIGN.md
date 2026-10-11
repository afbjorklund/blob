# Design notes

Rationale for the choices in this store. Usage: README.md. Implementation details: AGENTS.md.

## Content-based codec, not extension-based

The CID's codec byte is chosen from the file's content: a file whose first byte is `{` or `[` is stored with the `Json` codec, everything else with `Raw` (`isJSON` in main.go).

- This distinguishes JSON OCI manifests/configs (`Json`) from binary OCI artifacts/layers such as tarballs and SIFs (`Raw`) without a separate object-type metadata store.
- CIDv1 carries the codec (a single multicodec byte) alongside the multihash, so the type travels with the address — no object-type or size column (which is what Git objects need); the CAS stays a pure bytes→bytes mapping.

## Name index

The name index is separate from the CAS and stores the **sha256 digest** — the exact key each backend uses for its blobs (`storage.Digest`) — not a CID string. This keeps each backend self-contained: a name resolves to a blob using only that backend's own data (no CID decoding step).

- Key is the file basename, expected to be a human-readable name (`ubuntu.sif`), *not* a CID — which is what keeps `get`'s CID-first resolution unambiguous.
- Two names can map to one digest, and **last add wins**, so `latest`-style aliases (e.g. `ubuntu_latest.sif` re-added as new content arrives) stay current.
- The digest is the key only; the CID (its codec + multibase) is *derived*, not stored. If a `lookup`/name→CID command is ever re-added it is **constant time regardless of blob size** and **never re-hashes**: the CAS already streams, so peeking the 1 magic byte (the same `isJSON` rule) is free, and the multihash is a wrap of the stored digest rather than a recompute — `SHA2-256` is one code byte `0x12` + the 32-byte digest, so `multihash = 0x12 || digest`. The CID is then `0x01` + codec + multihash, base32hex-encoded. A 1-byte read + composition replaces both the old "store the string" and the old full-file re-hashing: nothing is lost by dropping the CID, and a 4 GB SIF costs the same as a 12 B config.
- Dir backend: the index entry is a symlink `index/<name> -> ../<prefix>/<rest>` pointing at the blob, so reading the entry directly yields the content (`cat .blob-store/index/<name>` works). SQL backend: a `names (name, hash)` table where `hash REFERENCES blobs(hash)` — a real foreign key, since the index value is a blob key.
- `get` parses a CID to its digest first, and only falls back to the name index when the argument is not a CID, so a CID is never shadowed by a name.
- Per-name files / rows instead of a shared `index.json` as OCI registries use: each update is an atomic single-key write (symlink + rename, primary-key upsert), with no read-modify-write of one shared document.
- `add` records the name **after** the blob is confirmed stored, so a failed store leaves no dangling name.

## Deliberately small: what we are *not* doing

- No distributed name resolution. IPFS is also CID-based (`ipfs add`/`ipfs get` are the closest model), but being distributed it has no name index at this layer; names there come from IPNS/DNSLINK — a much larger mechanism (signatures, keys, global lookup) that local `add`/`get` round-tripping does not need.
- No `ipfs add -w`-style directory node: we prefer one content CID resolvable both by CID and by the original filename, rather than a second directory-node CID to memorize.
- No OCI-registry-style shared mutable `index.json`. If metadata beyond name → digest is ever needed, an append-only `index.txt`/`index.jsonl` log is the intended shape. For v1, name and digest are plenty.
