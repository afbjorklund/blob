package index

import (
	"bytes"
	"strings"
	"testing"

	"blob/cid"
	"blob/identity"
)

// keyFor builds the store key (a sha2-256 multihash) of data.
func keyFor(t *testing.T, data []byte) identity.Identity {
	t.Helper()
	id, err := identity.HashReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// renderOIDC is a typo-safe spelling of the OCI digest of id.
func digestOf(t *testing.T, id identity.Identity) string {
	t.Helper()
	d, err := identity.ToOCI(id)
	if err != nil {
		t.Fatal(err)
	}
	return string(d)
}

func renderCID(t *testing.T, code cid.Code, key identity.Identity) string {
	t.Helper()
	s, err := cid.Render(cid.Build(code, key))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCheck(t *testing.T) {
	abc := keyFor(t, []byte("abc"))
	def := keyFor(t, []byte("def"))
	abcDigest := digestOf(t, abc)
	abcRaw := renderCID(t, cid.CodeRaw, abc)
	abcDagPb := renderCID(t, cid.CodeDagPb, abc)

	cases := []struct {
		nm       string
		target   identity.Identity
		wantSkip bool
		wantErr  bool
	}{
		// plain names: record as-is
		{"ubuntu.sif", abc, false, false},
		{"busybox@sha256:fd7d", abc, false, false},                           // @sha256:… is metadata, not a direct handle
		{"sha1:da39a3ee5e6b4b0d3255bfef95601890afd80709", abc, false, false}, // other algorithm: resolver never converts it
		{abcDagPb, abc, false, false},                                        // dag-pb CID: not a direct handle here, may name (the ipfs-add interop handle)
		{abcDagPb, def, false, false},                                        // ...even if it names a different blob — it does not pin one
		// registry-style names: legal now (Encode flattens the '/')
		{"docker.io/library/busybox", abc, false, false},
		{"quay.io/org/img@sha256:abc", abc, false, false},
		{"50%off", abc, false, false}, // '%' is a legal name character too
		// native handles: reserved as direct addresses
		{abcDigest, abc, true, false}, // the blob's own digest → skip (redundant with the direct address)
		{abcDigest, def, false, true}, // the digest of another blob → reject
		{abcRaw, abc, true, false},    // a held-codec CID of the blob → skip
		{abcRaw, def, false, true},    // a held-codec CID of another blob → reject
		// malformed names
		{"", abc, false, true},
		{".", abc, false, true},
		{"..", abc, false, true},
		{"a\nb", abc, false, true},
		{"a\rb", abc, false, true},
		{"a\x00b", abc, false, true},
		{"a\x7fb", abc, false, true},
		{strings.Repeat("x", 300), abc, false, true}, // encoded segment over the 255 cap
	}
	for _, tc := range cases {
		got, err := Check(tc.nm, tc.target)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("Check(%q): got nil error, want one", tc.nm)
		case !tc.wantErr && err != nil:
			t.Errorf("Check(%q): unexpected error: %v", tc.nm, err)
		case got != tc.wantSkip && !tc.wantErr:
			t.Errorf("Check(%q) skip = %v, want %v", tc.nm, got, tc.wantSkip)
		}
	}
}

func TestIsOciRef(t *testing.T) {
	cases := []struct {
		nm   string
		want bool
	}{
		{"oci/linux/amd64/sha256/3f4d", true},
		{"oci/linux", true},
		{"oci", true},
		{"ocix/linux/amd64/sha256/3f4d", false},
		{"docker.io/library/busybox", false},
		{"alpine.sif", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsOciRef(tc.nm); got != tc.want {
			t.Errorf("IsOciRef(%q) = %v, want %v", tc.nm, got, tc.want)
		}
	}
}

func TestParseOciRefName(t *testing.T) {
	cases := []struct {
		nm   string
		os   string
		arch string
		algo string
		hex  string
		ok   bool
	}{
		{"oci/linux/amd64/sha256/3f4d6b32", "linux", "amd64", "sha256", "3f4d6b32", true},
		{"oci/linux/arm64/sha256/abcdef", "linux", "arm64", "sha256", "abcdef", true},
		// os is just a component: windows is legal in the shape too (the write
		// path's NormalizePlatform is the one that defaults to linux; the name
		// shape itself allows any os string).
		{"oci/windows/amd64/sha256/deadbeef", "windows", "amd64", "sha256", "deadbeef", true},
		{"oci/linux/amd64/sha512/abcdef", "linux", "amd64", "sha512", "abcdef", true},
		// missing components
		{"oci/linux/amd64/sha256", "", "", "", "", false},
		{"oci/linux/amd64", "", "", "", "", false},
		{"oci/linux", "", "", "", "", false},
		{"oci", "", "", "", "", false},
		// uppercase hex is not the canonical form (the write path lowercases)
		{"oci/linux/amd64/sha256/ABCDEF", "", "", "", "", false},
		// non-hex
		{"oci/linux/amd64/sha256/gg", "", "", "", "", false},
		{"oci/linux/amd64/sha256/3f4d:6b", "", "", "", "", false},
		// not an oci name at all
		{"busybox", "", "", "", "", false},
		{"ociX/linux/amd64/sha256/3f4d", "", "", "", "", false},
	}
	for _, tc := range cases {
		gotOS, gotArch, gotAlgo, gotHex, ok := ParseOciRefName(tc.nm)
		if ok != tc.ok {
			t.Errorf("ParseOciRefName(%q) ok = %v, want %v", tc.nm, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if gotOS != tc.os || gotArch != tc.arch || gotAlgo != tc.algo || gotHex != tc.hex {
			t.Errorf("ParseOciRefName(%q) = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
				tc.nm, gotOS, gotArch, gotAlgo, gotHex, tc.os, tc.arch, tc.algo, tc.hex)
		}
	}
}

func TestBuildOciName(t *testing.T) {
	got := BuildOciName("linux", "amd64", "sha256", "3f4d6b32")
	want := "oci/linux/amd64/sha256/3f4d6b32"
	if got != want {
		t.Errorf("BuildOciName = %q, want %q", got, want)
	}
	// round-trip through ParseOciRefName
	if o, a, al, h, ok := ParseOciRefName(want); !ok || o != "linux" || a != "amd64" || al != "sha256" || h != "3f4d6b32" {
		t.Errorf("round-trip ParseOciRefName(%q) = (%q, %q, %q, %q, %v), want (linux, amd64, sha256, 3f4d6b32, true)",
			want, o, a, al, h, ok)
	}
}

func TestNormalizeOciDigest(t *testing.T) {
	cases := []struct {
		in      string
		algo    string
		hex     string
		wantErr bool
	}{
		{"sha256:3f4d6b32e267", "sha256", "3f4d6b32e267", false},
		{"sha256:ABCDEF", "sha256", "abcdef", false},      // upper-case lowercased
		{"3f4d6b32e267", "sha256", "3f4d6b32e267", false}, // bare hex → sha256 assumed
		{"SHA256:abcdef", "sha256", "abcdef", false},      // upper-case algo lowercased
		{"sha512:abcdef", "", "", true},                   // not sha256 — rejected
		{"md5:abcdef", "", "", true},                      // not sha256 — rejected
		{"sha256:", "", "", true},                         // empty hex — rejected
		{":abcdef", "", "", true},                         // empty algo — rejected
		{"sha256:gh", "", "", true},                       // non-hex — rejected
		{"", "", "", true},                                // empty — rejected
	}
	for _, tc := range cases {
		algo, hex, err := NormalizeOciDigest(tc.in)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("NormalizeOciDigest(%q) = (%q, %q, nil), want error", tc.in, algo, hex)
		case !tc.wantErr && err != nil:
			t.Errorf("NormalizeOciDigest(%q) = (%q, %q, %v), want (%q, %q, nil)", tc.in, algo, hex, err, tc.algo, tc.hex)
		case !tc.wantErr && (algo != tc.algo || hex != tc.hex):
			t.Errorf("NormalizeOciDigest(%q) = (%q, %q), want (%q, %q)", tc.in, algo, hex, tc.algo, tc.hex)
		}
	}
}

func TestNormalizePlatform(t *testing.T) {
	cases := []struct {
		in      string
		os      string
		arch    string
		wantErr bool
	}{
		{"linux/amd64", "linux", "amd64", false},
		{"linux/arm64", "linux", "arm64", false},
		{"amd64", "linux", "amd64", false}, // bare arch → linux assumed
		{"arm64", "linux", "arm64", false},
		{"windows/amd64", "windows", "amd64", false}, // non-linux: shape allows it
		{"", "", "", true},
		{"/amd64", "", "", true}, // empty os
		{"linux/", "", "", true}, // empty arch
	}
	for _, tc := range cases {
		os, arch, err := NormalizePlatform(tc.in)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("NormalizePlatform(%q) = (%q, %q, nil), want error", tc.in, os, arch)
		case !tc.wantErr && err != nil:
			t.Errorf("NormalizePlatform(%q) = (%q, %q, %v), want (%q, %q, nil)", tc.in, os, arch, err, tc.os, tc.arch)
		case !tc.wantErr && (os != tc.os || arch != tc.arch):
			t.Errorf("NormalizePlatform(%q) = (%q, %q), want (%q, %q)", tc.in, os, arch, tc.os, tc.arch)
		}
	}
}

func TestCheckOciRefName(t *testing.T) {
	// An oci/ reference is a *name*, not a direct handle: index.Check must not
	// intercept it (digest.Parse and cid.Parse both fail on the oci/ shape)
	// and must not skip it (it never matches a blob's own multihash). This is
	// why the composite can be recorded freely for any target — the
	// "tolerate the missing upstream index" case we deliberately accept.
	abc := keyFor(t, []byte("abc"))
	name := BuildOciName("linux", "amd64", "sha256", "3f4d6b32")
	got, err := Check(name, abc)
	if err != nil {
		t.Errorf("Check(%q, abc): %v; want (false, nil)", name, err)
	}
	if got {
		t.Errorf("Check(%q, abc) skip = true; want false (record it)", name)
	}
}

func TestEncode(t *testing.T) {
	cases := []struct {
		nm   string
		want string
	}{
		// identity: names legal before escaping are unchanged (no '/', no '%')
		{"busybox", "busybox"},
		{"ubuntu.sif", "ubuntu.sif"},
		{"a@sha256:abc", "a@sha256:abc"},
		{"quay.io/org/img", "quay.io%2Forg%2Fimg"},
		{"café", "café"}, // non-ASCII UTF-8 passes through byte-wise
		// the escape table: '/' -> %2F, '%' -> %%
		{"a/b", "a%2Fb"},
		{"a//b", "a%2F%2Fb"},
		{"/a", "%2Fa"},
		{"a/", "a%2F"},
		{"docker.io/library/busybox", "docker.io%2Flibrary%2Fbusybox"},
		{"50%off", "50%%off"},
		// the '%' escape is what keeps the round-trip unambiguous: the *name*
		// "%2F" (three chars) encodes to something that decodes back to "%2F",
		// never to "/".
		{"%2F", "%%2F"},
		{"a%2Fb", "a%%2Fb"}, // the name "a%2Fb" (literal) != the name "a/b"
		{"a%/b", "a%%%2Fb"}, // mixed: the '%' and the '/' both escape
	}
	for _, tc := range cases {
		if got := Encode(tc.nm); got != tc.want {
			t.Errorf("Encode(%q) = %q, want %q", tc.nm, got, tc.want)
		}
		// strict round-trip: every name, once encoded, decodes back exactly to
		// itself. This is the invariant the backends rely on (Encode is applied
		// before storing, Decode on reading back).
		if got := Decode(Encode(tc.nm)); got != tc.nm {
			t.Errorf("Decode(Encode(%q)) = %q, want %q", tc.nm, got, tc.nm)
		}
	}
}

func TestDecode(t *testing.T) {
	cases := []struct {
		seg  string
		want string
	}{
		// identity: a segment with no escapes is the name itself
		{"busybox", "busybox"},
		{"a@sha256:abc", "a@sha256:abc"},
		{"café", "café"},
		// the escapes produced by Encode
		{"a%2Fb", "a/b"},
		{"a%2F%2Fb", "a//b"},
		{"%2Fa", "/a"},
		{"docker.io%2Flibrary%2Fbusybox", "docker.io/library/busybox"},
		{"50%%off", "50%off"},
		{"a%%2Fb", "a%2Fb"}, // the *name* "a%2Fb", not "a/b" (see Encode)
		{"a%%%2Fb", "a%/b"},
		// legacy tolerance: a '%' not followed by %% or %2F is a literal '%'
		{"a%xb", "a%xb"},
		{"a%00b", "a%00b"},
		{"a%zz", "a%zz"},
		{"%", "%"},
		{"a%2", "a%2"},
		{"a%2G", "a%2G"},
		{"a%2f", "a%2f"}, // lower-case f is not the escape (Encode emits upper-case)
	}
	for _, tc := range cases {
		if got := Decode(tc.seg); got != tc.want {
			t.Errorf("Decode(%q) = %q, want %q", tc.seg, got, tc.want)
		}
	}

	// Legacy vs strict: a store predating Encode may hold a raw "%2F" (as a
	// *name*); it decodes tolerantly to "a/b" so such a store still reads.
	// The *encoded* form "a%%2Fb" decodes to the distinct name "a%2Fb".
	// Encoding either of those names now gives a form that round-trips exactly.
	if got := Decode("a%2Fb"); got != "a/b" {
		t.Errorf("tolerant Decode(%q) = %q, want a/b", "a%2Fb", got)
	}
	if got := Decode("a%%2Fb"); got != "a%2Fb" {
		t.Errorf("tolerant Decode(%q) = %q, want %q", "a%%2Fb", got, "a%2Fb")
	}
	if got := Encode("a/b"); got != "a%2Fb" || Decode(got) != "a/b" {
		t.Errorf("round-trip a/b failed via %q", got)
	}
	if got := Encode("a%2Fb"); got != "a%%2Fb" || Decode(got) != "a%2Fb" {
		t.Errorf("round-trip of the two-char-slash-looking name failed via %q", got)
	}
}

func TestDefault(t *testing.T) {
	cases := []struct {
		arg  string
		want string
	}{
		{"x.sif", "x.sif"},
		{"dir/x.sif", "x.sif"},
		{"x.tar", "x"},
		{"x.tgz", "x"},
		{"x.tar.gz", "x"},
		{"dir/x.tar.gz", "x"},
		{"x.tarfile", "x.tarfile"}, // only the exact suffix is stripped
	}
	for _, tc := range cases {
		if got := Default(tc.arg); got != tc.want {
			t.Errorf("Default(%q) = %q, want %q", tc.arg, got, tc.want)
		}
	}
}
