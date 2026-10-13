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
}

func NewHTTPStorage(base string, mode Mode) (*HTTPStorage, error) {
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

	// No client-wide Timeout: a large blob must be allowed to stream for as
	// long as it needs (a per-operation context would be the right knob, but
	// the StorageBackend interface does not carry one).
	return &HTTPStorage{
		baseURL:   u,
		client:    &http.Client{Transport: transport},
		transport: transport,
		readOnly:  mode != ReadWrite,
	}, nil
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
// a definitive 404 (→ "not found") from a server error or other failure.
type httpError struct {
	status int
	method string
	key    string
	body   string
}

func (e *httpError) Error() string {
	msg := fmt.Sprintf("HTTP %d", e.status)
	if e.body != "" {
		msg += ": " + strings.TrimSpace(e.body)
	}
	return e.method + " " + e.key + " -> " + msg
}

// do runs the request and returns the response, translating non-2xx statuses
// into a *httpError. The caller owns and closes resp.Body on the success
// path; on the error path the body is already read and closed here.
func (s *HTTPStorage) do(r *http.Request, method, key string) (*http.Response, error) {
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
