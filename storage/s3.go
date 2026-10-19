package storage

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Storage proxies a multihash-keyed CAS to an S3-compatible object store
// (the primary target being Ceph RADOS Gateway; MinIO and AWS also work). Like
// the http backend it is a *storage* backend with its own layout, and the
// multihash stays opaque to this package: it is only rendered as the hex of
// its bytes to form keys, exactly as the dir and sql backends do with theirs.
//
// The key is the hex of the multihash bytes (not a CID — that is a
// client-side derivation), and the name index is a separate object whose body
// is that same hex hash, mirroring the dir symlink / sql foreign key:
//
//	blob   <bucket>/<hex>            blob bytes
//	index  <bucket>/index/<name>     body = <hex>
//
// Blobs and index are kept separate (as in sql) and a name is only a *link*
// to its blob's key, never a copy of the content, so names do not double the
// storage of big blobs. Last add wins: Index is a PUT over an existing name.
type S3Storage struct {
	client   *minio.Client
	bucket   string
	readOnly bool
}

// NewS3Storage builds a client to endpoint (host:port, or a full
// http(s):// URL whose scheme picks TLS), scoped to bucket. Access key /
// secret ride on auth (a zero Auth = anonymous). region is the S3 region used
// for SigV4 signing: the CLI defaults it (e.g. us-east-1, which Ceph RGW
// accepts); if it is left empty the client defers to the server's bucket
// location.
func NewS3Storage(endpoint, bucket, region string, auth Auth, mode Mode) (*S3Storage, error) {
	if bucket == "" {
		return nil, errors.New("s3 backend requires a bucket name")
	}
	// A scheme-less endpoint uses http (secure=false); a scheme-d endpoint
	// must agree with that flag. Detecting the scheme here keeps the client's
	// signing in step with how the endpoint was written.
	secure := strings.HasPrefix(endpoint, "https")

	var creds *credentials.Credentials
	if auth.hasCredentials() {
		creds = credentials.NewStaticV4(auth.User, auth.Password, "")
	} else {
		creds = credentials.NewStaticV4("", "", "")
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:        creds,
		Secure:       secure,
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 backend: %w", err)
	}

	return &S3Storage{
		client:   client,
		bucket:   bucket,
		readOnly: mode != ReadWrite,
	}, nil
}

// blobKey is the object key of a blob: the hex of the multihash bytes (this
// package treats the digest as opaque and only renders it for the key).
func (s *S3Storage) blobKey(hash Digest) string {
	return hex.EncodeToString(hash)
}

// indexKey is the object key of a name index entry.
func (s *S3Storage) indexKey(name string) string {
	return indexPrefix + "/" + name
}

// isNoSuchKey reports whether err is a definitive "object does not exist"
// (the S3 404), which minio-go surfaces as Code NoSuchKey. A bare 404 with
// no S3 XML body is already mapped to NoSuchKey by the client, so a server
// that returns a plain 404 still lands here. Everything else is a real error.
func isNoSuchKey(err error) bool {
	return minio.ToErrorResponse(err).Code == minio.NoSuchKey
}

// isRangeNotSatisfiable reports whether err is a 416 response to our ranged
// request. S3 uses Code `InvalidRange` for this (HTTP status 416, `Requested
// Range Not Satisfiable`), which a real RGW/AWS sends in its S3 XML body,
// *and* a bare 416 (from a well-behaved non-S3 server or our in-process sim)
// which minio-go falls back to Code "416 Requested Range Not Satisfiable".
// Matching on `StatusCode == 416` covers both cases. It fires specifically
// when the object is shorter than the requested range (the contract case for
// `Peek` on a short/empty blob). We treat it as "0 bytes" so we stay honest
// to the contract and don't surface a spurious failure against strict
// S3-compliant servers.
func isRangeNotSatisfiable(err error) bool {
	return minio.ToErrorResponse(err).StatusCode == http.StatusRequestedRangeNotSatisfiable
}

func (s *S3Storage) Put(hash Digest, size int64, r io.Reader) error {
	if s.readOnly {
		return ErrReadOnly
	}

	ctx, cancel := context.WithTimeout(context.Background(), s3PutTimeout)
	defer cancel()

	_, err := s.client.PutObject(ctx, s.bucket, s.blobKey(hash), r, size, minio.PutObjectOptions{})
	if err != nil {
		return err
	}
	return nil
}

func (s *S3Storage) Get(hash Digest) (io.ReadCloser, int64, int64, error) {
	obj, err := s.client.GetObject(context.Background(), s.bucket, s.blobKey(hash), minio.GetObjectOptions{})
	if err != nil {
		if isNoSuchKey(err) {
			return nil, 0, 0, errors.New("blob not found")
		}
		return nil, 0, 0, err
	}

	// GetObject defers the HTTP request to the first read, but Stat always
	// performs it and returns the full object size and last-modified time;
	// calling it before handing out the reader also surfaces the not-found
	// case as an error here.
	stat, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if isNoSuchKey(err) {
			return nil, 0, 0, errors.New("blob not found")
		}
		return nil, 0, 0, err
	}

	// stat.LastModified is the server's LastModified. A zero time.Time means
	// the server did not send one — in that case return 0, the documented
	// "unknown" sentinel; we do not fabricate one (e.g. time.Now()).
	var mtime int64
	if !stat.LastModified.IsZero() {
		mtime = stat.LastModified.Unix()
	}

	// obj is an io.ReadCloser; closing it releases the open transfer.
	return obj, stat.Size, mtime, nil
}

func (s *S3Storage) Has(hash Digest) (bool, error) {
	_, err := s.client.StatObject(context.Background(), s.bucket, s.blobKey(hash), minio.StatObjectOptions{})
	if err != nil {
		if isNoSuchKey(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *S3Storage) Index(name string, hash Digest) error {
	if s.readOnly {
		return ErrReadOnly
	}

	key := s.indexKey(name)
	body := hex.EncodeToString(hash)
	ctx, cancel := context.WithTimeout(context.Background(), s3PutTimeout)
	defer cancel()

	_, err := s.client.PutObject(ctx, s.bucket, key, strings.NewReader(body), int64(len(body)), minio.PutObjectOptions{})
	if err != nil {
		return err
	}
	return nil
}

func (s *S3Storage) Lookup(name string) (Digest, error) {
	obj, err := s.client.GetObject(context.Background(), s.bucket, s.indexKey(name), minio.GetObjectOptions{})
	if err != nil {
		if isNoSuchKey(err) {
			return nil, errors.New("name not found")
		}
		return nil, err
	}
	defer obj.Close()

	// The name entry is the hash itself (the link, not the blob), so it is
	// small and safe to read whole.
	raw, err := io.ReadAll(obj)
	if err != nil {
		return nil, err
	}

	digest, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("name %q is not a valid hash: %q", name, string(raw))
	}
	return digest, nil
}

// Names lists the name index objects (keys "index/<name>") via an S3
// bucket listing, so it needs no extra server facility — the object API
// that every S3-compatible endpoint must implement. The prefix keeps the
// listing scoped to the index (never the CAS), and names are the key with
// the prefix stripped. Sorted client-side for the contract's stable
// order; an index with no entries is a nil slice, not an error.
func (s *S3Storage) Names() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s3PutTimeout)
	defer cancel()

	names := make([]string, 0)
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix: indexPrefix + "/",
	}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		// Strip the "index/" prefix and drop folder markers (a key with
		// nothing else, which our own Index method never creates but a
		// folder marker from another tool might).
		rest := strings.TrimPrefix(obj.Key, indexPrefix+"/")
		if rest == "" || strings.ContainsRune(rest, '/') {
			continue
		}
		names = append(names, rest)
	}
	sort.Strings(names)
	return names, nil
}

// Peek returns the first n bytes of a blob via a ranged GET (bytes=0..n-1), so
// only the head is fetched. At most n bytes are returned; fewer if the blob is
// shorter or empty. Bounded regardless of blob size and never materializes the
// blob. If the object is shorter than the requested range the server returns
// the whole object, which is still within the n-byte bound here.
func (s *S3Storage) Peek(hash Digest, n int) ([]byte, error) {
	if n < 0 {
		n = 0
	}
	if n == 0 {
		return nil, nil
	}

	opts := &minio.GetObjectOptions{}
	if err := opts.SetRange(0, int64(n-1)); err != nil {
		return nil, err
	}

	obj, err := s.client.GetObject(context.Background(), s.bucket, s.blobKey(hash), *opts)
	if err != nil {
		if isNoSuchKey(err) {
			return nil, errors.New("blob not found")
		}
		if isRangeNotSatisfiable(err) {
			// The object is shorter than the requested range (possibly empty);
			// per the Peek contract, return 0 bytes rather than erroring.
			return nil, nil
		}
		return nil, err
	}
	defer obj.Close()

	buf := make([]byte, n)
	got, err := io.ReadFull(obj, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		// minio-go defers the ranged GET to the first Read, so a 416 from a
		// spec-compliant S3 server surfaces here (not on the GetObject call,
		// which succeeds). Treat it as "0 bytes" per the Peek contract — the
		// object is shorter than the requested range (possibly empty).
		if isRangeNotSatisfiable(err) {
			return buf[:got], nil
		}
		return nil, err
	}
	return buf[:got], nil
}

// Close is a no-op: minio-go owns its HTTP client and the backend has no
// dedicated resources to release (the dir backend is the same).
func (s *S3Storage) Close() error {
	return nil
}

// s3PutTimeout bounds a single Put / Index transfer. The CLI opens this
// backend one-shot per invocation, so a hung upload is never retried by us —
// minio-go's own per-request retries still apply within the window. A generous
// ceiling (rather than a tight one) so multi-part uploads of large blobs are not
// aborted by the clock.
const s3PutTimeout = 10 * time.Minute
