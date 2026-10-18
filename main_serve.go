package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"blob/daemon"
	"blob/storage"

	"github.com/spf13/cobra"
)

// runServe opens the store read-only once for the server's life (a read-only
// backend has nothing to write, so it is the natural fit for a shared handle,
// and the sql backend is pool-backed — hence safe to hand to every request
// goroutine — while the dir backend needs no locking at all) and hands it to a
// daemon.Server, which serves the CID gateway (/blob/<CID>, /name/<name>) and
// the OCI distribution read surface (/v2/...). The daemon package is the
// testable serving layer (see daemon/); this command is the thin CLI adapter:
// it opens the backend from the storage flags, runs the server until a signal
// (SIGINT / SIGTERM), and triggers a graceful shutdown on the way out.
func runServe(cmd *cobra.Command, args []string) error {
	backend, err := getStorageBackend(storage.ReadOnly)
	if err != nil {
		return err
	}
	defer backend.Close()

	srv := daemon.New(backend)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return srv.ListenAndServe(ctx, listenAddr)
}
