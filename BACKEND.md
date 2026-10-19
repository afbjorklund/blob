# Backend Quick Start

How to point `blob` at a server that already exists. This is a **Quick
Start**: each section is complete enough to go from a blank host to a working
`blob add` / `blob get`, with pointers to the upstream docs for the deeper
details. `dir` and `sql` are local, so they appear here mainly for the layout
you'd see if you peeked at the raw store.

Two things to keep in mind for the remote backends:

- The client never provisions anything. The *operator* creates the store /
  directory, the user / key, and the bucket — the client just talks to them.
- The client only ever sends `GET`/`PUT`/`HEAD` (plus ranged `GET` for the
  "peek the first byte" read). Your server only needs to serve/accept those.

## `dir` — local directory

The default. No server to run; it reads and writes a normal directory.

```
blob -s dir add file.sif            # default --path is ./.blob-store
blob -s dir --path /srv/store add file.sif
```

How it is organized (created automatically, no migration steps):

```
<--path>/
├── 12/20/…/<rest>          # one file per blob, fanned out by the first 3 key
│                           # bytes into 1-byte dirs (<b0>/<b1>/<b2>/<rest>)
└── index/
    └── <name>  -> ../…       # a symlink to the blob, the name→blob link
```

- Blobs: one file per multihash, hex-encoded, spread across up to 3 one-byte
  levels so a single directory never holds more than 256 entries
  (`.blob-store/12/20/<byte3>/<rest>` for a SHA-256 multihash).
- Name index: `index/<name>` is a symlink back to the blob file, so reading the
  name directly yields the content.
- Windows note: symlinks are Linux-only here. A Windows / `core.symlinks`
  style text-target fallback is the intended migration if you need it.

## `sql` — local SQLite database

`--path` is the database *file*. Single file, no server, no extra config.

```
blob -s sql add file.sif           # default --path is ./.blob-store (a dir)
blob -s sql --path /srv/store.db add file.sif
```

How it is organized (schema auto-created on first open, WAL mode for
concurrent readers + writer):

```
blobs ( hash BLOB PRIMARY KEY, data BLOB NOT NULL, mtime INTEGER NOT NULL DEFAULT (strftime('%s','now')) )   -- one row per blob
names ( name TEXT PRIMARY KEY, hash BLOB NOT NULL REFERENCES blobs(hash) )
```

- `hash` in `blobs` is the multihash key; `names.hash` is a real foreign key
  to `blobs(hash)`, so the name→blob link is enforced at the DB level.
- `mtime` is the blob's last-modified time in Unix seconds (defaulted to
  "now" on insert; we do not use `CURRENT_TIMESTAMP` because it is ISO-8601
  text rather than a plain integer, and a column we can compare in SQL is
  easier to query).
- Reads stream via the SQLite blob API (`OpenBlob`), so the blob is never
  loaded whole into memory.

## `http` — one web server, `GET`/`PUT`/`HEAD`

`--path` is a base URL. The store (blobs + name index) lives *on the server*;
this client just issues `PUT` (add), `GET`/`HEAD` (get/lookup/serve).

On-wire layout the server must serve (blobs at `<base>/<hex>`, name index at
`<base>/index/<name>` — the index body is the hex multihash, not the blob):

```
PUT  <base>/<hex>            body = blob bytes          (add)
GET  <base>/<hex>            → blob bytes               (get)
HEAD <base>/<hex>            → 200 / 404                 (existence check)
PUT  <base>/index/<name>     body = hex multihash        (index / add name)
GET  <base>/index/<name>     → hex multihash             (lookup)
GET  <base>/index/           → JSON autoindex of names   (list names)
```

`GET <base>/index/` (the directory listing, with the trailing slash) is the
backend's `Names()` surface — the index contents as a JSON array of
`{"name":..., "type":"file", ...}` entries. The server must have autoindex
enabled for that directory **and** be serving it as JSON (the two-step
contract: autoindex on + JSON format). nginx achieves this with:

```nginx
location /index/ {
    autoindex on;
    autoindex_format json;
}
```

Any other server (Apache `mod_autoindex` with a `AddOutputFormat` JSON
directive, a reverse proxy that generates the same JSON, Ceph RGW's
`?list-type=2` XML converted to the same shape, etc.) is fine as long as the
directory GET returns the same JSON shape.

A well-behaved server should return `Last-Modified` (RFC 7231) on the blob
`GET` responses — that is how the backend learns the blob's last-modified time
for the `Last-Modified` header the read-only gateway emits and for the
`blob get --only-mtime` flag. If a server does not send it, the backend
reports `0` ("unknown") rather than inventing one: the gateway then omits
`Last-Modified`, and `--only-mtime` prints `0`. So adding the header to an
existing server changes the observable output from "no time" to "a real
time" — nothing else about the contract changes.

### nginx + htpasswd (Quick Start)

1. Writable root for the store:

   ```bash
   mkdir -p /srv/blob && chown -R nginx:nginx /srv/blob
   ```

2. Server block (note `limit_except GET HEAD` *allows* `PUT` for the write
   path — nginx refuses it by default):

   ```nginx
   server {
       listen      8080;
       server_name  blob.localhost;
       root        /srv/blob;

       auth_basic        "blob";
       auth_basic_user_file /etc/nginx/htpasswd;

        location / {
            try_files $uri @forbidden;
            # Allow reads for everyone who authenticates, and writes too,
            # but only from the trusted internal range:
            limit_except GET HEAD {
                allow 10.0.0.0/8;
                deny  all;
            }
        }
        # The `list` command needs the index/ directory as a JSON autoindex:
        location /index/ {
            autoindex on;
            autoindex_format json;
        }
        location @forbidden { return 403; }
   }
   ```

3. Add the user (the password for `auth_basic`):

   ```bash
   htpasswd -c -B /etc/nginx/htpasswd blob     # → set the password
   ```

4. Point the client at it:

   ```bash
   blob -s http -p http://blob.localhost:8080 -u blob -P <pass> add file.sif
   blob -s http -p http://blob.localhost:8080 -u blob -P <pass> get file.sif -o out.sif
   ```

For more on `auth_basic`/`htpasswd`, see the nginx docs (http://nginx.org/en/docs/).

## `s3` — one object bucket (primary target: Ceph RGW)

`--path` is the endpoint (host:port or an `http(s)://` URL), `--bucket` is the
named store inside it. This backend speaks the S3 object API (PUT/GET/HEAD on
`<bucket>/<key>`), so any S3-compatible server works:

- **Ceph RGW** (primary target) — `--path http://rgw.host:7480` (the S3 port).
- MinIO — `--path http://minio.host:9000`.
- AWS S3 — `--path s3.us-east-1.amazonaws.com` (`--region` matters here).

In-bucket layout (blobs and name index are *separate objects*, the index body
is the hex multihash — a link, not a copy of the blob):

```
<bucket>/<hex of multihash>        blob bytes
<bucket>/index/<name>              body = hex multihash   (the name→hash link)
```

### Ceph RGW: create the key + bucket (once, as the operator)

```bash
# S3 API key (these become --user / --pass)
ceph orch apply rgw my-rgw --placement='1@nodeA' \
    --service-id rgw-my   # or: radosgw-admin user create …

radosgw-admin user create --uid=blobstore --display-name=blob-store
# → prints "access" (your --user) and "secret" (your --pass)

radosgw-admin bucket create --uid=blobstore --bucket=blobstore
```

Then the client:

```bash
# scheme-less endpoint defaults to http; use https://… for TLS
export BLOB_USER=<access> BLOB_PASS=<secret>      # or -u / -P flags
blob -s s3 --path 127.0.0.1:7480 --bucket blobstore add file.sif
blob -s s3 --path 127.0.0.1:7480 --bucket blobstore get file.sif -o out.sif
```

For more on RGW / Ceph, see the Ceph docs (https://docs.ceph.com/).

### Credentials, by backend

| flag          | `http` (Basic auth)        | `s3` (SigV4)                |
|---------------|----------------------------|-----------------------------|
| `--user`/`BLOB_USER`   | username / token | S3 access key |
| `--pass`/`BLOB_PASS`   | secret / token   | S3 secret key |
| `~/.netrc` (per host)  | accepted by both backends |   |

Also available (s3 only): `--region` / `BLOB_S3_REGION`. Unset by default —
minio-go then defers to the server's bucket location (fine for RGW, MinIO, and
Ceph). Set it for region-aware deployments such as AWS where the region takes
part of the SigV4 signature (e.g. `--region us-west-2`).

For more on the S3 API / SigV4, see the AWS S3 docs (https://aws.amazon.com/s3/)
or MinIO (https://min.io/docs/minio/).

## Smoke tests

Each backend has an opt-in end-to-end test that runs against a real target and
skips otherwise, so `go test ./...` stays green in a sandbox. The in-process
shims (in-memory `httptest` servers) already cover the protocol shape, so the
smoke tests exist to verify the *real* thing: network round-trips, a real
filesystem under the `dir` backend, real SigV4 signing for `s3`, etc.

| test          | env var           | verifies                                                   |
|---------------|-------------------|------------------------------------------------------------|
| `TestDirSmoke`  | `BLOB_DIR_PATH`   | a real directory: plain fs, NFS/CIFS, FUSE (rclone, sshfs, …) |
| `TestHTTPSmoke` | `BLOB_HTTP_PATH`  | a real HTTP server speaking the on-wire contract above     |
| `TestS3Smoke`   | `BLOB_S3_ENDPOINT` + `BLOB_S3_BUCKET` | a real S3 / Ceph RGW endpoint (creds via `BLOB_USER`/`BLOB_PASS`) |

All three check the same contract: `Put` → `Peek` (first bytes) → name
`Index`/`Lookup` round-trip with last-add-wins re-point → `Names` (list the
index, not the CAS) → the read-only guard rejecting writes → `Has`/`Get` full
read-back.

```bash
BLOB_DIR_PATH=/mnt/nfs/blobstore \
    go test -v -run TestDirSmoke ./storage/

BLOB_HTTP_PATH=http://blob.localhost:8080 BLOB_USER=blob BLOB_PASS=<pass> \
    go test -v -run TestHTTPSmoke ./storage/

BLOB_S3_ENDPOINT=127.0.0.1:7480 BLOB_S3_BUCKET=blobstore \
BLOB_USER=<access> BLOB_PASS=<secret> \
    go test -v -run TestS3Smoke ./storage/
```

## What the *gateway* is (not a server you set up)

`blob serve` is different from the `http` backend: it is a **read-only,
user-facing CID gateway** built into this binary, like the IPFS gateway. Given
any backend under it, it serves `GET /blob/<CID>` and `GET /name/<name>`
(302 → `/blob/<CID>`). You don't provision it — you run it:

```bash
blob -s dir serve            # or -s sql / -s http / -s s3 with the matching flags
curl -L http://127.0.0.1:8080/name/file.sif
```
