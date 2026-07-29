# Error taxonomy — what upstream says versus what is actually true

This document is the **sole source of judgement** for classifying an upstream
failure. Every binding, the transfer pipeline and the job engine take their
`Kind` and their retry decision from `pkg/opendrive/classify.go`, which
implements exactly what is written here. No module may sniff a status code or a
message on its own (whitepaper §4.5, `CLAUDE.md` rule 9).

It exists because OpenDrive's error surface is unreliable in a specific,
repeatable way: **the status code and the message frequently describe something
other than what actually went wrong.** Two entries in `discrepancies.md` cost a
full debugging round each for exactly this reason — D38 (an HTML 401 that had
nothing to do with the token) and D39 (a permission message for a condition that
was not about permissions). Patching those one at a time does not scale, because
the next binding meets the next disguise. So the rule is inverted: classification
is driven by *evidence we gather*, not by *what upstream claims*.

## The evidence

The classifier may look at four things, and nothing else:

| evidence | why it matters |
|---|---|
| **HTTP status** | necessary, never sufficient. Upstream reuses 400/401/403 for unrelated conditions. |
| **body shape** | JSON object / JSON array / HTML / plain text / empty. A non-JSON body means the response *did not come from the API*, so nothing in it may be believed about credentials. |
| **`Content-Type`** | corroborates the body shape; `text/html` on an error is the edge, not the API. |
| **endpoint identity** | the path decides which front end answered and which parameter conventions apply. `upload_file_chunk2.json` is served by a different process than the rest of the API (D38). |

Two further inputs are used only where the table below says so, and both are
gathered rather than read off the response:

- **an access probe** — one idempotent read (`users/info.json`) that answers
  "does this credential still work at all?". Single-flight, cached, and rate
  limited, so a burst of ambiguous errors produces at most one probe.
- **a success witness** — whether this client has ever completed a comparable
  call successfully in this process. This is the only evidence that separates
  T2a from T2b below, because their wire forms are byte-identical.

## The invariant

> **A response that is not API-shaped, or an ambiguous error that has not been
> disambiguated, must never drive the authentication state machine.**

No refresh, no silent re-login, no `reauth_required`, no captcha circuit-breaker
transition. This is the generalisation of the failure D38 describes: an HTML 401
from an edge proxy classified as `token_expired` sends the auth machine chasing a
token that was never the problem, and in the worst case burns the refresh token
and walks the account into a captcha lock. Pinned by
`TestNonAPIBodyNeverDrivesTheAuthStateMachine` and
`TestAmbiguousErrorNeverDrivesTheAuthStateMachine`.

Only two signals are permitted to reach the auth machine, and both must arrive in
a JSON body from the API: the OAuth error identifiers (`invalid_token`,
`invalid_grant`, `invalid_client`, …) and the session-expiry message of T12.

---

## The disguise forms

| ID | actual situation | what upstream returns | correct `Kind` | how it is decided |
|----|------------------|----------------------|----------------|-------------------|
| [T1](#t1) | wrong credential *type* for that front end | `401` + an openresty HTML page | `edge_rejected` | body is not JSON |
| [T2a](#t2) | resource pressure / transient refusal | `403` "Your user access enables you only to view this folder…" | `upstream_error`, **retryable** | access probe passes **and** a comparable call has succeeded |
| [T2b](#t2) | the account really lacks the right | `403`, byte-identical text | `upstream_error`, permanent | no success witness, or the probe fails the same way |
| [T3](#t3) | the login's account *type* forbids the whole module | `403` "Account users cannot …" | `upstream_error`, permanent | message is specific; no probe spent |
| [T4](#t4) | the parameter encoding is wrong | `400` "Expecting boolean value" — after a boolean was sent | `invalid_request`, permanent | status 400 from the API |
| [T5](#t5) | an empty string does not satisfy "required" | `400` "`access_folder_id` is required." — after it was sent | `invalid_request`, permanent | status 400 from the API |
| [T6](#t6) | the item does not exist | `200` with the arrays omitted | `not_found` | binding-level shape check |
| [T7](#t7) | the folder is permanently deleted | `200` with full metadata | — | never use `info.json` as an existence check |
| [T8](#t8) | the password is correct | `{"result":false}`, no `TempKey` | — | key off the status, never the message |
| [T9](#t9) | the transfer can resume from byte N | `400` "Incorrect chunk offset: uploaded=N" | recovered internally | endpoint identity + message capture |
| [T10](#t10) | the close call has the wrong shape | `400` "Total uploaded=0" — while the bytes are on the server | `invalid_request` | endpoint identity |
| [T11](#t11) | a captcha is required, anywhere | `403`/`200` mentioning captcha | `captcha_required`, never retried | message match, checked first |
| [T12](#t12) | the session really has expired | `401` JSON "Session does not exist, please re-login." | `token_expired` | JSON body **and** API-shaped |
| [T13](#t13) | no information at all | non-2xx, empty body | `upstream_error`, ambiguous | length 0 |
| [T14](#t14) | the account's bandwidth is spent | `200` with `BWExceeded=1`, or a message | `bandwidth_exceeded`, never retried | body field or message |
| [T15](#t15) | a genuine rate limit | `429`/`503` + `Retry-After` | `rate_limited`, retryable | status + header |

---

### T1 — a non-JSON error body is not an API answer {#t1}

Measured on 2026-07-29:

```
POST /v1/upload/upload_file_chunk2.json/OAUTH/xyz?access_token=…
→ 401  Content-Type: text/html
  <html><head><title>401 Authorization Required</title></head>
  <body><center><h1>401 Authorization Required</h1></center>
  <hr><center>openresty</center></body></html>
```

Against the same endpoint with a real session id in the path, the API answers
normally — `400 {"error":{"code":400,"message":"`temp_location` is required."}}`
— which is what proves the 401 came from in front of the API rather than from it.
For contrast, a route that genuinely does not exist is still answered *by the
API*, in JSON: `GET /v1/nosuchmodule/nope.json/{session}` → `404
{"error":{"code":404,"message":""}}`.

So the discriminator is not the status and not the path: it is that the body is
HTML. Nothing in an HTML body may be believed about credentials, because the
process that wrote it never looked at them the way the API does.

`Kind: edge_rejected`. Retryable only when the status is 0, 5xx or 429 — an edge
502 is worth another attempt, an edge 401 is not. Never touches the auth machine.

### T2 — the ambiguous permission 403 {#t2}

This is the form that cost the most, and the one that cannot be settled by
reading the response.

D39 recorded writes failing with `403 "Your user access enables you only to view
this folder, please contact your administrator to discuss your user
permissions."` for a reason that had nothing to do with permissions. Two
explanations were offered there and both turned out to be wrong; what ~500
measured operations actually show is that upstream refuses some writes at random
with this message, sometimes for a minute at a stretch, and accepts the identical
call afterwards.

D25 recorded the *same* message for a genuine, permanent lack of write rights.

Measured side by side on 2026-07-29 with the current account, which now has write
rights on its granted folders but not at the account root:

| request | result |
|---|---|
| `POST /folder.json` into the granted base folder | `200`, created |
| `POST /folder.json` with `folder_sub_parent: "0"` | `403 "Your user access enables you only to view this folder, please contact your administrator to discuss your user permissions."` |

The refusal is byte-for-byte the message D39 saw for a transient condition. **The
message carries no information about which of the two situations holds**, so any
classifier that reads it is guessing, and P4 would surface "contact your
administrator" to a user whose only problem is that a bulk upload got ahead of
itself.

Resolution — two pieces of gathered evidence:

1. **Access probe.** One `users/info.json` read. If it succeeds, the credential
   is fine and the auth machine stays out of it, whatever else is true. If it
   fails with the same 403, the account itself is restricted and the error is
   permanent. The probe is single-flight, cached for 30 s and rate limited to one
   per 30 s, so a job engine pushing fifty files that all trip the condition
   still spends exactly one probe.
2. **Success witness.** Whether the same operation has already succeeded *on the
   same resource* — `Request.Scope`, normally the containing folder id. Rights
   are held per folder, so a success in one folder is not evidence about
   another; a bare operation-level witness would have called a genuine denial at
   the account root transient, because writes into a granted folder use the same
   endpoint. An account that has been writing into this folder all along has a
   witness, so a sudden permission-shaped 403 there is treated as pressure:
   `Temporary() == true`, retried with backoff, and capped by the normal retry
   policy rather than retried forever.

   With no witness, one refusal settles nothing either — D40 measures the two
   cases as byte-identical — so the call is attempted **once** more and the
   verdict is then remembered for the probe interval. A bulk job against a
   folder it truly may not touch therefore costs one extra call in total rather
   than one per file, and a genuine denial is reported after exactly one
   confirmation. The memory expires deliberately: upstream's refusals come in
   windows lasting up to a minute (D39, twice revised), and a verdict kept for
   the life of the process would turn one such window into an operation that
   stays broken until restart — the same mistake as believing the message.

**Replaying a refusal is always safe.** Upstream declined to act, so nothing
happened and a second attempt cannot duplicate anything. `403` and `429` are
therefore replayed whatever the HTTP method, overriding `Request.Retryable`;
`5xx` and timeouts are not, because there the write may well have landed. Without
this, a `POST` would have sat out the single condition retrying exists for.

Both branches classify as `upstream_error` — it is not a credential problem
either way (§4.5) — and they differ only in retryability and in the wording the
job engine is allowed to show the user. T2a must not be reported as a permission
failure.

### T3 — the account-type gate {#t3}

```
GET /v1/sharing/listsharedusers.json/{session} → 403 "Account users cannot list shared users"
```

All seven sharing operations answer this regardless of what was asked (D33). The
wording is specific enough to classify directly, so no probe is spent: it is a
property of the login and no retry will change it. `upstream_error`, permanent.

### T4 — the 400 that asks for the type it was given {#t4}

```
POST /v1/folder/move_copy.json  {"move": false}
→ 400 "Invalid value specified for `move`. Expecting boolean value"
```

A JSON boolean was sent and the reply asks for a boolean (D26). The message is
actively misleading; the real fix is to send `"false"` as a string. Classified as
`invalid_request` because it is a caller-side encoding bug, and never retried —
retrying an encoding error is pure waste.

### T5 — "required" is not satisfied by an empty string {#t5}

```
POST /v1/file.json  {"access_folder_id": ""}
→ 400 "`access_folder_id` is required."
```

It *was* sent (D28). Same treatment as T4: `invalid_request`, permanent. Both T4
and T5 are the reason `CLAUDE.md` requires probing the live API before writing a
binding — a mock will happily accept whatever the spec claims.

### T6 — success that means "not found" {#t6}

`folder/itembyname.json` answers a miss with `200 {"DirUpdateTime":…}` and simply
no `Folders`/`Files` arrays (D23), which reads identically to "the folder is
empty". The binding maps the shape to `not_found`; the classifier cannot see it,
because there is no error to classify.

### T7 — success that means "gone" {#t7}

`folder/info.json` keeps returning full metadata for a permanently deleted folder
(D27). There is no error and no shape difference, so **no classification can
rescue this**: the rule is a binding rule, and it is absolute — existence is
decided by the parent listing or `idbypath`, never by a successful `info` call.
Listed here so the taxonomy is complete about the ways a 200 can lie.

### T8 — `{"result":false}` for a correct password {#t8}

`file/verifypassword.json` returns `{"result":false}` for the correct password
and for a wrong one alike, and never issues a `TempKey` (D32). Separately, the
refusal wording on the public download route depends on which gate upstream
reaches first — "File requires password" or "Download permissions are not enabled
for this file" — for the same underlying state.

Rule: **key off the status, never the message.** A `result:false` from this
endpoint is not evidence of a wrong password and must not be reported as one.

### T9 — the 400 that is really a resume point {#t9}

```
POST /upload/upload_file_chunk2.json/…?chunk_offset=999999
→ 400 "Incorrect chunk offset: uploaded=0, chunk_offset=999999"
```

`uploaded=N` is authoritative: exactly how many bytes upstream holds (D37). The
upload pipeline consumes it and resumes; it never reaches the caller as an error.
Recognised by endpoint identity plus the message pattern, which is why endpoint
identity is part of the evidence set.

### T10 — "Total uploaded=0" while the bytes are on the server {#t10}

A deduplicated upload closed *with* `temp_location` is refused with `400 "Invalid
upload file size. Total uploaded=0. File size=1280"` even though the content is
already stored (D36). `invalid_request`; the fix is the call shape, not a retry.

### T11 — captcha is checked first, everywhere {#t11}

A captcha can appear outside login — `file/verifypassword.json` accepts
`captcha_response` (D9) — so the captcha test runs before any status-based
branch. `captcha_required` is never retried and never triggers a re-login: doing
either is what turns a throttle into a lockout.

### T12 — the one credential signal that is real {#t12}

```
GET /v1/users/info.json/notasession
→ 401  Content-Type: application/json
  {"error":{"code":401,"message":"Session does not exist, please re-login."}}
```

JSON, from the API, unambiguous. This — and the OAuth error identifiers — are the
only inputs allowed to drive the silent re-login state machine. Everything else
in this document is explicitly excluded from it.

The message is matched rather than only the status, because upstream does not
always pair the sentence with a 401: some endpoints report errors inside a 200
body (§11), and a session expiry announced that way is still a session expiry.

### T13 — no evidence at all {#t13}

A non-2xx with an empty body says nothing. It classifies as `upstream_error`,
marked ambiguous, retryable only on the usual 5xx/0 rule, and it may not drive
the auth machine — an empty 401 is exactly the case where guessing is most
tempting and least justified.

### T14 — bandwidth {#t14}

`download/file.json` reports an exhausted allowance in a 200 body (`BWExceeded=1`)
rather than as an error. `bandwidth_exceeded`, never retried: the allowance does
not come back within any retry window, and hammering it is how an account gets
throttled further.

### T15 — a genuine rate limit {#t15}

`429` or `503` with `Retry-After`. `rate_limited`, retryable, and the header's
delay wins over the computed backoff. This is the only form where the status code
alone is trustworthy.

---

## Retry policy, decided in one place

`APIError.Temporary()` and `APIError.RetryAfter()` are the only retry authority in
the codebase. Callers — the transfer pipeline, the job engine, the Bridge REST
layer — consume them and never re-derive them.

| `Kind` | retried | note |
|---|---|---|
| `network` | yes | backoff |
| `rate_limited` | yes | `Retry-After` wins |
| `upstream_error` | only when 5xx/0, **or** when T2a was proven | T2a is the sole message-driven retry, and only with the probe and the witness behind it. `APIError.RetryBudget()` caps it at one attempt where the evidence is weak |
| `edge_rejected` | only when 5xx/0/429 | an edge 401/403 will not change |
| `token_expired` | no — renewed once, then replayed | not a retry; a renewal (§2.2 #3) |
| `captcha_required`, `quota_exceeded`, `bandwidth_exceeded`, `invalid_name`, `invalid_request`, `not_found`, `conflict`, `reauth_required`, `keystore_unavailable` | no | retrying either cannot help or actively harms |

## Adding a form

When a new disguise turns up:

1. Record the raw observation in `docs/discrepancies.md` as usual.
2. Add a row and a section here, with the measured request and response.
3. Add the rule to `classify.go` — as evidence, not as a special case at the call
   site.
4. Add one mock case to `classify_test.go` named after the form, and, if the
   condition is reachable on the sandbox, one live assertion.

A fix applied anywhere other than `classify.go` is a bug in this process.
