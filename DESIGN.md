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
digest with the `multihash.SHA2_256` code (`main.go`'s `cidFromReader`,
built in `identity/`). If a different algorithm were ever used,
`cidFromReader` would produce that algorithm's multihash bytes directly
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
  convenience, like `sum`/`ipfs add --only-hash`; the CID is always
  recomputable from the store.)
- Dir backend: the index entry is a symlink
  `index/<name> -> ../<byte0>/<byte1>/<byte2>/<rest>` pointing at the
  blob (one `../` hop back to the store root, then the fanout path),
  so reading the entry directly yields the content
  (`cat .blob-store/index/<name>` works). SQL backend: a `names
  (name, hash)` table where `hash REFERENCES blobs(hash)` — a real
  foreign key, since the index value is a blob key. HTTP/S3 backends
  file the same link as a single object, `index/<name>` whose body is
  the hex multihash — the server-side counterpart of the dir symlink
  and the sql foreign key (the on-wire shape is in
  [BACKEND.md](BACKEND.md)).
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
  half, ignore the docker half. A legacy-only docker archive (a tar
  with `manifest.json` and no `index.json`) is upgraded in place: its
  `manifest.json` names the same blobs by their own sha256 digest, so
  a synthesized `index.json` + manifest references them directly and
  the walk stores the config and layers under their own digests.
  Only a file with *no* layout at all (no `index.json`, no
  `manifest.json`) is wrapped as a single-layer **artifact**: the
  canonical empty-config `{}` + one layer of the file's own bytes.
- **The artifact is a pure function of the layer.** For a single
  layer, the whole OCI scaffold (`index.json`, its manifest, the
  `{}` config) is determined by content alone — the layer digest,
  its size, and the SIF-vs-generic media types, all re-derived from
  the bytes. We exploit this on the read/interop side: `oci export`
  and the `/v2/` registry surface serve the scaffold **from memory**,
  constructed on demand, and *never persist it* — so a `blob add`-ed
  file and an `oci import`-ed copy are both exportable and pullable as
  an OCI artifact, with no OCI JSON added to the CAS. `oci ls` keeps
  the opposite policy (it *reports* the store): it lists an artifact
  only when `oci import` actually persisted its scaffold.
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

## Foreign digests as names

A *name* in the store's index is a plain string, and nothing requires
it to be the store's CID. `add` and `oci import` both accept a
**foreign digest or reference** — an IPFS CID from `ipfs add` (the
default `ipfs add` shape is a `dag-pb` CID over the content, or a
raw-leaf piece CID), or an OCI registry reference like
`docker.io/library/busybox@sha256:…` — and record it as a plain *name*
(the label) for the content this store holds. The *content that foreign
digest names* (an IPFS piece, a registry manifest) is **not** stored
here: the store's invariant that a digest identifies a blob *in this
CAS* is preserved by treating the foreign string as just a label the
user chose, not as a digest we would resolve and pull.

The two foreign spellings are **separable up-front, without content
inspection**: an IPFS CID carries the `dag-pb` (or raw) codec, which
the store does not hold (our store's CIDs are JSON or Raw, and their
contents are always the same as the store's CAS), and an OCI reference
is either `registry[:tag]@sha256:…` (which is not a CID at all — a
scheme-qualified string) or a bare `sha256:…` (which parses as the
store's own digest form). The shape rules already keep the namespaces
apart; the name-index table (`index/<name>` in the dir backend, a
`names` row in sql, the object in s3/http) is just where we file them.

The one **liberty** worth knowing: a real registry's
`registry@digest` for a *multiarch* image points to the multiarch
index that references every platform's manifest. We never speak to a
registry — the user does the platform filtering and saves one platform
with `docker save` (which is what `--platform <…>` does at pull time).
So the *same* `registry@digest` string in a real registry (→ the
multiarch index) is a *different* identity in our store (→ the manifest
and config of the single local image the user actually saved, with its
own CID keyed to those bytes). The platform label is metadata inside
the blob's config and manifest, not a first-class field we store: it
is the content, and the content decides the CID. The user may re-map a
`registry@digest` label to whichever local CID they like; the **CID
is the non-mutable reference**, and the label remains a mutable alias
the user is free to remap.

## Origin references as names

The same multiarch liberty has a second face: the *multi-arch index*
digest for an image is an identifier the user has, but it *does not
name content in the CAS* — it is the hash of an index we do not store.
A re-save of the same image, or the same image converted to a SIF, has
different bytes and a different digest, yet the user still means "the
image I pulled as `...@sha256:X`". So we record the **origin** (the
foreign digest **plus** the classification the content needs) as a
first-class name, and let each `(variant, os, arch, digest)` be a
separate, unambiguous entry. If we let `registry@digest` collide across
arches, a linux/amd64 save and a linux/arm64 save of the same upstream
image would fight over the same name. So the two are **disambiguated by
shape**, in a name class of their own:

- **Shape**: `oci/<variant>/<os>/<arch>/<algo>/<hex>` (e.g.
  `oci/blob/linux/amd64/sha256/a46257e0…`). Each component is a
  classification: `variant` (what the leaf names), `os`/`arch`
  (the platform), `algo`/`hex` (the foreign digest). Only the leaf
  carries an identifier; the rest are classifications.
- **Variant**: what the name points at. `blob` — a blob (raw file bytes: a
  SIF, a tar, a layer, a config), the thing `apptainer --basepath` wants;
  `json` — the OCI index / manifest (the layout `docker`/`podman save`
  produces, or the single-layer scaffold `oras`/`apptainer push` produce).
  An OCI image and an OCI artifact are the same JSON structure, so they
  share the one `json` value. It is not a flag: it follows the input shape —
  `oci import <bare-file>` is always `blob`, `oci import <layout>` is always
  `json`. This lets two co-exist under one `(os, arch, digest)` — the SIF
  and the OCI layout — each at its own path, each naming its own stored CID.
  The flags are an OCI concern and live only on `oci import`: a plain
  `blob add` records only the basename and never files an `oci/` name.
- **Source**: the `--digest` flag (plus `--platform` or `--arch`) on
  `oci import`. `--digest` accepts the full OCI digest (`sha256:<hex>`)
  or a bare hex (assumed sha256); non-sha256 algos are rejected at import —
  the store only bridges that one.
- **Storage**: a plain name, like any other — one flat segment in the
  name index (the `/`s are escaped via the same `index.Encode` every
  registry-style name uses), resolved through the same name-index path
  as `blob get`/`lookup`/`cat` and served by the same `/name/<name>`
  gateway. No new backend API, no new mount option.
- **Mount surface**: it is also projected as the `oci/` top directory
  in the FUSE view — a 5-level directory tree (variant / os / arch /
  algo / hex) where the leaf is a symlink to `../../blob/<CID>`. A
  client that wants a flat base path like apptainer's `--basepath`
  passes `<mount>/oci/blob/<os>/<arch>/<algo>` and reads the file
  `<hex>` under it.

The shape is chosen to *avoid* the direct-handle path in the resolver:
`oci/...` is not a CID, not a bare OCI digest, and not a digest-shaped
name, so it flows straight to the name-index lookup. That is the
mechanism that lets it "tolerate the missing upstream index" safely —
the foreign identifier is a *label for* the content, never an
assertion *of* the content.

And the two are **decoupled**: the foreign name can drift while our
CID stays put. The user can re-chunk a file for IPFS (a different
foreign raw CID, the *same bytes* → the same store CID), or move an
image from one registry to another (a different `registry@digest`, the
same content → the same store CID) — different foreign addresses, one
and the same blob here.

One caveat on what "the CID stays put" means: it is stable for the
*algorithm* we use, SHA-2 256 today. We *could* swap in SHA-3 *today*
— the multihash is algorithm-qualified, so the content re-addresses
under a different but structurally similar CID: same layout, the
algorithm code and digest length simply vary (pick an equal bit count,
e.g. SHA3/SHAKE-256 against SHA-2 256, and the digest — and so the
CID's length — stays the same; another bit count, e.g. 512, gives a
longer digest in the same shape). We are not saying we should; the
point is only that the format already accommodates it.

Because the algorithm sits *inside* the multihash, a blob hashed SHA-2
and the *same* blob hashed SHA-3 are two distinct, co-resident
addresses in one space, not one identity rewritten. A migration would
re-point the *names* (the "label" mechanism already used for foreign
IPFS CIDs and `registry@digest` strings) at the new digest while the
old blobs stay reachable by their own multihash/CID — a re-label, not a
rupture. That is the opposite of Git's `sha1 → sha256` situation, where
remotes, forks and submodules keyed on the old digests are stranded;
here the multihash absorbs a rehash. The same mechanism extends to any
*other* digest scheme a user wants to cross-reference — a Git OID, for
example, is a 20-byte SHA-1 over a `<type> <length>\0<content>` header,
not a multihash at all. It cannot be our CID; but it *can* be a *name*
we accept alongside IPFS CIDs and `registry@digest` strings, and it
resolves the same way those do.

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
