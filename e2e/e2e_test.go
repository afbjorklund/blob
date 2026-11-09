// Package e2e is the opt-in CLI test for the blob binary. run.sh and the
// tNNN_*.bash scenarios hold the assertions; this file is only the adapter that
// makes the suite reachable from `go test`. It does not build the binary (a
// build is a prerequisite — `go build` or an explicit BLOB_BIN); it points
// scratch at a temp dir and execs the shell suite.
package e2e

import (
	"os"
	"os/exec"
	"testing"
)

// TestE2E is the opt-in (BLOB_E2E=1) CLI test. It is a thin adapter over the
// shell suite: it does NOT build a binary (the user runs `go build` or sets
// BLOB_BIN first), points scratch at a temp dir, execs run.sh from the suite
// dir, and maps the exit code to the testing framework (77 = everything
// skipped). BLOB_BIN is left to the shell-side resolution in lib.sh (explicit
// var, then <repo>/blob, then PATH); a missing binary is a setup failure, not
// a skip.
func TestE2E(t *testing.T) {
	if os.Getenv("BLOB_E2E") == "" {
		t.Skip("set BLOB_E2E=1 to run the CLI e2e test")
	}

	tmp := t.TempDir()
	// run.sh sits next to this test file; go test runs in the package dir.
	cmd := exec.Command("bash", "run.sh")
	cmd.Env = append(os.Environ(), "BLOB_TMP="+tmp)
	out, err := cmd.CombinedOutput()
	t.Logf("e2e:\n%s", out)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 77 {
			t.Skip("all e2e scenarios skipped")
		}
		t.Fatalf("e2e failed: %v", err)
	}
}
