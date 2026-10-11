# Design notes

Rationale for the choices in this store. Usage: README.md. Implementation details: AGENTS.md.

## Content-based codec, not extension-based

The CID's codec byte is chosen from the file's content: a file whose first byte is `{` or `[` is stored with the `Json` codec, everything else with `Raw` (`isJSON` in main.go).

- This distinguishes JSON OCI manifests/configs (`Json`) from binary OCI artifacts/layers such as tarballs and SIFs (`Raw`) without a separate object-type metadata store.
- CIDv1 carries the codec (a single multicodec byte) alongside the multihash, so the type travels with the address — no object-type or size column (which is what Git objects need); the CAS stays a pure bytes→bytes mapping.

## Name index

The name index is separate from the CAS and stores the **CID string** (not the internal hash path), keeping it backend-agnostic.

- Key is the file basename, expected to be a human-readable name (`ubuntu.sif`), *not* a CID — which is what keeps `get`'s CID-first resolution unambiguous.
- Two names can map to one CID, and **last add wins**, so `latest`-style aliases (e.g. `ubuntu_latest.sif` re-added as new content arrives) stay current.
- Per-name files in the dir backend (one row in the sql backend) instead of a shared `index.json` as OCI registries use: each update is an atomic single-key write (tmp-file + rename, primary-key upsert), with no read-modify-write of one shared JSON document.
- `add` records the name **after** the blob is confirmed stored, so a failed store leaves no dangling name.
- `get` tries CID parsing first and only falls back to the name index when the argument is not a CID, so a CID is never shadowed by a name.

## Deliberately small: what we are *not* doing

- No distributed name resolution. IPFS is also CID-based (`ipfs add`/`ipfs get` are the closest model), but being distributed it has no name index at this layer; names there come from IPNS/DNSLINK — a much larger mechanism (signatures, keys, global lookup) that local `add`/`get` round-tripping does not need.
- No `ipfs add -w`-style directory node: we prefer one content CID resolvable both by CID and by the original filename, rather than a second directory-node CID to memorize.
- No OCI-registry-style shared mutable `index.json`. If metadata beyond name → CID is ever needed, an append-only `index.txt`/`index.jsonl` log is the intended shape. For v1, name and CID are plenty.
