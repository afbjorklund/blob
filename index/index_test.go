package index

import (
	"bytes"
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
		// native handles: reserved as direct addresses
		{abcDigest, abc, true, false}, // the blob's own digest → skip (redundant with the direct address)
		{abcDigest, def, false, true}, // the digest of another blob → reject
		{abcRaw, abc, true, false},    // a held-codec CID of the blob → skip
		{abcRaw, def, false, true},    // a held-codec CID of another blob → reject
		// malformed names
		{"", abc, false, true},
		{".", abc, false, true},
		{"..", abc, false, true},
		{"a/b", abc, false, true},
		{"a\nb", abc, false, true},
		{"a\rb", abc, false, true},
		{"a\x00b", abc, false, true},
		{"a\x7fb", abc, false, true},
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
