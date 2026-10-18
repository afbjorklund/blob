# Concepts

An introduction to the ideas behind `blob`: content addressing (CID), a
content-addressable store (CAS), the name index, and the read-only HTTP
gateway. This borrows the framing of IPFS — address data by *what it is*, not
*where it sits* — and keeps only the parts that apply to a plain, reachable
store. If new to the territory,
[How IPFS works](https://docs.ipfs.tech/concepts/how-ipfs-works/#subsystems-overview)
is a good companion.

One word on scope, because it colors everything below: `blob` is
**content-addressed and shared-by-agreement**, not distributed. The store can
sit on one machine, or on a backend a whole cluster points at (a shared
directory on NFS, an HTTP server, or S3) — but you already *know where it is*
(you configure `--path`), so nothing has to be *discovered*. IPFS is global
and peer-to-peer; `blob` is "local" only in the sense of *your*
infrastructure, not the open internet.

## Content addressing (CID)

Most systems point to data by *location*: a path on a disk, an IP address, a
URL. If anything changes where the data lives (a move, a rename, a different
machine), the pointer breaks.

Content addressing flips this. Instead of a location, data is identified by a
hash of its content. Change a single byte and the hash changes — so the
identifier is a fingerprint of *what the data is*. Two consequences IPFS
emphasizes, and this store relies on too:

- **Fetch by content, not location.** You ask for "the bytes whose SHA-256 is
  X", and it does not matter which backend or path holds them.
- **Verify by recomputation.** Anyone who fetches the data can hash it and
  confirm it matches the identifier — a tamper- or corruption check for free.

A **CID** (Content ID) is that identifier. It is the content's hash (a
*multihash*: an algorithm code plus the digest) plus a *codec* — a tag for
what the bytes *are* (JSON vs. raw bytes). The address is self-describing:
from the CID alone you can tell how the content was hashed and what kind of
thing it is, without consulting any metadata store.

In `blob`, the CID is CIDv1 rendered in **base32hex** — a 5-bits-per-character
multibase encoding using the alphabet `0-9a-v` (whose first 16 characters,
`0-9a-f`, are exactly base16hex), e.g.

```
v05ah4859p5vrofmfpdv5i8om998c9k4gd0ge2shctr5jofc6ufj3ghgf8k
```

Note the `p`, `u`, `v` in that string — they come from `base32hex`'s `g-v`
tail, which plain base16 has no room for. The **back-end** key, by contrast,
is plain `base16` hex of the multihash digest bytes (`0-9a-f`, 16 characters)
— the fanout directories you see in the dir backend, and the object key the
`http`/`s3` backends transmit. So the CID (front-end → `base32hex`,
`0-9a-v`) and the multihash key (back-end → `base16`, `0-9a-f`) are
deliberately *different encodings of the same bytes*. IPFS's CIDv1 default is
`base32` — a different 32-char alphabet, `a`–`z` + `2`–`7`; `blob` picks the
hex-flavoured `base32hex` (which extends base16hex) so CIDs read like hex.

## Content-addressable store (CAS)

A **CAS** is a store keyed by content address: the key *is* the CID (really,
the multihash inside it), and the value is the bytes. Because the key is
computed from the content:

- **Deduplication for free.** Add the same file twice — or under two names —
  and it is stored once. The second add finds the key already present and
  simply succeeds.
- **No separate metadata for identity.** The address already carries the
  hash and the type; there is no object-ID column, no "size" column needed to
  tell one blob from another.
- **Integrity by construction.** A fetch returns the bytes that were hashed
  into the key, so a client can hash the response and confirm it matches.

`blob`'s backends (a sharded directory, SQLite, a remote HTTP server, or S3)
are all realizations of this same CAS idea: put bytes in under their
multihash, take bytes out by their multihash. The key is treated as opaque
bytes by the storage layer — it never decodes the hash algorithm, so the store
is agnostic to which hash or codec is used.

## Name index

A content address is stable but not *human*: nobody wants to quote
`v05ah4859…` in a command when they mean `ubuntu_latest.sif`.

The name index is the bridge. It is a **separate** mapping from a
human-readable name to the content address of the stored blob — *not* part of
the CAS itself, and it stores the hash, not the CID string (the CID is
re-derived from the bytes when you need it). This mirrors how IPFS needs a
name-resolution layer (IPNS/DNSLink) to get from a friendly name to a CID;
`blob` replaces that heavy, signed, global mechanism with a local, per-name
link:

```console
$ blob add ubuntu_26.04.sif
Added: v05ah4859p5vrofmfpdv5i8om998c9k4gd0ge2shctr5jofc6ufj3ghgf8k

$ cp ubuntu_26.04.sif ubuntu_latest.sif
$ blob add ubuntu_latest.sif       # same content → same CID, stored once
File already exists in store: v05ah48...j8dg

$ blob get ubuntu_latest.sif -o latest.sif
Retrieved: ubuntu_latest.sif -> latest.sif
```

Two behaviors worth noting, both aimed at the common "point an alias at the
latest thing" workflow:

- **Many names, one blob.** `ubuntu_26.04.sif` and `ubuntu_latest.sif` can
  both resolve to the same content.
- **Last add wins.** Re-adding a name repoints it at the newest blob, so a
  `latest` alias stays current as new content is stored.

`get` tries to parse the argument as a CID first, and only falls back to the
name index if it is not a valid CID — so a name can never shadow a real
content address.

## HTTP gateway

A **gateway** is the idea that HTTP tools (`curl`, browsers, scripts) can
reach content-addressed data through a plain HTTP interface, without knowing
how the data is actually stored. IPFS does this to let ordinary HTTP clients
talk to the whole network; `blob serve` does the same over one store you point
at — a **read-only** gateway:

```console
$ blob serve                       # default 127.0.0.1:8080

$ curl http://127.0.0.1:8080/blob/v05ah48...j3ghgf8k
$ curl -L http://127.0.0.1:8080/name/ubuntu_latest.sif
```

Two read paths, one canonical:

- `GET /blob/<CID>` — stream the bytes. The response is never buffered whole,
  and carries `Content-Digest: sha-256=…` (the stored multihash digest) so a
  client can verify what it fetched, and `Content-Type` derived from the
  CID's codec.
- `GET /name/<name>` — resolve the name to its CID, then **302-redirect** to
  the canonical `/blob/<CID>`. It does not duplicate the serving path; it is
  the same name→blob pointer the dir backend keeps as a symlink on disk. A
  302 (not 301) rather than a permanent redirect, because the name→CID
  mapping is mutable (last add wins).

The gateway makes a stable `blob://<CID>` scheme mappable to
`http://<host>/blob/<CID>` without the client learning anything about
backends, storage paths, or the hash — the client only ever sees the
self-describing CID.

## How `blob://` compares to `oras://` and `ipfs://`

If you come from one of the other content-addressed schemes, `blob` should feel
familiar. All three rest on the same idea — *identify content by a hash of it
rather than by where it sits* — and so all three deduplicate the same bytes and
can hand you a stable handle plus a mutable alias on top:

|                | stable content handle | mutable name      | how you reach it                |
|----------------|-----------------------|-------------------|---------------------------------|
| `ipfs://`      | CID                   | IPNS / DNS-link   | peer network — **discover** peer|
| `oras://`      | digest (+ manifest)   | tag               | a **registry** you push/pull to |
| `blob://`      | CID                   | name index        | a **store** you point `--path` at|

Read the last column as the difference that matters. They share *how content
is addressed*; they differ in *how content is found and fetched*:

- **`ipfs://` is distributed.** A node asks the peer network (DHT, then
  Bitswap) *who has* that CID and pulls it from a peer you've never met; IPNS
  names are signed so they resolve globally. The handle is global trust-free,
  and the price is the whole network machinery.
- **`oras://` is registry-based.** You know the registry; you push/pull over
  its protocol, and a *tag* (e.g. `latest`, last-push-wins) points to a
  manifest that references its layers. Shared, but the registry is a defined
  service you authenticate to.
- **`blob://` is configured.** There is nothing to discover and no registry
  protocol: the *address* (`blob://<CID>`) and the *name* (`blob://<name>`)
  are resolved by the same `--path` store you configured — a dir (possibly
  shared on NFS), an HTTP server, or S3 (`blob get`/`add` talk it directly;
  `blob serve` + the gateway expose it over plain HTTP). You can run several
  clients against one shared store, but every client is told up-front where
  it is.

So relative to the other two: `blob` drops IPFS's discovery + signed names and
ORAS's registry protocol (push/pull to a live registry), and in return is just
*address, store, serve* — no network to bootstrap, no registry to deploy, no
keys to sign with. The content is addressed exactly like in the others;
you're only opting out of how it travels. (`blob` does read and write *OCI
layouts* themselves — see the next section — just without the registry behind
them.)

## OCI, the closest cousin

One row of that table deserves its own explanation, because it is the one other
scheme that is *also content-addressed and also has a mutable name on top*:
`oras://`. So it may seem odd that `blob` doesn't use the same OCI manifests
and layouts as ORAS does. And it isn't odd at all — OCI lays out the same
content-addressing idea `blob` is built on, just with the vocabulary of the
container ecosystem:

- **OCI digest** (`sha256:<hex>`) ≈ **store blob key** (a multihash over the
  same 32 bytes). One is a human-readable "algorithm:hex" name, the other is
  the same hex bytes with a 2-byte algorithm tag (a multihash). They name the
  same content; they differ in presentation, not in what they identify.
- **OCI layout** (`oci-layout` + `index.json` + `blobs/sha256/<hex>`) ≈ a
  **name → index → a set of blobs** mapping. `index.json` is a small JSON
  document naming a manifest, which names a config and several layers —
  each one a plain content blob, addressed by its digest. A `blob oci add`
  just walks that list and stores each blob in the CAS; `blob oci get`
  reconstructs the same list of blobs.
- **OCI tag / name** ≈ `blob`'s **name index** (one name points at one
  entry, last-write-wins). The one deliberate difference is that a *real*
  OCI registry holds a mutable `index.json` (shared, mutable) while `blob`
  records one *name → one index* link per name (each name is an atomic
  single-key write, no read-modify-write of a shared document). Same idea,
  same direction (name → content → set of blobs), different scale of
  mutation.

So `blob` is not an "OCI store" — it is a *blob store that happens to speak
OCI on the wire*. That means:

- an OCI layout can be imported / exported without re-hashing anything
  (the digests line up with the multihashes),
- and an OCI layout can be stored with all the benefits of this store (CAS,
  backend choice, read-only serve) that a registry wouldn't give.

That is the whole OCI story, conceptually. No push, no tag map, no auth, no
HTTPS — but the `serve` command *does* speak the OCI distribution **read** API
(`/v2/<repo>/manifests/<ref>`, `/v2/<repo>/blobs/<sha256:...>`), with the
repository name being the very CID `oci import` printed (primary) or a stored
name (secondary). The only tag is `latest`; inner components are still
addressed by their `sha256:` digest. This is a *read-only registry endpoint*
on the same backend `serve` already exposes — stock clients (podman, skopeo,
oras, buildkit) can pull a layout in one shot, no `oci export` step needed.
That is why `oci` is a small set of sub-packages (`identity` for the
digest⇄multihash bridge, `oci` for the layout, `cid` for the store's CID
conventions) and not a full registry implementation.

## What this is not

Deliberately left out, because they only pay off in IPFS's *distributed*
setting and `blob` has no `add`/`get` equivalent for any of them:

- **DHT / Kademlia / Bitswap** — finding *where* a CID lives across a peer
  network you must join. `blob` doesn't join a network; it talks to the store
  you configured.
- **IPNS / DNSLink** — signed, globally-resolvable mutable names. `blob`'s
  name is a per-store link; no keys, signatures, or network lookup.
- **IPLD / Merkle DAG, CAR files** — chunking and linking large files into a
  graph of blocks. A `blob` blob is a single, whole unit of content.
- **Writes over HTTP** — the gateway is read-only; `add` is how data gets in.
