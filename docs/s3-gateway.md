# The S3 gateway

Backup programs talk to the bridge as if it were an S3 service. The bridge keeps
what they write on its own disk and sends it to OpenDrive in the background
(whitepaper §3.6). This page is how to point a program at it, what it does and
does not do, and how it is protected.

## Turning it on

It runs whenever the caching gateway runs with write-back — `--cache-dir` (or
`ODB_CACHE_DIR`), which the Docker compose file sets. Nothing else is needed:
the first start makes an access key and an HTTPS certificate. See them with

```bash
odctl s3
```

which prints the endpoint, the access key, the secret, and the certificate's
fingerprint. The web page shows the same.

| Setting | Default | |
|---|---|---|
| `--s3-https-addr` / `ODB_S3_HTTPS_LISTEN` | `0.0.0.0:9751` | HTTPS, reachable from your network (a NAS needs this). Empty switches it off. |
| `--s3-http-addr` / `ODB_S3_HTTP_LISTEN` | `127.0.0.1:9752` | Plain HTTP for programs on the same machine. **Loopback only** — anything else is refused at startup. |
| `--s3-tls-cert`, `--s3-tls-key` | self-signed | Your own certificate instead, e.g. the one DSM gets from Let's Encrypt. |
| `--s3-tls-hosts` / `ODB_S3_TLS_HOSTS` | — | Extra names or addresses for the self-signed certificate (in a container it can only see the container's own). |
| `--s3-delete` / `ODB_S3_DELETE` | `trash` | `trash`: deletes go to OpenDrive's trash and can be recovered. `permanent`: gone. |
| `--s3` / `ODB_S3` | `true` | `false` switches the gateway off. |

`odctl s3 reset` replaces the key; every program using the old one stops working
at once.

## Setting up a program

Every program needs: the endpoint, the access key and secret, **path-style**
addressing, and a region (any; use `us-east-1`). A bucket is a top-level folder
on OpenDrive — create one per program.

- **Synology Hyper Backup** — *S3 Storage → Custom Server URL*: the NAS reaches
  the bridge at `https://<bridge address>:9751`. Hyper Backup requires HTTPS; it
  will be shown the bridge's own certificate, and its fingerprint is what `odctl
  s3` prints. *Whether Hyper Backup accepts a self-signed certificate has not
  been tested yet* (S3 acceptance, first item). If it refuses, give the bridge a
  real certificate with `--s3-tls-cert`/`--s3-tls-key`.
- **restic** — `restic -r s3:http://127.0.0.1:9752/<bucket>/<path> init`, with
  `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` set. Over the network use the
  `https://…:9751` address and `--cacert` with the certificate file, or
  `--insecure-tls`.
- **rclone** — type `s3`, provider `Other`, `endpoint = http://127.0.0.1:9752`,
  `force_path_style = true`.
- **Kopia, Arq, Duplicati** — "S3-compatible" with the same four values.

## What it promises

- **Nothing is acknowledged before it is safe on the bridge's disk** (§3.5.2):
  the bytes and the journal entry are flushed first. An object then stays on the
  bridge until OpenDrive has it and its MD5 matches. `odctl cache status` says
  how much is still waiting; `odctl cache flush --wait` waits for it.
- **What you wrote, you can read, list and delete at once**, before it reaches
  OpenDrive.
- **Writes are accepted while OpenDrive is unreachable**, and delivered —
  folders created as needed — once it is back. Two exceptions, both answered
  with a retryable 503: a bucket the bridge has not seen since it started (it
  cannot tell whether it exists), and *listings*. A listing made without OpenDrive
  would leave out everything already there, and a backup program told its data
  has gone would draw the wrong conclusion; a 503 it retries does no harm.
- **Every body is checked.** It must be exactly as long as declared; a
  `Content-MD5` is checked; a signed `x-amz-content-sha256` is checked against
  the body. Nothing that fails is stored.
- **Reads are checked**: an object up to 64 MB not already on the bridge is
  fetched whole, checked against OpenDrive's MD5, and served from disk. Larger
  ones are streamed straight through.

## Security

- **Every request must be signed** with AWS Signature Version 4 and the bridge's
  key. Unsigned requests, version-2 signatures, a wrong secret and an unknown key
  are all refused with 403. **Presigned URLs are refused too**: none of the
  supported programs uses them, and a leaked one would be a leaked credential.
- The key lives only in `credentials.key`, encrypted like the OpenDrive sign-in,
  and signing out of OpenDrive does not remove it (switching accounts would
  otherwise break every backup). It is never written to a log; `odctl s3`, the
  web page and `GET /v1/s3` — all behind the bridge's own API address and
  optional API key — are the only ways to read it.
- Plain HTTP is only ever offered on loopback. Anything that crosses a network
  uses TLS 1.2 or later.
- The self-signed certificate is valid for 825 days and replaced 30 days before
  it runs out, or when `--s3-tls-hosts` asks for a name it does not have.
  Replacing it changes the fingerprint: a program that pinned the old one must be
  told to trust the new one.

## Known differences from Amazon S3

- **No versioning, object lock, ACLs, lifecycle rules or custom metadata.**
  OpenDrive has nothing to keep them in. Every object reports
  `application/octet-stream`.
- **Multipart ETags.** Completing a multipart upload returns the S3-style
  `md5-of-md5s-N` ETag; afterwards the object reports the MD5 of the whole file,
  which is what OpenDrive stores. None of the target programs compares the two.
- **Keys OpenDrive cannot hold as names** — `\ : * ? " < > |`, control
  characters, a leading or trailing space — are stored with look-alike
  characters and given back exactly as written. A key part longer than 255 bytes
  once stored, one that begins or ends with an unusual space character (a
  no-break space, say), or one with an empty part (`a//b`) is refused, in words,
  when it is written — never accepted and then stuck.
- **`key/` objects are folders.** A zero-byte object whose key ends in `/` makes
  the folder; deleting it removes the folder only when it is empty.
- **Copies go through the bridge**: the bytes are read and written again, and the
  copy obeys the same rules as any write.
- **An object larger than the unsent allowance** (`--cache-max-dirty-bytes`) in a
  single PUT is refused with `EntityTooLarge`; multipart uploads of it work.

## Audit: the S3 library, and what the bridge adds

The protocol is served by [gofakes3](https://github.com/rclone/gofakes3) (MIT),
the S3 server rclone uses. Reading its source before relying on it found these,
each now handled in `internal/server/s3_http.go` or `s3.go` and tested in
`s3_test.go`/`s3_more_test.go`:

| gofakes3 behaviour | Consequence if left | What the bridge does |
|---|---|---|
| With no keys configured it accepts every request. | One missing setting and the account is open. | The bridge verifies signatures itself, in front of gofakes3, and refuses to build a gateway without a key. |
| A plain PUT is not checked for length; a short body is stored. | A dropped connection leaves a truncated object that was acknowledged. | Exactly the declared length must arrive, and the body is read to its end. |
| `Content-MD5` is checked only when the body is read to its end. | The check can be skipped. | Bodies are always read to the end. |
| A signed `x-amz-content-sha256` is never compared with the body. | A body altered in transit is accepted under a valid signature. | Compared; a mismatch stores nothing. |
| Chunk signatures in aws-chunked bodies are not checked. | Same, for streamed uploads. | *Not closed.* The request signature is still checked; over HTTPS the transport protects the body; plain HTTP is loopback only. |
| Presigned URLs have no upper limit on validity. | A leaked URL works for as long as its author chose. | Presigned URLs are refused. |
| Any error code it does not know becomes a 500. | "OpenDrive is away for a minute" looks like a server fault; some clients give up. | The backend chooses 503 with `Retry-After`, or 400, and the guard applies it. |
| The request path is trimmed of `/` at both ends. | `dir/` is stored as a file called `dir`, colliding with the folder `dir/x` needs. | The trailing `/` is carried past gofakes3 in a form no valid key can take. |
| A PUT without `Content-Length` is refused, even when the length is in `X-Amz-Decoded-Content-Length`. | minio-go (restic's library) sends empty objects that way; every empty file failed. | The decoded length is supplied, and an empty chunked body is given a length of 0. |
| Its own multipart implementation holds every part in memory. | A large backup exhausts memory. | Parts are streamed to disk and flushed; completing an upload streams them into the cache. |
