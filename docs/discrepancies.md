# Upstream discrepancies — PDF vs live Swagger spec

Per `CLAUDE.md` rule 3 and whitepaper §2.6/§12.2, the live Swagger specification
(`https://dev.opendrive.com/api/v1/resources/*.json`) and real sandbox responses
take precedence over `docs/OpenDrive_API_guide.pdf`. Every divergence found while
implementing the Bridge is recorded here.

**Baseline:** `testdata/spec/`, produced by `tools/fetch-spec` with the test
account: 17 modules, 220 endpoints, 220 operations. See
`testdata/spec/manifest.json` for the fetch timestamp, the auth strategy used and
per-file SHA256 checksums.

| ID | Status | Area |
|----|--------|------|
| [D1](#d1) | confirmed, implemented | `download/all.json` uses `session_id`, not `session_key` |
| [D2](#d2) | confirmed, implemented | `session/captcharequired.json` missing from the PDF |
| [D3](#d3) | confirmed, implemented | `/v1` appears in both base path and endpoint paths |
| [D4](#d4) | open, P3 | undocumented `download/file.json` parameters |
| [D5](#d5) | open, P2 | undocumented `file/thumb.json` parameters |
| [D6](#d6) | open, P1 note | `oauth2/grant.json` grant types and `client_id` semantics |
| [D7](#d7) | confirmed, implemented | listing pagination parameters |
| [D8](#d8) | open, out of scope | public `users/*` endpoints absent from the PDF |
| [D9](#d9) | open, P2 | captcha applies beyond login |
| [D10](#d10) | resolved | the `sharing` module is invisible to anonymous callers |
| [D11](#d11) | resolved | only the OAuth2 token unlocks the full explorer spec |
| [D12](#d12) | **PDF is wrong**, implemented | the breadcrumb endpoint is spelled correctly upstream |
| [D13](#d13) | new endpoint, P3 | `upload/has_ddref.json` is a dedupe probe |
| [D14](#d14) | **PDF is wrong**, implemented | the file resource is `/file.json`, not `/file/file.json` |
| [D15](#d15) | **PDF is wrong**, implemented | three endpoints use a different verb than documented |
| [D16](#d16) | confirmed, implemented | `move` and `overwrite_if_exists` are required strings |
| [D17](#d17) | open, P2 | `folder/list.json` has three undocumented parameters |
| [D18](#d18) | confirmed, implemented | expiring links pass everything as path segments |
| [D19](#d19) | confirmed, implemented | the same field is a string in one response and a number in another |
| [D20](#d20) | implemented | `users/info.json` returns a `PrivateKey` |
| [D21](#d21) | informational | six modules beyond the v1.0 scope are live |
| [D22](#d22) | confirmed, implemented | `idbypath` answers with `FolderId`, everything else with `FolderID` |
| [D23](#d23) | confirmed, implemented | `itembyname` reports "not found" by omitting the arrays |
| [D24](#d24) | confirmed, implemented | folder and file `move_copy` disagree on the type of `move` |
| [D25](#d25) | resolved | the sandbox account is read-only |
| [D26](#d26) | **spec is wrong**, implemented | `folder/move_copy` rejects the boolean `false`, so a copy needs the string |
| [D27](#d27) | confirmed, implemented | `folder/info.json` still answers for a permanently deleted folder |
| [D28](#d28) | **live API is stricter**, implemented | `file.json` rejects an empty `access_folder_id` |
| [D29](#d29) | confirmed, implemented | `DELETE /file.json` deletes a file that was never trashed |
| [D30](#d30) | **spec is wrong**, implemented | `file/filefullpath.json` answers in `DownloadLink`, with backslashes |
| [D31](#d31) | **spec is wrong**, implemented | the expiring-link endpoints return one object, not an array |
| [D32](#d32) | open, blocks password-protected downloads | `file/verifypassword.json` answers false for a correct password |

---

## D1 — `download/all.json` calls its session parameter `session_id`, not `session_key` {#d1}

*Whitepaper §2.6 #2 (from the PDF)* states that `POST /download/all.json` is the
one endpoint that names the session parameter `session_key`.

*Live spec* (`testdata/spec/download.json`) documents the request body as
`files`, `folders` and **`session_id`** — in both the anonymous and the
authenticated fetch.

**Resolution:** follow the live spec; the SDK sends `session_id` by default.
`Request.SessionParam` still allows a per-endpoint override, so if the sandbox
turns out to accept only `session_key` the fix is one field on one call.

**Follow-up:** exercise the real endpoint during P3 (download pipeline).

Covered by `TestArchivedSpecDocumentsTheDownloadAllSessionParameter`,
`TestSessionParamOverride`.

## D2 — `session/captcharequired.json` exists online but not in the PDF {#d2}

`GET /v1/session/captcharequired.{format}`, one optional `username` query
parameter:

> Lets the login page render the captcha on first load when the client's IP is
> already throttled, instead of only after a failed submission.

The concrete instance of whitepaper §2.6 #1.

**Resolution:** implemented as `opendrive.CaptchaRequired(ctx, client, username)`
with the `CaptchaStatus` model.

## D3 — `basePath` stops at `/api` while every declaration path starts with `/v1` {#d3}

Every module declares `"basePath": "https://dev.opendrive.com/api"` and paths
such as `/v1/session/login.{format}`, while the Bridge base URL is
`https://dev.opendrive.com/api/v1`. Naive concatenation yields `/api/v1/v1/...`.

**Resolution:** `joinPath` collapses a duplicated trailing base segment, so both
`/session/login.json` and `/v1/session/login.json` resolve to the same URL.

## D4 — `download/file.json` accepts parameters the PDF does not list {#d4}

*PDF / whitepaper §2.3:* `session_id`, `offset`, `inline`, `sharing_id`, `test`,
`backup`, `temp_key`. *Live spec* adds `app` (string), `temp_auth` (string) and
`preview` (int).

**Status:** open; semantics unknown. `temp_auth` is treated as a credential by
the redaction list already. To be probed in P3 before any is exposed.

## D5 — `file/thumb.json` accepts `time_offset` and `temp_key` {#d5}

`time_offset` (float — presumably the video frame to grab) and `temp_key` are not
in the PDF's thumbnail section.

**Status:** open, to be confirmed in P2.

## D6 — `oauth2/grant.json` documents three grant types and a different `client_id` meaning {#d6}

> `grant_type` : authorization_code, password, refresh_token
> `client_id` : **Partner ID**
> `username`, `password`, `code`

1. The whitepaper (§2.2 B) covers only `password` and `refresh_token`; an
   `authorization_code` flow exists and would need a registered partner. Out of
   scope for v1.0.
2. `client_id` is documented as the *partner id* while the whitepaper passes the
   literal `"OpenDrive"`. The SDK defaults to `DefaultClientID = "OpenDrive"` and
   accepts an override via `WithClientID`.

The spec does not document the grant *response*, so the token lifetimes
(86400 s access, 30 days refresh) still come from the PDF. The SDK prefers
`expires_in` when the response carries it.

**Status:** open until a sandbox grant response is recorded as a fixture.

## D7 — listing pagination pairs `offset` with `last_request_time` {#d7}

`folder/list.json` documents `offset` and `last_request_time` (plus
`search_query`, `only_subfolders`, `with_breadcrumbs`, `sharing_id`), confirming
whitepaper §2.6 #14: paging past the first 100 entries requires echoing the
previous response's `DirUpdateTime`. There is no `limit` parameter — the page
size is fixed upstream.

Note the spelling: the list parameter is `with_breadcrumbs` and the standalone
endpoint is `folder/breadcrumb.json`; see D12.

**Resolution:** modelled by `Pagination` (`FirstPage`/`Next`/`Validate`), which
clamps the page size to 100 and refuses a later page without `last_request_time`.

## D8 — public `users/*` endpoints absent from the PDF's Users chapter {#d8}

`users/forgotpassword.json`, `users/confirmpasswordreset.json` and
`users/verifyemailsignup.json` need no authentication. Out of scope for v1.0
(§1.4); recorded so drift detection does not report them as new.

## D9 — captcha is not limited to login {#d9}

`POST /v1/file/verifypassword.json` accepts `captcha_response`, so
password-protected file access can also be throttled behind a captcha.

**Status:** open. The SDK maps any captcha response to `KindCaptchaRequired`,
which is never retried; the P2 file module must surface it on `verifypassword`
too.

## D10 — the `sharing` module is invisible to anonymous callers {#d10}

The anonymous `resources.json` advertises nine modules and no `sharing`, which
made the whitepaper's §2.3 Sharing section look wrong. With credentials the
listing grows to seventeen modules and `sharing` is there, with seven
operations:

```
POST   /v1/sharing.{format}
DELETE /v1/sharing.{format}/{session_id}/{sharing_id}
PUT    /v1/sharing/setmode.{format}
GET    /v1/sharing/listsharedfolders.{format}/{session_id}/{sharing_id}
GET    /v1/sharing/listsharedusers.{format}/{session_id}
GET    /v1/sharing/listusers.{format}/{session_id}/{folder_id}
GET    /v1/sharing/checkaccountusersaccess.{format}
```

**Resolved:** the whitepaper was right; the anonymous spec was incomplete.
`/sharing.json` is a third multi-verb resource (POST shares, DELETE revokes),
now covered by `TestSameResourceDifferentVerbs`.

## D11 — only the OAuth2 access token unlocks the full explorer spec {#d11}

`tools/fetch-spec` tries four authenticated strategies. Measured against the live
explorer on 2026-07-25:

| strategy | modules | operations |
|---|---|---|
| anonymous | 10 | 21 |
| PHP session cookie | 10 | 21 |
| `?session_id=` | 11 | 26 (5 modules unreadable) |
| session as a path segment | — | 404, the explorer has no such route |
| **`?session_id=OAUTH&access_token=`** | **17** | **220** |

**Resolved:** the OAuth2 token is the only way to see the whole surface, which
matters for the weekly drift job (§12.2) — it must run with credentials, not
anonymously, or it will report 199 operations as "removed".

## D12 — the breadcrumb endpoint is spelled correctly upstream {#d12}

*Whitepaper §2.6 #3 (from the PDF):* "官方拼写就是 breadcrump" — the endpoint is
supposedly misspelled and the misspelling must be reproduced.

*Live spec:* `GET /v1/folder/breadcrumb.{format}/{session_id}/{folder_id}`.

*Verified against the live API with the test account:*

| request | response |
|---|---|
| `GET /folder/breadcrumb.json/{session}/0` | `400 {"error":{"code":400,"message":"Invalid folder IDAA"}}` — endpoint exists, argument rejected |
| `GET /folder/breadcrump.json/{session}/0` | `404 {"error":{"code":404,"message":""}}` — no such endpoint |

**Resolution:** `EndpointFolderBreadcrumb = "/folder/breadcrumb.json"`. Upstream
evidently fixed the typo at some point after the PDF was written. The *field*
spelling trap of §2.6 #3 still stands separately (`OwnerSuspendet` in the login
response), and remains covered by `TestSessionLoginKeepsUpstreamFieldSpelling`.

**Note for the whitepaper:** §2.6 #3 should be amended — this is the one gotcha
whose premise is inverted by the live API.

## D13 — `upload/has_ddref.json` is an undocumented dedupe probe {#d13}

```
POST /v1/upload/has_ddref.{format}
  session_id : string (required)
  file_size  : int    (required) - File size in bytes
  file_hash  : string (required) - MD5 file hash (32 hex chars)
```

The PDF's upload chapter does not mention it. It looks like a direct "do you
already hold this blob?" query, i.e. the dedupe decision of §2.4 without having
to call `create_file` first.

**Status:** to be evaluated in P3. If it behaves as it reads, the upload pipeline
can skip a round trip on the hot dedupe path.

## D14 — the file resource is `/file.json`, not `/file/file.json` {#d14}

*Whitepaper §2.3* lists `/file/file.json` for both "create an empty file" (POST)
and "permanently delete from the trash" (DELETE).

*Live spec:*

```
POST   /v1/file.{format}
DELETE /v1/file.{format}/{session_id}/{file_id}
POST   /v1/file/remove.{format}      # permanent removal, body form
```

So the resource is one level up, the DELETE form takes its arguments as path
segments, and there is a separate `file/remove.json` for permanent removal. The
same shape holds for folders (`POST /v1/folder.{format}`,
`POST /v1/folder/remove.{format}`).

**Resolution:** `EndpointFile = "/file.json"`, plus `EndpointFileRemove`.
`TestEndpointConstantsMatchTheArchivedSpec` verifies every constant against the
archive, so this class of error cannot recur silently.

## D15 — three endpoints use a different verb than the whitepaper documents {#d15}

| endpoint | whitepaper §2.3 | live spec |
|---|---|---|
| `file/access.json` | PUT | **POST** |
| `file/filesettings.json` | POST | **PUT** |
| `folder/foldersettings.json` | POST | **PUT** |

The first two are exactly inverted. Since §2.6 #4 makes verb selection
load-bearing, this would have been a silent 404 or, worse, the wrong operation.

**Resolution:** the verbs are recorded next to each constant and asserted by
`TestEndpointConstantsMatchTheArchivedSpec`.

## D16 — `move` and `overwrite_if_exists` are required strings {#d16}

```
POST /v1/file/move_copy.{format}
  move                : string (required) - (true = move, false = copy)
  overwrite_if_exists : string (required) - (true, false)
```

Confirms whitepaper §2.6 #13 and adds that both are **required**, not optional.

**Resolution:** the `StringBool` type marshals to `"true"`/`"false"`; P2 must
send both fields on every move_copy call.

## D17 — `folder/list.json` has three parameters the PDF does not mention {#d17}

Beyond the documented set: `encryption_supported` (int), `order_by` (string) and
`order_type` (string). `folder/trashlist.json` additionally takes `count_only`.

**Status:** open. `order_by`/`order_type` are worth surfacing on the Bridge `/v1/ls`
endpoint in P4; `encryption_supported` probably relates to Secure Folders (v1.2).

## D18 — expiring links pass every argument as a path segment {#d18}

```
GET /v1/folder/expiringlink.{format}/{session_id}/{date}/{counter}/{folder_id}/{enable}
GET /v1/file/expiringlink.{format}/{session_id}/{date}/{counter}/{file_id}/{enable}
```

Confirms the need for per-endpoint session placement: here the session is the
*first path segment*, whereas `folder/trash.json` takes it in the body for POST
and as a path segment for DELETE.

**Resolution:** `Request.SessionPlacement` (`SessionInBody`/`SessionInQuery`/
`SessionInPath`/`SessionOmit`) plus `Request.PathSegments`, with escaping so a
segment cannot forge an extra path element.

## D19 — the same field is a string in one response and a number in another {#d19}

Real responses from the test account:

| field | `session/login.json` | `users/info.json` |
|---|---|---|
| `UserID` | `"2125533"` (string) | `2125533` (number) |
| `AccType` | `"1"` (string) | — |
| `Trial` | — | `"0"` (string boolean) |
| `UserSince` | — | `"1785027739"` (string timestamp) |
| `FVersioning` | `"0"` (string boolean) | — |
| `OwnerSuspendet`, `max_file_size` | absent | absent |

Live confirmation of §2.6 #5 and #6, and of why fields have to tolerate absence:
this account's login response omits two fields the PDF documents as always
present.

**Resolution:** `FlexString`, `FlexInt`, `FlexBool` and `UnixTime` handle all of
these; every form above is in the type tests.

## D20 — `users/info.json` returns a `PrivateKey` {#d20}

The account information response carries a `PrivateKey` field. It is not
mentioned in the PDF and it is clearly credential-shaped.

**Resolution:** added to the redaction list, so it cannot reach a debug log
(§9.4).

## D21 — six modules beyond the v1.0 scope are live {#d21}

The authenticated listing exposes, with operation counts:

| module | ops | v1.0 scope? |
|---|---|---|
| tasks | 43 | no (v1.3) |
| admin | 40 | no |
| notes | 26 | no (v1.3) |
| folder | 24 | yes |
| file | 20 | yes |
| users | 14 | read-only subset |
| accountusers | 11 | no (v1.1) |
| branding | 8 | no |
| sharing | 7 | yes |
| upload | 7 | yes |
| usergroups | 5 | no (v1.1) |
| session | 5 | yes |
| download | 3 | yes |
| securefolders | 3 | no (v1.2) |
| stats | 3 | no (v1.2) |
| oauth2 | 1 | yes |

Informational: the roadmap in §12.1 lines up with what exists, and `admin`
(40 operations) is a module the whitepaper never mentions at all.

## D22 — `folder/idbypath.json` answers with `FolderId`, not `FolderID` {#d22}

Observed against the sandbox on 2026-07-26:

```
POST /v1/folder/idbypath.json  {"session_id":"…","path":"Developing"}
→ {"FolderId":"MzNfNDg3Njg1N19WM3pvSg"}
```

Every other folder endpoint spells the field `FolderID`. The Swagger
declaration documents neither, because the response class is `void` throughout
the module.

**Resolution:** the decoder accepts both spellings and prefers the observed one,
so a future correction upstream will not break the SDK
(`TestFolderIDByPathAcceptsBothSpellings`).

## D23 — `folder/itembyname.json` reports a miss by omitting the arrays {#d23}

A hit returns `{"DirUpdateTime":…,"Folders":[…]}`; a miss returns
`{"DirUpdateTime":…}` — no error, no empty array, HTTP 200. Reading the result
naively yields "found nothing" and "the folder is empty" as the same value.

**Resolution:** the SDK maps an empty result to `KindNotFound`, so callers can
use `errors.Is(err, ErrNotFound)` as they would anywhere else.

## D24 — the two `move_copy` endpoints disagree on the type of `move` {#d24}

| endpoint | `move` | `overwrite_if_exists` / `copy_recursive` |
|---|---|---|
| `file/move_copy.json` | **string** `"true"`/`"false"`, required | `overwrite_if_exists`, string, required |
| `folder/move_copy.json` | **boolean**, optional | `copy_recursive`, boolean, optional |

Whitepaper §2.6 #13 documents the string form as though it were universal. It is
not: the folder endpoint documents real JSON booleans.

**Resolution:** the folder binding sends real booleans and the file binding will
send `StringBool`; both are asserted against the archived spec, so if upstream
ever aligns the two, the contract test says so.

## D25 — the sandbox account is read-only {#d25}

Every mutating folder call with the current test account is refused:

```
POST /v1/folder.json          → 403 "Your user access enables you only to view
                                     this folder, please contact your
                                     administrator…"
GET  /v1/folder/trashlist.json → 403 "Permission denied"
GET  /v1/folder/exportcsv.json → 403 "Export failed. Permission denied for
                                     restricted user"
```

`users/info.json` shows why: the login is an *account user*
(`AccessUserID` differs from `UserID`) with view-only rights on the one folder
it can see.

**Consequence:** the P2 exit criterion "sandbox smoke test passes" can only be
met for read operations. Every write binding is covered by unit tests against a
mock upstream and by contract tests against the archived spec, but nothing has
executed a real create, rename, move, trash or delete. An account with write
rights is needed before the P3 transfer pipeline, which cannot be validated any
other way.

**Note for classification:** these 403s are plain upstream refusals, not
credential problems. They map to `upstream_error`, which is what keeps the
silent re-login state machine from mistaking a permission problem for a changed
password (v1.1 §4.5).

## D26 — `folder/move_copy.json` rejects the documented boolean `false` {#d26}

The archived spec (and D24) describe `move` on the folder endpoint as a real
JSON boolean, unlike the file endpoint's `"true"`/`"false"` strings. The live
endpoint disagrees, and only for one of the two values:

| `move` sent | result |
|---|---|
| `false` (JSON boolean) | **400** — ``Invalid value specified for `move`. Expecting boolean value`` |
| `true` (JSON boolean) | 200, moves |
| `"false"` (string) | 200, copies |
| `"true"` (string) | 200, moves |
| `0` (integer) | 200, copies |
| `"0"` (string) | 400 |

So the documented encoding cannot express a copy at all: the boolean `false` is
refused with a message that asks for the very type it was given. Whitepaper
§2.6 #13, which called the string form universal, turns out to be right, and the
Swagger description is wrong.

**Resolution:** the folder binding sends `move` and `copy_recursive` as
`StringBool`, matching the file module. Caught by
`TestSandboxWriteCapabilities` against the live API — the mock-based unit test
had happily asserted the spec's (wrong) boolean form, which is exactly the class
of bug integration tests exist for.

Covered by `TestFolderMoveCopySendsStringBooleans`,
`TestSandboxWriteLifecycle`.

## D27 — `folder/info.json` still answers for a permanently deleted folder {#d27}

After `folder/trash.json` followed by `folder/remove.json` (a permanent
delete), `folder/info.json/{session}/{folder_id}` keeps returning the folder's
full metadata — immediately, and still five seconds later, so this is not
eventual consistency. The parent's `folder/list.json` drops the folder at once,
which is the correct view.

**Consequence:** `info.json` is not a liveness check. Anything deciding whether
a folder exists — `/v1/stat`, the path cache, upload pre-flight in P3 — must use
the parent listing or `idbypath`, never a successful `info` call.

Covered by `TestSandboxWriteLifecycle`.

## D28 — `POST /file.json` rejects an empty `access_folder_id` {#d28}

The spec marks `access_folder_id` required but says nothing about its value, and
the obvious reading — "required, so send it, empty when there is no access
folder" — fails every time:

| `access_folder_id` | result |
|---|---|
| omitted | 400 ``  `access_folder_id` is required. `` |
| `""` | 400 ``  `access_folder_id` is required. `` |
| `"0"` | **200**, file created |
| the granted base folder id | 200, file created |
| the target folder's own id | 403 "Your user access enables you only to view this folder" |

So an empty string does not satisfy a required string, and the value that works
is the root marker `"0"` — the same convention as `folder_sub_parent` (§2.6 #9).

**Resolution:** `CreateEmpty` defaults `access_folder_id` to `RootFolderID`.
Caught by the live suite; the mock test had been asserting the empty string and
passing. Covered by `TestSandboxFileAccessFolderIDIsRequired`.

Also recorded while probing: `file_type` is an extension, not a MIME type.
`"txt"` produces a file called `Text.txt`, and a second call in the same folder
produces `Text (1).txt`.

## D29 — `DELETE /file.json` does not require the file to be trashed {#d29}

The PDF presents `DELETE /file.json/{session}/{file_id}` as the way to remove a
file *from the trash*. The sandbox deletes a live file just as readily, with no
trash step and no warning, returning `{"DirUpdateTime":…}` either way.

**Consequence:** the trash is not a safety net for this call. The Bridge's
`/v1/rm` keeps `permanent: false` as its default (§4.2) and only reaches this
endpoint when the caller asks for a permanent delete.

Covered by `TestSandboxFileDeleteWithoutTrash`.

## D30 — `file/filefullpath.json` answers in `DownloadLink`, with backslashes {#d30}

```
GET /v1/file/filefullpath.json/{session}/{file_id}
→ {"DownloadLink":"Application\\odb-f3-1785122522\\Text.txt"}
```

Two surprises in one small body: the field is called `DownloadLink` even though
it holds a path rather than a URL, and the separator is a backslash, while the
folder module's `folder/path.json` returns forward slashes. A binding that reads
`FullPath` — the name the field has in the folder module — silently returns the
empty string.

**Resolution:** the binding reads `DownloadLink`, still accepts `FullPath` in
case upstream corrects it, and normalises the separators. Covered by
`TestFileFullPathNormalisesTheRecordedShape` and `TestSandboxFileLifecycle`.

## D31 — the expiring-link endpoints return one object, not an array {#d31}

`folder/folderexpiringlinks.json` and `file/fileexpiringlinks.json` are plural
and the PDF describes them as lists. Both return a single JSON object, so a
binding that decodes into a slice fails with "cannot unmarshal object into Go
value of type []…".

The two modules also disagree on the field names:

| | create | list |
|---|---|---|
| folder | `{"Link":…}` | `{"Link":…,"CounterMax":"5","CounterEnable":"0","ExpiringDate":"2026-12-31","Counter":"0"}` |
| file | `{"DownloadLink":…,"StreamingLink":…}` | the same plus the counter fields |

`ExpiringDate` is a calendar date string, not the Unix timestamp the shared
model originally assumed, and the counters arrive quoted.

**Resolution:** one `ExpiringLink` type covering both shapes with a `URL()`
accessor, and both `ExpiringLinks` methods return a single value. This was a
latent bug in the already-merged folder module, not only in the new file one.
Covered by `TestFolderExpiringLinks`, `TestFileExpiringLinksAndLocalValidation`
and `TestSandboxFileExpiringLink`.

## D32 — `file/verifypassword.json` answers false for a correct password {#d32}

After setting a password through `file/filesettings.json`, verifying it returns
the same body as verifying a wrong one:

```
POST /v1/file/verifypassword.json {"file_id":…,"password":"<correct>"} → {"result":false}
POST /v1/file/verifypassword.json {"file_id":…,"password":"wrong"}     → {"result":false}
```

No `TempKey` is returned in either case, so the documented flow — verify a
password, receive a temporary key, pass it to `download/file.json` or
`file/thumb.json` — cannot be completed as described.

**Status:** open, and it blocks password-protected downloads in P3. The binding
reports exactly what upstream sends and its doc comment warns that a false
result is not evidence of a wrong password. Possible explanations still to rule
out: the password may need to be set through a different endpoint, or the
verification may only work on the public share route rather than the API one.

Covered by `TestSandboxFileVerifyPassword`, which logs loudly if upstream ever
starts distinguishing the two.
