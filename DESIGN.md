# Design notes

Rationale for the choices in this store. Usage: README.md.
Implementation details: IMPLEMENTATION.md.

## Content-based codec, not extension-based

The CID's codec byte is chosen from the file's content: a file whose
first byte is `{` or `[` is stored with the `Json` codec, everything
else with `Raw` (the rule lives in `cid/CodecForFirstByte`;
`detectCodec` in main.go peeks the byte and calls it).

- This distinguishes JSON OCI manifests/configs (`Json`) from binary
  OCI artifacts/layers such as tarballs and SIFs (`Raw`) without a
  separate object-type metadata store.
- CIDv1 carries the codec (a single multicodec byte) alongside the
  multihash, so the type travels with the address — no object-type or
  size column (which is what Git objects need); the CAS stays a pure
  bytes→bytes mapping.

## Key

The CAS key is the **multihash** (algorithm code + digest) as opaque
`[]byte` — `storage.Digest`. The storage package has no multiformats
dependency: it treats this as an opaque key and does not interpret the
algorithm byte. The multihash's self-describing nature is a property
of the CID, which the *user-facing* layer builds from it; the storage
layer itself is algorithm-agnostic.

In the current implementation the multihash wraps a `crypto/sha256`
digest with the `multihash.SHA2_256` code (`main.go`'s `computeHash`,
built in `identity/`). If a different algorithm were ever used,
`computeHash` would produce that algorithm's multihash bytes directly
(no schema change in `storage/`).

**The key is carried, not verified, and has one facade between
worlds.** `identity.Identity` is the semantic name for that key and is
the single package that speaks *both* of the two spellings the project
carries — the multihash (the store's key) and the OCI digest
(go-digest, for `oci` import/export). It is a re-encode, not a
re-hash, and it gates on the algorithm (sha256 today; a second
algorithm is a one-case change *here*, with the bytes and the type
unchanged). Similarly, `cid/` is the one package that knows the
store's CID conventions over go-cid (base32hex render, content-based
codec, supported codecs), while `oci/` knows only go-digest + stdlib
and `storage/` knows only bytes. Each is a thin layer over its
library — the same way go-cid, go-multihash, and go-digest are each
thin over their spec.

**String presentation vs. storage key.** A `cid.Cid` carries
version, codec, and multihash — no base; the base is a render-time
parameter to `StringOfBase(base)`. This store renders user-facing strings as
**`base32hex`** — a 5-bits-per-character multibase alphabet `0-9a-v`
(whose first 16 characters, `0-9a-f`, are exactly base16hex) —
instead of IPFS's CIDv1 default **`base32`** (go-cid's `String()`
default), whose alphabet is `a`–`z` + `2`–`7` (no `0` or `1`). We
pick `base32hex` so CIDs read as an extension of hex. Each backend is
free to render the key (`storage.Digest`) in its own format
internally: `dir` uses hex of the bytes on the filesystem and `sql`
stores the bytes raw in a BLOB. The string form (presentation) and the
storage form (backend-specific key representation) are distinct
concerns.

## Name index

The name index is separate from the CAS and stores the blob **key**
(the multihash bytes) — *not* a CID string. This keeps each backend
self-contained: a name resolves to a blob using only that backend's
own data (no CID decoding step).

- Key is the file basename, expected to be a human-readable name
  (`ubuntu.sif`), *not* a CID — which is what keeps `get`'s
  CID-first resolution unambiguous.
- Two names can map to one key, and **last add wins**, so
  `latest`-style aliases (e.g. `ubuntu_latest.sif` re-added as new
  content arrives) stay current.
- The multihash is the key only; the CID (codec + multibase) is
  *derived*, not stored. Name→CID is therefore a **peek**, not a
  re-hash: `Peek(digest, 1)` reads the blob's first byte (a backend
  call, constant work regardless of blob size),
  `cid.CodecForFirstByte` applies the same content-based rule as
  `detectCodec`, and `cidForDigest` composes the CID
  (`cid.Build(key, codec)`) rendered with `cid.Render` (base32hex) —
  so a name resolves to the *exact* CID `add` printed. This is what
  both `lookup <name>` and the `/name` gateway rely on. The 1-byte
  read plus composition replaces the old "store the string" and the
  old full-file re-hashing: nothing is lost by dropping the CID string,
  and a 4 GB SIF costs the same as a 12 B config. (`lookup` is a pure
  convenience, like `hash`/`ipfs add --only-hash`; the CID is always
  recomputable from the store.)
- Dir backend: the index entry is a symlink
  `index/<name> -> ../<byte0>/<byte1>/<byte2>/<rest>` pointing at the
  blob (one `../` hop back to the store root, then the fanout path),
  so reading the entry directly yields the content
  (`cat .blob-store/index/<name>` works). SQL backend: a `names
  (name, hash)` table where `hash REFERENCES blobs(hash)` — a real
  foreign key, since the index value is a blob key.
- `get` parses a CID to its multihash first, and only falls back to
  the name index when the argument is not a CID, so a CID is never
  shadowed by a name.
- Per-name files / rows instead of a shared `index.json` as OCI
  registries use: each update is an atomic single-key write
  (symlink + rename, primary-key upsert), with no read-modify-write
  of one shared document.
- `add` records the name **after** the blob is confirmed stored, so a
  failed store leaves no dangling name.

## OCI import / export

An OCI image layout (directory or tar) is the same content-addressing
this store already does, just spelled differently. Three consequences
follow that make OCI *cheap* to support here rather than a new
subsystem:

- **The digests are the same bytes.** An OCI digest `sha256:<hex>` is
  this store's sha2-256 multihash key minus the 2-byte `12 20`
  code/size prefix. The `identity/` package is a one-point re-encode
  in each direction — `FromOCI`/`ToOCI` — **not a re-hash**. So an OCI
  import and a plain `add` of identical bytes land on the *same* CAS
  key and deduplicate for free; there is no second store to point at.
- **Layout = name → index → set of digests.** `oci add` treats the
  layout's `index.json` as the name's blob (it is just another content
  blob, stored like any other), then walks `index → manifest(s) →
  config + layers`, storing the *reachable* set. Blobs that are in a
  tar but referenced by nothing (e.g. the docker-archive half of a
  v25+ "hybrid") are never walked and never stored — we read the OCI
  half, ignore the docker half. A bare file (no layout) is wrapped as
  a single-layer **artifact**: the canonical empty-config `{}` + one
  layer.
- **Streams, no materialization.** A layer may be GB/TB, so
  `oci add`/`oci get` stream blobs between the layout and the backend
  byte-for-byte; only `index.json` and manifests are small enough to
  buffer.

The layering keeps each concern in one place: `oci/` speaks only
go-digest + stdlib (no multihash, CID, or backend), `identity/` is
the multihash↔digest bridge, and `main` bridges
`oci.Source`/`oci.Sink` to `storage.StorageBackend`. This is the same
"store is bytes, the rest are thin" split as `storage/` — and it is
why adding OCI did not touch the backends at all.

What OCI support is *not*: it is not registry I/O. There is no
`docker://`/`oras://` fetch, no tags, no push — input and output are
a local OCI layout (dir or tar), the same shape
`docker save`/`podman save` produce. The OCI-reference→CID convenience
mapping is a deliberately separate, planned wrapper.

## Deliberately small: what we are *not* doing

- No distributed name resolution. IPFS is also CID-based
  (`ipfs add`/`ipfs get` are the closest model), but being
  distributed it has no name index at this layer; names there come
  from IPNS/DNSLINK — a much larger mechanism (signatures, keys,
  global lookup) that local `add`/`get` round-tripping does not need.
- No `ipfs add -w`-style directory node: we prefer one content CID
  resolvable both by CID and by the original filename, rather than a
  second directory-node CID to memorize.
- The HTTP gateway (`blob serve`) leads with the canonical address,
  `GET /blob/<CID>`, mirroring the IPFS gateway: the CID carries the
  codec (→ `Content-Type`) and multihash (→ `Content-Digest`) without
  reading content. `GET /name/<name>` exists on top of that and does
  *not* duplicate the streaming logic — it resolves the name,
  **peeks the first byte to derive the CID**, and `302`-redirects to
  `/blob/<CID>` (a true 302, since last-add-wins makes the mapping
  mutable). So name serving is a redirect to the single canonical
  content path, not a second serving path; the only name-specific work
  is the one-byte peek. This mirrors how the dir backend already
  models "name → blob" as a symlink (a reference to the canonical
  path) rather than a second copy of the content.
- No **push** OCI-*registry* protocol, by scoping decision: network
  push/save/load is left to the stock OCI client of choice (podman,
  oras, skopeo, crane, buildkit) talking to whatever registry it
  speaks. `blob`'s two doors are (a) `oci import`/`oci export`,
  reading and writing a local OCI **layout** (dir or tar — the on-disk
  form `docker save`/`podman save` write), and (b) the `blob serve`
  registry endpoint, which is **read-only**: no blob-upload sessions,
  no `DELETE`, no tag listing, no token dance — a read-only backend
  (`storage.ReadOnly`) plus a read-only handler is the whole surface.
  And no shared mutable `index.json` as the *name* store (as
  registries use): our name index stays the per-name atomic upsert
  described above; the layout's read-only `index.json` is just one
  blob we happen to store under a name. If richer metadata beyond name
  → digest is ever needed, an append-only `index.txt`/`index.jsonl`
  log is the intended shape. For v1, name and digest are plenty.
- **Metadata, if any, must be a pure function of the content
  (reproducible) — never the clock or client state.** That follows
  from content addressing: the store key *is* a function of the
  bytes, so re-importing the same content must land on the same key,
  and anything that injects wall-clock or client-local time (e.g.
  oras-go stamping `org.opencontainers.image.created` with
  `time.Now()` on push, so the *same* manifest hashes differently
  every time) breaks that. The honest way to satisfy a field a
  spec/client demands is to derive it from the content — as Apptainer
  does with its created-time (hence reproducible, and a clean reason
  to build a write path on `go-containerregistry` rather than
  oras-go) — or to omit it. Symmetrically, on the read side
  Apptainer pulls by digest (`docker://`/`oras://` only contributes
  the registry+repo, per the OCI distribution spec) and never
  requires `title`; the oras CLI's "unnamed layers are skipped" is
  the same client heuristic, not an OCI requirement. The default
  stays "no metadata": the CAS is the bytes and nothing else. (The
  `org.opencontainers.image.title` layer-skip in oras is the same
  class of issue — a client heuristic treating an optional annotation
  as mandatory, not an OCI requirement.)

## Possible additions: what we haven't done **yet**

- No **`rm`**, yet. A name-only `rm <name>` (drop the index link,
  blob stays) is the safe, cheap half — one bounded index entry,
  no consistency invariant to check, no CAS walk — and is the
  natural first step once asked for. A blob-level `rm <cid>` is a
  different shape: it is a *removal-of-reference* operation, not a
  filesystem `rm`, and the design constraint is "never leave a
  dangling name or a shared layer orphaned." Concretely, a `rm <cid>`
  would need: (1) to also drop every name that maps to the CID
  (dangling-index guard, bounded on every backend — `SELECT
  WHERE hash = ?`, `ReadDir` with a filter, prefix listing), and
  (2) an *orphan check* of the inverse Reachability walk — for
  every name `n`, "is this CID a member of the graph reachable
  from `n`?" — to refuse the deletion if the blob is referenced
  by another name's layout. The walk already exists
  (`oci.Reachable`, forward from a root); the *reverse* (who
  references this) requires enumerating all names and walking
  forward from each, which on http is an unbounded N wire cost.
  That's the same cost that motivates deferring `du` (below).
  The honest escape hatch today is the operator's: the store's
  physical layout (dir files, sql rows, S3 keys) is the admin
  surface, and `blob cid` gives you the multihash to find it.
- No **`du` / list-blobs**, yet. The cost and even implementability
  are backend-dependent: cheap on sql (`SELECT hash, length(data)
  FROM blobs`), bounded on s3 (`ListObjects` already carries
  `ContentLength`), more work on dir (`stat` per fanout leaf), and
  unbounded on http (N wire round-trips to `HEAD` each entry).
  The design consequence is a clean split: `list` enumerates the
  *bounded* index (one round-trip on every backend, names only,
  never touches the CAS), and per-blob sizes are resolvable one at
  a time on demand via `get --only-size` / `--only-mtime`. A
  whole-CAS enumeration is a deliberately separate command once
  asked for (a `du` or `gc`), because it would inherit the same
  backend-dependent cost profile as a blob-level `rm`.
