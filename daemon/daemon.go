// Package daemon is the read/serving layer of the store: it takes a
// storage.StorageBackend and exposes the store in the two public formats this
// store speaks — CID (the store's own language) and OCI (the format `oci
// import` accepts) — over HTTP.
//
// It sits above `storage` the way `main` did before being split out: `storage`
// is the pure, format-agnostic CAS (multihash keys, no multiformats), and the
// daemon is where that CAS is bridged to content identifiers (cid + identity)
// and to OCI (via the oci package) — the layer that `main` used to keep inline.
// Because it is an ordinary package (not package main) it is tested like the
// other subsystems: the same `StorageBackend` interface the real backends
// implement can be stubbed, so the handlers are exercised against an in-memory
// store (daemon_test.go) and, opt-in, against a real dir backend (
// daemon_smoke_test.go).
//
// The surface is read-only by construction: only Get / Peek / Lookup are used
// on the backend, so a `storage.ReadOnly` handle fits it by design (no
// upload/add endpoint). `main` (CLI) opens a backend and hands it to
// `New`; the daemon serves it.
package daemon

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"blob/storage"
)

// Server serves a single read-only store backend over HTTP: the CID gateway
// (/blob/<CID>, /name/<name>) and the OCI distribution read surface (/v2/...).
// One shared backend is held for the server's life and handed to every request
// goroutine; the backends are safe for concurrent use (sql pools its
// connections, dir opens a fresh handle per op, http/s3 serialise internally).
type Server struct {
	backend storage.StorageBackend
}

// New builds a Server over backend. backend should be opened storage.ReadOnly
// (the daemon only reads); it is shared across all the handlers this Server
// wires, so the caller keeps and Closes it.
func New(backend storage.StorageBackend) *Server {
	return &Server{backend: backend}
}

// Handler returns the multiplexer the Server serves: the CID gateway
// (/blob/<CID> and /name/<name>) plus the OCI distribution read surface
// (/v2/...). The three prefixes are mutually disambiguated by the mux, so the
// namespaces never shadow each other.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// /blob/<CID> : the store's own surface — a stable content address the
	// client can use (blob://<CID> -> http://<host>/blob/<CID>) without knowing
	// the backend.
	mux.HandleFunc("/blob/", func(w http.ResponseWriter, r *http.Request) {
		s.serveBlob(w, r, r.URL.Path[len("/blob/"):])
	})
	// /name/<name> : the name-index surface — resolves <name> (last-add-wins)
	// and 302-redirects to the canonical /blob/<CID>.
	mux.HandleFunc("/name/", func(w http.ResponseWriter, r *http.Request) {
		s.serveName(w, r, r.URL.Path[len("/name/"):])
	})
	// /v2/... : the OCI distribution read surface — a layout already in the store
	// can be pulled by stock OCI clients (podman, oras, skopeo, buildkit) with no
	// `oci export` step. It is a distinct prefix from /blob/ and /name/, so the
	// mux disambiguates by prefix.
	mux.Handle("/v2/", s.ociV2Handler())
	return mux
}

// ListenAndServe runs the Server on addr until ctx is cancelled (SIGINT /
// SIGTERM) or the listener fails. On cancellation it initiates a graceful
// shutdown (up to 5s) so in-flight blobs can drain. It blocks for the life of
// the server, which is what a `serve` command wants.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler()}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	fmt.Printf("Serving on http://%s\n", addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}
