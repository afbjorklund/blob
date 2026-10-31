# README improvement plan

Status: **draft**. What's already done in this + last session:

1. Committed at `7712868` — the OCI "names" prose moved out of the
   README (2-line pointer left where the ~90-line block was).
2. Working tree (uncommitted) — the HTTP/S3 backend intros; the one
   cross-backend insight that was unique to those intros ("the http/s3
   name index is the same link as the dir symlink and the sql
   foreign-key") was added to `DESIGN.md` §Name-index.
   EDIT: Commited at `7969cff`

This document is the plan for what's left. It should be picked up cold
by a future session or by the maintainer, not executed in the session
it was written in.

## Principle

Every doc must be readable top-to-bottom by its own audience without
*having* to follow a cross-reference. Cross-references are optional
"want to know more" doors, not load-bearing for understanding.

`blob` has four human audiences plus one non-human one:

| doc                 | audience                              | answers                                |
| ------------------- | ------------------------------------- | -------------------------------------- |
| `README.md`         | the user in a terminal                | what can I do, how do I do it          |
| `CONCEPTS.md`       | developer: the *why* of the ideas     | CID / CAS / name index / gateway / OCI |
| `DESIGN.md`         | developer: the *why* of the choices   | each decision + what we skip           |
| `IMPLEMENTATION.md` | developer: the *how* of the code      | packages, layers, concurrency, testing |
| `BACKEND.md`        | the storage admin                     | point `blob` at a real server; setup   |
| `AGENTS.md`         | the agent that maintains all of these | the map + the build / test gates       |

Applied to the README specifically: keep the *overall* design and
features (content addressing, dedup, name index, OCI interop, gateway,
`/v2/` pull) at the "what is it and what can I do with it" level. Do
not go into "how it works". A user should be able to read the README
top-to-bottom without having to flip to CONCEPTS / DESIGN / BACKEND /
IMPLEMENTATION, and only flip if they really want more.

## What's left on the table

Grouped **A** = Features section, **B** = Usage (per-command) prose,
**C** = the two long sections at the end. Each item lists:

- what's there now
- what stays in the README
- what moves, and to where (verified vs. needs-add)
- the target shape

### A. Features section

**A1. Name-index bullet (the "Registry-style names" sub-bullet).**

- Now: per-backend shape (`symlink` in dir / FK table in sql /
  `index/<name>` in http-s3); the `%2F`→`%%` encoded-segment rule;
  "`oci import` records the basename plus `--name`".
- Stays: names resolve to a blob; last-add-wins; names may be
  registry-style (`docker.io/…`).
- Moves: per-backend shape → `DESIGN.md` §Name index (verified);
  `%2F`/`%%` → `IMPLEMENTATION.md` §`index.Encode` (verified).
- Target: 2–3 lines + a pointer to `CONCEPTS.md` §Name index.

**A2. Read-only HTTP gateway bullet.**

- Now: full header inventory, 302-rationale, registry-pull mention.
- Stays: "serves blobs over plain HTTP; `/name/<name>` 302-redirects
  to `/blob/<CID>`; see Usage §HTTP gateway for `curl`".
- Moves: header shapes → `CONCEPTS.md` §HTTP gateway (verified);
  302-not-301 rationale → `DESIGN.md` §Deliberately small (verified).
- Target: 2–3 lines.

**A3. OCI distribution registry (pull) bullet.**

- Now: full pull list, tag policy, `X-`-header, auth/HTTPS note.
- Stays: "also speaks the OCI distribution read API, so podman / oras
  / skopeo can pull an imported layout without `oci export`;
  read-only; pull by CID or name; tag is `latest`".
- Moves: "no push" scope → `DESIGN.md` §Deliberately small (verified);
  `/v2/` paths + `X-Blob-Cid` → `IMPLEMENTATION.md` §daemon (verified);
  pull-idea overview → `CONCEPTS.md` tail "OCI, the closest cousin"
  (verified).
- Target: 2–3 lines.

**A4. Streaming I/O bullet.**

- Now: "the sqlite backend reads via `db.OpenBlob` in a read-only
  handle instead of loading the whole row".
- Stays: "streamed without materializing the whole blob in memory".
- Moves: sqlite `OpenBlob` detail → `BACKEND.md` §sql (verified:
  "Reads stream via the SQLite blob API (`OpenBlob`) …").
- Target: 1 line.

**A5. Pluggable-backends sub-list.**

- Now: per-backend layout hints (3-level fanout, `<base>/<hex>`,
  `<bucket>/<hex>`).
- Stays: the name of each backend + which flag to point at what.
- Moves: layout → `BACKEND.md` §per-backend (verified); fanout →
  `IMPLEMENTATION.md` §Storage internals (verified).
- Target: 1 line per backend.

### B. Usage section

**B1. `blob sum` Compute / Verify paragraphs.**

- Now: two full paragraphs contrasting with `sha256sum` ("improperly
  formatted lines" argument, why `sha256sum` must assume SHA-256, the
  `-i` common case).
- Stays: usage shape already in the code block; one line: "prints CIDs
  in the `sha256sum` shape; `-c <list>` verifies;
  `-i / --ignore-missing` skips unreadable lines".
- Moves: the "CID is self-describing, `sha256sum` is not" argument →
  `CONCEPTS.md` §Content addressing (CID) — a "the address is
  self-describing" sentence already exists there; **add one line**
  contrasting with `sha256sum` (needs-add).
- Target: 5–6 lines replacing the current ~30.

**B2. `blob get --verify` option.**

- Now: "downloaded to a temp file and renamed into place only after the
  check passes, so a failed verification leaves no partial or
  mismatched file at the output path".
- Stays: "re-hash the retrieved data; fail if it doesn't match (leaves
  no partial file at the output path)".
- Moves: the temp-file-rename mechanic — **needs a destination
  decision**: either drop the mechanic and keep the outcome in the
  README, or add one line to `DESIGN.md` under the OCI / integrity
  section. Currently only in the README.
- Target: 1–2 lines.

**B3. `blob lookup` intro ("peek first byte / no re-hash").**

- Now: "peeks the blob's first byte (the same content-based codec rule
  as `add`/`sum`) — no re-hash, bounded by one byte regardless of blob
  size".
- Stays: "resolves an index name to the stored blob's CID".
- Moves: the peek mechanic → `DESIGN.md` §Name index (verified:
  "Name→CID is therefore a peek, not a re-hash … a 4 GB SIF costs the
  same as a 12 B config").
- Target: drop the mechanic, keep the one-word usage.

**B4. `blob list -l` ("resolved once, not per name").**

- Now: implementation dedup note.
- Stays: the example output (already in the code block).
- Moves: the "resolved once" clause → `DESIGN.md` §Name index (covered
  by the same peek note).
- Target: drop the clause.

**B5. `blob cid` ("A pure decode: a CID argument never opens a
backend").**

- Now: implementation note.
- Stays: "decodes a CID and prints its parts; a name argument resolves
  the name first, then prints".
- Moves: the "never opens a backend" clause → `DESIGN.md` or drop
  (borderline; the *why* that the command is a pure decode is a design
  choice).
- Target: 1 line.

**B6. `oci import / ls / export` prose (the "Bare files" + `oci ls`
vs. `oci export` policy bullets).**

- Now: "a bare file's OCI shape is a *pure function of the file's
  bytes*"; "`oci ls` reports what the store holds"; "`oci export`
  produces interop output and never writes to the store".
- Stays: the usage code block (kept) + one line: "`oci ls` lists what
  the store holds; `oci export` writes the layout out — a bare file
  exports even if it was only `blob add`-ed".
- Moves: "pure function of the bytes" → `DESIGN.md` §OCI import/export
  (verified: "The artifact is a pure function of the layer"); `oci ls`
  vs. `oci export` policy difference → `DESIGN.md` §OCI import/export
  (verified: "`oci export` and the `/v2/` registry surface serve the
  scaffold from memory … `oci ls` keeps the opposite policy").
- Target: 1–2 lines replacing the current 15-line block.

**B7. `--user` / `--pass` repetition in the HTTP and S3 example
intros.**

- Now: both example intros say "Credentials via `--user`/`--pass` (or
  `BLOB_USER`/`BLOB_PASS`)".
- Already listed once in the global Options block under `add`.
- Action: drop the repetition in the two example intros; keep the
  mention in the Options block.
- Target: −4 lines total.

### C. The two long sections at the end

**C1. The `### HTTP gateway` section.**

- Stays: `blob serve [--listen ...]`, the two-path summary, the
  `curl` examples.
- Moves: per-header "why" → `CONCEPTS.md` §HTTP gateway (verified);
  302-not-301 rationale → `DESIGN.md` §Deliberately small (verified);
  "streamed, never buffered whole" → `CONCEPTS.md` §HTTP gateway
  (verified).
- Target: 15–20 lines (currently ~50).

**C2. The `### OCI distribution pull (/v2/)` section.**

- Stays: the `podman pull docker://…` example; "read-only; tag is
  `latest`; pull by CID or name; no auth, no HTTPS".
- Moves:
  - `/v2/…` path table → `IMPLEMENTATION.md` §daemon (verified).
  - `X-Blob-Cid` header rationale → `IMPLEMENTATION.md` §daemon
    (verified: "the store's CID is discoverable via the `X-Blob-Cid`
    header … needed because the spec's `Docker-Content-Digest` is
    `sha256:<hex>` and carries no codec").
  - "bare file is pullable by name too, reconstructed on demand" →
    `DESIGN.md` §OCI import/export (verified: "The artifact is a pure
    function of the layer … serve the scaffold from memory, constructed
    on demand, and never persist it").
  - "no tag map" → `CONCEPTS.md` tail "OCI, the closest cousin"
    (verified).
- Target: 15–20 lines (currently ~48).

## What stays in the README (non-negotiables)

- The command-by-command usage: `sum`, `add`, `get`, `cat`, `list`,
  `lookup`, `cid`, `oci import / ls / export`, `serve`, `/v2/` pull.
  The user needs these in one place.
- The example transcripts (dir / sql / http / s3 round-trips; aliasing
  the same blob under two names).
- The flag + env-var reference (the Options block under `add`).
- The "storage backend vs. `blob serve`" distinction — it is
  user-relevant and easy to confuse.
- The short pointers to `CONCEPTS.md` / `DESIGN.md` / `BACKEND.md` /
  `IMPLEMENTATION.md` — those are the "want to know more" doors, not
  the content itself.

## Destination inventory

| fact to move out of the README                                      | destination file · section                                       | status                                       |
| ------------------------------------------------------------------- | ---------------------------------------------------------------- | -------------------------------------------- |
| per-backend index shape (dir symlink / sql FK / http and s3 object) | `DESIGN.md` · §Name index                                        | committed (dir+sql); http/s3 in working tree |
| `%2F` / `%%` encoded-segment rule                                   | `IMPLEMENTATION.md` · §`index.Encode`                            | verified                                     |
| gateway response headers (Content-Type, Content-Digest, …)          | `CONCEPTS.md` · §HTTP gateway                                    | verified                                     |
| 302-not-301 rationale                                               | `DESIGN.md` · §Deliberately small                                | verified                                     |
| `/v2/` path table + `X-Blob-Cid` header                             | `IMPLEMENTATION.md` · §daemon                                    | verified                                     |
| bare-file 'reconstructed on demand' + pure-function-of-bytes        | `DESIGN.md` · §OCI import / export                               | verified                                     |
| `oci ls` reports vs. `oci export` writes                            | `DESIGN.md` · §OCI import / export                               | verified                                     |
| sqlite `OpenBlob` streaming                                         | `BACKEND.md` · §sql                                              | verified                                     |
| dir fanout (3 levels)                                               | `BACKEND.md` · §dir and `IMPLEMENTATION.md` · §Storage internals | verified                                     |
| SHA-3 / Git-OID rehash, multiarch liberty, foreign-digest-as-name   | `DESIGN.md` · §Foreign digests as names                          | added in HEAD                                |
| 'What names what' (CID identity, basename default, `--name`)        | `CONCEPTS.md` · §What names what                                 | added in HEAD                                |
| **CID is self-describing (vs `sha256sum` hex)**                     | `CONCEPTS.md` · §Content addressing (CID)                        | **needs-add** (~1 line)                      |
| **`get --verify` no-partial-file mechanic**                         | `DESIGN.md` · (new, small) or drop                               | **needs decision**                           |

## Read-through test (after each batch)

Read the README top-to-bottom as a user who just `go install`-ed the
thing:

- [ ] Does every Features bullet feel like "what I can do" and not
  "how it's implemented"?
- [ ] For every "see … for the rationale / setup" pointer, could I read
  the README past it and still understand what I'd be missing? (If not,
  I've moved too much.)
- [ ] Are there sentences in the README a non-developer, non-admin user
  does not need? (Those are candidates to move.)
- [ ] Does the file feel shorter and easier to skim than before the
  batch?
- [ ] Is `markdownlint-cli2 *.md` green?

## Batch order (suggested)

**Batch 1 — pure relocation, no additions needed:**

A1 · A2 · A3 · A4 · A5 · B3 · B4 · B5 · B7 · C1 · C2

**Batch 2 — one small addition at a destination first, then the move:**

- B1 — add the `sha256sum` contrast to `CONCEPTS.md` §Content
  addressing (one line), then condense the README `sum` paragraphs.
- B2 — decide where the `--verify` mechanic goes (add to `DESIGN.md`
  or drop the mechanic), then trim the option line.
- B6 — verify `DESIGN.md` §OCI import/export covers
  "oci ls reports / oci export writes", then condense the two policy
  bullets.

**After each batch:**

1. `markdownlint-cli2 *.md` — clean
2. Read-through test — all boxes ticked
3. `git add` / `git commit` the batch
4. (Optional) if any pointer set shifted, update the table in
   `AGENTS.md`.

## What NOT to do

- **Don't delete — move.** Every fact leaving the README must land
  somewhere with the audience that owns it. Repetition across docs is
  fine as long as each doc serves its own audience.
- **Don't leave the README with only pointers.** The "present the
  overall design and ideas, just not the details" line means the user
  should still understand identity / dedup / name-index / OCI-interop /
  gateway from the README alone.
- **Don't touch `AGENTS.md`'s table unless a pointer set actually
  shifted.** It is the agent's map, not the user's.
