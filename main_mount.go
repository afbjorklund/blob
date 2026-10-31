package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"blob/mount"
	"blob/storage"

	"bazil.org/fuse"
	"github.com/spf13/cobra"
)

// defaultMountPoint is the default location the FUSE mount is created in: a
// hidden subdirectory of the user's home (paralleling ipfs's default
// content path of $HOME, and keeping the mount out of the way of a working
// tree while the relative name/ + blob/ symlink shape keeps working no
// matter where the store actually lives). Expanded to the real home by the
// caller's os.UserHomeDir — the string form of "$HOME/.blob-mount" is not
// what a shell user expects, so the CLI never prints or uses that literal.
func defaultMountPoint() string {
	if v := os.Getenv("BLOB_MOUNT_PATH"); v != "" {
		// env-specified: still expand any `~`/`~/...` form the user
		// naturally writes, so `BLOB_MOUNT_PATH=~/blob-mnt blob mount`
		// lands where the user meant it (not under a directory called `~`).
		return expandTilde(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// A sensible fallback for the "no $HOME" edge case; the user is
		// expected to pass an explicit absolute path in that case anyway.
		home = os.TempDir()
	}
	return filepath.Join(home, ".blob-mount")
}

// expandTilde resolves a leading `~` or `~/...` to the user's home, exactly
// as a shell does (a bare `~` is the home, `~/x` is the home + /x). Other
// values are passed through unchanged, so an absolute path or a cwd-relative
// path is not re-written by us.
func expandTilde(p string) string {
	if p == "" {
		return p
	}
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func runMount(cmd *cobra.Command, args []string) error {
	backend, err := getStorageBackend(storage.ReadOnly)
	if err != nil {
		return err
	}
	defer backend.Close()

	mountPoint := mountDir
	// The kernel mounts *into* an existing directory, so create it if it is
	// not already there. Best-effort: if it already exists, MkdirAll is a
	// no-op; if the user gave a path whose parent does not exist, MkdirAll
	// builds the parent chain for them (this is the "subdirectory of the home
	// directory, similar to what the ipfs default was" shape — the user's
	// default is $HOME/.blob-mount, and they may pass ~/foo/bar and expect
	// the same convenience).
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return err
	}
	// FUSE refuses to mount a non-empty directory (it would hide its
	// contents under the mount). Check early for a friendly error message
	// instead of the kernel's EBUSY.
	if entries, _ := os.ReadDir(mountPoint); len(entries) != 0 {
		return fmt.Errorf("%s is not empty; FUSE mounts into an existing empty directory", mountPoint)
	}

	fsys := mount.New(backend)
	if dbg := os.Getenv("BLOB_MOUNT_DEBUG"); dbg != "" {
		fuse.Debug = func(msg interface{}) {
			fmt.Fprintln(os.Stderr, "FUSE:", msg)
		}
	}
	conn, err := fsys.Mount(mountPoint)
	if err != nil {
		return err
	}
	// Close the FUSE connection on all paths (including on error after the
	// mount succeeded): closing the connection releases the kernel's hold
	// on the mount, so the mount point becomes empty again.
	defer func() { _ = conn.Close() }()

	fmt.Printf("Mounting read-only FUSE view of %s at %s\n", storagePath, mountPoint)
	fmt.Printf("  %s/name/   the recorded index names (each a link to the blob's CID)\n", mountPoint)
	fmt.Printf("  %s/blob/   the content CAS, addressed by CID (listable-by-path, not enumerable)\n", mountPoint)
	fmt.Printf("Unmount on Ctrl-C or SIGTERM.\n")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := fsys.ServeContext(ctx, conn); err != nil {
		return err
	}
	return nil
}
