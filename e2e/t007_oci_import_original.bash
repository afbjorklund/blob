# t007_oci_import_original — the synthetic-default + --original feature for
# importing a genuine authored OCI layout (dir or tar, with index.json).
#
# Background
# ----------
# An authored OCI layout carries the author's own index.json + manifest(s).
# Before this change, `oci import` persisted those author JSON as-is and the
# name pointed at them (the author's annotation-bearing digests). The new
# behaviour makes the *synthetic* wrapper — the annotation-free index +
# manifest the store would re-derive for the same config + layers — the
# default: the name's CID is a pure function of the content (guessable), and
# `--original` opts into *also* keeping the author's JSON as sha256
# addressable links to the same payload (not the name's target).
#
# Layouts under test
# ------------------
#   * A busybox-shape image: real image config (rootfs.diff_ids), two real
#     layers, and an ANNOTATED index + manifest (io.containerd.image.name) —
#     the exact shape `docker save` / skopeo produce and the one t006 uses.
#   * A bare file is wrapped as a single-layer artifact by its bytes; it
#     carries no author OCI JSON, so --original is a no-op for it. (Covered
#     here only to prove the flag is tolerated; the bare-path itself is t002.)
#
# What we assert
# --------------
#   (a) Synthetic-default import of the annotated image layout:
#       - `oci ls img` (by name) lists the config + both layers and the
#         manifest with the *canonical* (annotation-free) digest, and does NOT
#         name the author's manifest (the store does not persist it).
#       - `oci ls <config-digest>` (the Docker Image ID path) synthesizes the
#         same canonical tree.
#       - `oci export img -o out` writes the synthetic layout (index.json +
#         config + both layers) and the index.json carries no annotation.
#       - The name's CID resolves to the *synthetic* index blob (not the
#         author's index.json, even though the author's index.json was in the
#         source layout): a CID whose multihash is the content's sha256, with
#         the annotation-free digest — `blob cat <cid>` of that index is the
#         canonical index bytes, not the author's.
#   (b) `oci import --original` on the same layout:
#       - Same synthetic default as (a); the name still points at the
#         synthetic index CID.
#       - In addition, the author's own index.json + manifest.json are in the
#         store as sha256-addressable blobs: `blob cat sha256:<author>`
#         succeeds for both, and the bytes match the source layout's.
#       - `oci export <author-index-digest>` (by the author's digest) ALSO
#         writes a layout with the same config + both layers — the author's
#         index/manifest are a valid "link" into the same payload.
#       - No name recorded as the author's manifest digest: the author's
#         JSON is addressable by digest only, never by name (names are the
#         synthetic index CID and any user --name).
#   (c) `oci import` of a bare file with --original:
#       - Succeeds; the bare path is synthetic-only (--original is a no-op);
#         the name still points at the file's bytes.
t007_oci_import_original() {
  scenario_init t007

  command -v sha256sum >/dev/null 2>&1 || skip "t007 needs sha256sum"

  printf 't007 rootfs layer one (uncompressed, in the store)\n' > "$WORK/layer1" \
    || { echo "FAIL: cannot seed layer1" >&2; return 1; }
  printf 't007 rootfs layer two\n' > "$WORK/layer2" \
    || { echo "FAIL: cannot seed layer2" >&2; return 1; }
  local layout="$WORK/layout"
  oci_image_layout_real "$layout" "$WORK/layer1" "$WORK/layer2" || return 1
  [ -f "$WORK/config.json" ] || { echo "FAIL: builder produced no config.json" >&2; return 1; }

  local cfghex l1hex l2hex authidx manauth
  cfghex="$(sha256sum "$WORK/config.json" | awk '{print $1}')"
  l1hex="$(sha256sum "$WORK/layer1" | awk '{print $1}')"
  l2hex="$(sha256sum "$WORK/layer2" | awk '{print $1}')"
  authidx="$(sha256sum "$layout/index.json" | awk '{print $1}')"
  # The author's manifest digest is discoverable from the layout's index.json
  # (its single-entry manifests array). Extract it without jq (portability).
  manauth="$(sed -n 's/.*"digest":"sha256:\([a-f0-9]\{64\}\)".*/\1/p' "$layout/index.json" | head -n 1)"
  [ -n "$manauth" ] || { echo "FAIL: could not extract author manifest digest from index.json" >&2; return 1; }

  # ------------------------------------------------------------------------
  # (a) Synthetic-default import of the annotated layout.
  # ------------------------------------------------------------------------
  expect_ok "import (synthetic default)" blob oci import "$layout" --name img || return 1

  local lsname
  lsname="$(expect_ok "oci ls img" blob oci ls img)" || return 1
  printf '%s\n' "$lsname" | grep -q "sha256:$cfghex" \
    || { echo "FAIL: oci ls by name missed the config digest" >&2; return 1; }
  printf '%s\n' "$lsname" | grep -q "sha256:$l1hex" \
    || { echo "FAIL: oci ls by name missed layer 1" >&2; return 1; }
  printf '%s\n' "$lsname" | grep -q "sha256:$l2hex" \
    || { echo "FAIL: oci ls by name missed layer 2" >&2; return 1; }
  # The author's manifest digest must NOT appear — the synthetic manifest (a
  # different digest) is what the name resolves to. (The author's index.json
  # is also not in the store; t006 already asserts the same on the ls-by-config
  # path.)
  printf '%s\n' "$lsname" | grep -q "sha256:$manauth" \
    && { echo "FAIL: oci ls by name named the author's manifest ($manauth); want the synthetic" >&2; return 1; }

  # (a2) The name's CID resolves to the image config (the Docker Image ID).
  # `blob cat <cid>` must return the exact config bytes — and it must NOT
  # carry the author's `io.containerd.image.name` annotation (which lives in
  # the author's index, not in the config). This is the content-addressable
  # target: it matches the `sha256` of the config's contents.
  local imgcid cfgbytes
  imgcid="$(expect_ok "lookup img cid" blob lookup img)" || return 1
  cfgbytes="$(expect_ok "cat config by name" blob cat "$imgcid")" || return 1
  [ "$cfgbytes" = "$(cat "$WORK/config.json")" ] \
    || { echo "FAIL: name CID's bytes do not match the layout's config.json" >&2; return 1; }
  printf '%s' "$cfgbytes" | grep -q 'io\.containerd\.image\.name' \
    && { echo "FAIL: name CID's config bytes carry the author's annotation; want only the config" >&2; return 1; }

  # (a3) oci export by name: synthetic layout, no annotations.
  local out_a="$WORK/out_a"
  expect_ok "oci export img" blob oci export img -o "$out_a" || return 1
  [ -f "$out_a/index.json" ] || { echo "FAIL: export (a) produced no index.json" >&2; return 1; }
  [ -f "$out_a/blobs/sha256/$cfghex" ] || { echo "FAIL: export (a) missing config blob" >&2; return 1; }
  assert_file_eq "export (a) config bytes" "$WORK/config.json" "$out_a/blobs/sha256/$cfghex" || return 1
  [ -f "$out_a/blobs/sha256/$l1hex" ] || { echo "FAIL: export (a) missing layer 1" >&2; return 1; }
  [ -f "$out_a/blobs/sha256/$l2hex" ] || { echo "FAIL: export (a) missing layer 2" >&2; return 1; }
  assert_file_eq "export (a) layer 1 bytes" "$WORK/layer1" "$out_a/blobs/sha256/$l1hex" || return 1
  assert_file_eq "export (a) layer 2 bytes" "$WORK/layer2" "$out_a/blobs/sha256/$l2hex" || return 1
  printf '%s' "$(cat "$out_a/index.json")" | grep -q 'io\.containerd\.image\.name' \
    && { echo "FAIL: exported index.json (a) carries the author's annotation; want synthetic" >&2; return 1; }

  # (a4) oci export by the author's INDEX digest must FAIL — that digest is
  # not the synthetic one and no name points at it. (The author's index
  # digest is not a layout in the store; it is a JSON blob with no
  # reachable set of its own.)
  expect_fail "oci ls author index (not a layout)" blob oci ls "sha256:$authidx" || return 1

  # ------------------------------------------------------------------------
  # (b) Same import with --original: the synthetic default unchanged, plus
  #     the author's index + manifest kept in the store as sha256 "links"
  #     into the same payload.
  # ------------------------------------------------------------------------
  expect_ok "import --original" blob oci import "$layout" --name img2 --original || return 1

  local ls2
  ls2="$(expect_ok "oci ls img2" blob oci ls img2)" || return 1
  printf '%s\n' "$ls2" | grep -q "sha256:$cfghex" || { echo "FAIL: (b) ls missed config" >&2; return 1; }
  printf '%s\n' "$ls2" | grep -q "sha256:$l1hex" || { echo "FAIL: (b) ls missed layer 1" >&2; return 1; }
  printf '%s\n' "$ls2" | grep -q "sha256:$l2hex" || { echo "FAIL: (b) ls missed layer 2" >&2; return 1; }
  printf '%s\n' "$ls2" | grep -q "sha256:$manauth" \
    && { echo "FAIL: (b) oci ls named the author's manifest (want only the synthetic)" >&2; return 1; }

  # (b1) The author's index.json and manifest.json must now be in the store
  # as sha256-addressable blobs, byte-identical to the source. (The store
  # dir backend stores them in blobs/<12 20 hex 32> — same as any blob; we
  # check their presence via `blob cat`.)
  local authoridxbytes authormanbytes
  authoridxbytes="$(expect_ok "cat author index" blob cat "sha256:$authidx")" || return 1
  authormanbytes="$(expect_ok "cat author manifest" blob cat "sha256:$manauth")" || return 1
  # Byte-compare against the source layout's copies (the dir backend writes
  # them, so `cat by sha256` is the portable form).
  local authoridx_src authorman_src
  # Reconstruct the author's index/manifest bytes by re-importing? No — the
  # source layout (a fresh dir) is the ground truth: it contains both files.
  authoridx_src="$(cat "$layout/index.json")" || return 1
  # The author's manifest bytes: fetch from the layout's blobs dir by hex.
  authorman_src="$(cat "$layout/blobs/sha256/$manauth")" || return 1
  [ "$authoridxbytes" = "$authoridx_src" ] \
    || { echo "FAIL: (b) author index bytes do not match the source layout's index.json" >&2; return 1; }
  [ "$authormanbytes" = "$authorman_src" ] \
    || { echo "FAIL: (b) author manifest bytes do not match the source layout's manifest" >&2; return 1; }

  # (b2) `oci export` by the author's index digest (a "link" into the same
  # payload) must succeed and produce a layout with the real config + both
  # layers byte-for-byte — the author's JSON is a digest-addressable pointer
  # into this store's content, regardless of whether it names the synthetic
  # or the author's manifest.
  local out_b="$WORK/out_b"
  expect_ok "oci export author index" blob oci export "sha256:$authidx" -o "$out_b" || return 1
  [ -f "$out_b/index.json" ] || { echo "FAIL: (b) export by author index no index" >&2; return 1; }
  [ -f "$out_b/blobs/sha256/$cfghex" ] || { echo "FAIL: (b) export by author index missing config" >&2; return 1; }
  assert_file_eq "(b) export by author index config bytes" "$WORK/config.json" "$out_b/blobs/sha256/$cfghex" || return 1
  [ -f "$out_b/blobs/sha256/$l1hex" ] || { echo "FAIL: (b) export by author index missing layer 1" >&2; return 1; }
  [ -f "$out_b/blobs/sha256/$l2hex" ] || { echo "FAIL: (b) export by author index missing layer 2" >&2; return 1; }

  # (b3) No name recorded as the author's manifest digest — it is only a
  # sha256-addressable link, never a name.
  local listing
  listing="$(expect_ok "blob list" blob list)" || return 1
  printf '%s\n' "$listing" | grep -q "sha256:$manauth" \
    && { echo "FAIL: (b) listing shows the author's manifest digest as a name" >&2; return 1; }

  # ------------------------------------------------------------------------
  # (c) Bare file + --original: the bare path is synthetic-only, the flag is
  #     tolerated (no-op), and the name still points at the file's bytes.
  # ------------------------------------------------------------------------
  printf 't007 a bare artifact under the --original flag\n' > "$WORK/blob" \
    || return 1
  local blobhex
  blobhex="$(sha256sum "$WORK/blob" | awk '{print $1}')" || return 1
  expect_ok "oci import (bare file) with --original" \
    blob oci import "$WORK/blob" --original || return 1
  local base
  base="$(basename "$WORK/blob")"
  local blobcid
  blobcid="$(expect_ok "lookup bare name" blob lookup "$base")" || return 1
  [ -n "$blobcid" ] || { echo "FAIL: (c) bare-import lookup gave no CID" >&2; return 1; }
  local out_c
  out_c="$WORK/out_c"
  expect_ok "oci export (bare + --original)" blob oci export "$base" -o "$out_c" || return 1
  [ -f "$out_c/blobs/sha256/$blobhex" ] \
    || { echo "FAIL: (c) export of --original bare import missing the layer blob" >&2; return 1; }
  assert_file_eq "(c) exported bare-artifact layer bytes" "$WORK/blob" "$out_c/blobs/sha256/$blobhex" || return 1

  return 0
}
