package storage

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// indexPrefix is the on-wire path segment under which name index entries live
// (<base>/index/<name>), mirroring the dir backend's index/ subdirectory.
const indexPrefix = "index"

// Auth is the pair of Basic-auth credentials an http backend sends to the
// server. It is a plain value the CLI resolves from wherever the secret lives
// (--user/--pass flags, BLOB_USER/BLOB_PASS, the user's ~/.netrc, or
// user:pass@ in the --path URL) and hands over; the storage layer only applies
// it and never decides where it came from. Because this store speaks plain HTTP
// by default, the password is often a shared, revocable token rather than a
// real account password.
type Auth struct {
	User     string
	Password string
}

// hasCredentials reports whether the pair would produce a Basic header (a user
// name is required; a bare password has no addressable identity to bind it to).
func (a Auth) hasCredentials() bool { return a.User != "" && a.Password != "" }

// HTTPStorage proxies a multihash-keyed CAS to a remote server over plain
// GET / PUT (and HEAD for an existence check). It is a *storage* backend with
// its own on-wire contract, independent of the read-only CID gateway (`serve`):
// the digest is keyed by the hex of the multihash bytes (not the CID, which is
// a client-side derivation), and /index holds the name→hash link (the same
// role as the dir symlink / sql foreign key; a conceptual alignment, realized
// as file content here rather than a symlink, and with no fanout):
//
//	blob   <base>/<hex>         PUT body=blob     GET/HEAD
//	index  <base>/index/<name>  PUT body=<hex>    GET
//
// A single shared *http.Client (and its transport) serves all operations, so
// an HTTPStorage is safe for concurrent use the way the pooled sql backend is
// — important because a shared read-only caller can hand the same backend to
// many goroutines. Close drops the transport's idle keep-alive connections,
// the analogue of the sql backend closing its pool.
type HTTPStorage struct {
	baseURL   *url.URL
	client    *http.Client
	transport *http.Transport
	readOnly  bool
	closed    bool
	// auth carries the resolved Basic-auth credentials (user + token/password)
	// to send with every request; empty when the target needs none.
	auth Auth
}

func NewHTTPStorage(base string, auth Auth, mode Mode) (*HTTPStorage, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("invalid storage URL %q: %v", base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid storage URL %q: need a scheme and host (e.g. http://localhost:8080)", base)
	}

	transport := &http.Transport{
		// The same backend is shared across a gateway's request goroutines,
		// so allow a comfortable number of idle connections to the server.
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}

	// Credentials are applied per request (see do). The URL's own userinfo
	// (if the base was written as http://user:pass@host/...) is stripped here
	// so the host we store and log is clean — the secret rides on auth, not in
	// the path. The CLI is free to have taken that userinfo into auth for us.
	if u.User != nil {
		u.User = nil
	}

	st := &HTTPStorage{
		baseURL:   u,
		client:    &http.Client{Transport: transport},
		transport: transport,
		readOnly:  mode != ReadWrite,
		auth:      auth,
	}
	return st, nil
}

// blobURL is the on-wire path of a blob, keying by the hex of the multihash
// bytes (the digest is opaque to this package; we only render it for the URL).
func (s *HTTPStorage) blobURL(hash Digest) string {
	return s.baseURL.JoinPath(hex.EncodeToString(hash)).String()
}

// nameURL is the on-wire path of a name index entry.
func (s *HTTPStorage) nameURL(name string) string {
	return s.baseURL.JoinPath(indexPrefix, name).String()
}

// ok reports whether a response status is 2xx.
func ok(status int) bool { return status >= 200 && status <= 299 }

// httpError carries the status of a non-2xx response so callers can distinguish
// a definitive 404 (→ "not found") from a server error or other failure. The
// message is deliberately terse for the *expected* outcomes (401, 404) — the
// ones a well-behaved caller should not treat as a crash — and falls back to
// the one-line server reason for everything else, so an nginx/HTML body is
// never dumped wholesale onto the terminal.
type httpError struct {
	status int
	method string
	key    string
	body   string
}

func (e *httpError) Error() string {
	switch e.status {
	case http.StatusNotFound:
		return "blob not found"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication required"
	default:
		reason := http.StatusText(e.status)
		if b := plainFirstLine(e.body); b != "" {
			reason += ": " + b
		}
		return fmt.Sprintf("%s %s -> HTTP %d %s", e.method, e.key, e.status, reason)
	}
}

// plainFirstLine returns the first non-empty, trimmed line of a response body
// (capped in length) for use in an error message — but only when that line does
// not look like markup or structured data (HTML/X, JSON). A real server may send
// a short plain-text reason ("quota exceeded", "rate limit hit") and we surface
// that; the common case of an HTML error page is not worth fragmenting, so we
// fall back to the status text alone. It returns "" when there is no usable line.
func plainFirstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.ContainsAny(line, "<>{}\"") {
			return "" // markup/JSON fragment — keep it out of the human message
		}
		if len(line) > 200 {
			line = line[:200] + "…"
		}
		return line
	}
	return ""
}

// do runs the request and returns the response, translating non-2xx statuses
// into a *httpError. Any resolved Basic-auth credentials are attached here, so
// every operation (Put, Get, Has, Index, Lookup, Peek) is authenticated
// uniformly. The caller owns and closes resp.Body on the success path; on the
// error path the body is already read and closed here.
func (s *HTTPStorage) do(r *http.Request, method, key string) (*http.Response, error) {
	if s.auth.hasCredentials() {
		r.SetBasicAuth(s.auth.User, s.auth.Password)
	}
	resp, err := s.client.Do(r)
	if err != nil {
		return nil, err
	}
	if !ok(resp.StatusCode) {
		body, _ := io.ReadAll(resp.Body)
		closeBody(resp)
		return nil, &httpError{status: resp.StatusCode, method: method, key: key, body: string(body)}
	}
	return resp, nil
}

// notFound reports whether err is a 404 from the server.
func notFound(err error) bool {
	var he *httpError
	return errors.As(err, &he) && he.status == http.StatusNotFound
}

// closeBody closes resp.Body, discarding the close error (the body has already
// been fully read, so a close error is not actionable); the explicit blank
// assignment keeps errcheck quiet.
func closeBody(resp *http.Response) {
	_ = resp.Body.Close()
}

func (s *HTTPStorage) Put(hash Digest, size int64, r io.Reader) error {
	if s.readOnly {
		return ErrReadOnly
	}

	req, err := http.NewRequest(http.MethodPut, s.blobURL(hash), r)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}

	resp, err := s.do(req, http.MethodPut, hex.EncodeToString(hash))
	if err != nil {
		return err
	}
	closeBody(resp)
	return nil
}

func (s *HTTPStorage) Get(hash Digest) (io.ReadCloser, int64, error) {
	req, err := http.NewRequest(http.MethodGet, s.blobURL(hash), nil)
	if err != nil {
		return nil, 0, err
	}

	resp, err := s.do(req, http.MethodGet, hex.EncodeToString(hash))
	if err != nil {
		if notFound(err) {
			return nil, 0, errors.New("blob not found")
		}
		return nil, 0, err
	}

	// resp.Body is the io.ReadCloser the caller streams from; closing it
	// releases the connection back to the transport pool.
	return resp.Body, resp.ContentLength, nil
}

func (s *HTTPStorage) Has(hash Digest) (bool, error) {
	req, err := http.NewRequest(http.MethodHead, s.blobURL(hash), nil)
	if err != nil {
		return false, err
	}

	resp, err := s.do(req, http.MethodHead, hex.EncodeToString(hash))
	if err != nil {
		if notFound(err) {
			return false, nil
		}
		return false, err
	}
	closeBody(resp)
	return true, nil
}

func (s *HTTPStorage) Index(name string, hash Digest) error {
	if s.readOnly {
		return ErrReadOnly
	}

	body := hex.EncodeToString(hash)
	req, err := http.NewRequest(http.MethodPut, s.nameURL(name), strings.NewReader(body))
	if err != nil {
		return err
	}

	resp, err := s.do(req, http.MethodPut, name)
	if err != nil {
		return err
	}
	closeBody(resp)
	return nil
}

func (s *HTTPStorage) Lookup(name string) (Digest, error) {
	req, err := http.NewRequest(http.MethodGet, s.nameURL(name), nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.do(req, http.MethodGet, name)
	if err != nil {
		if notFound(err) {
			return nil, errors.New("name not found")
		}
		return nil, err
	}
	defer resp.Body.Close()

	// The name entry is the hash itself (the link, not the blob), so it is
	// small and safe to read whole.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	digest, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("name %q is not a valid hash: %q", name, string(raw))
	}
	return digest, nil
}

// Peek returns the first n bytes of a blob with a Range request (bytes=0..n-1),
// so the server can stream only the head (206, or a 200 with the full body if
// it ignores Range). Either way at most n bytes are returned; fewer if the blob
// is shorter or empty. This bounds the transfer regardless of blob size.
func (s *HTTPStorage) Peek(hash Digest, n int) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, s.blobURL(hash), nil)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
	}

	resp, err := s.do(req, http.MethodGet, hex.EncodeToString(hash))
	if err != nil {
		if notFound(err) {
			return nil, errors.New("blob not found")
		}
		return nil, err
	}
	defer resp.Body.Close()

	buf := make([]byte, n)
	got, err := io.ReadFull(resp.Body, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:got], nil
}

func (s *HTTPStorage) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	// Drop the idle keep-alive connections opened for this server. In-flight
	// transfers are owned by their request/response and are not affected —
	// the HTTP analogue of the sql backend closing its pool.
	s.transport.CloseIdleConnections()
	return nil
}
