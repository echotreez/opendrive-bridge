# Upstream discrepancies — PDF vs live Swagger spec

Per `CLAUDE.md` rule 3 and whitepaper §2.6/§12.2, the live Swagger specification
(`https://dev.opendrive.com/api/v1/resources/*.json`) and real sandbox responses
take precedence over OpenDrive's REST API Guide, which is not in this repository
(see `official-api-reference.md`). Every divergence found while
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
| [D4](#d4) | **closed**, none exposed | undocumented `download/file.json` parameters |
| [D5](#d5) | open, P2 | undocumented `file/thumb.json` parameters |
| [D6](#d6) | open, P1 note | `oauth2/grant.json` grant types and `client_id` semantics |
| [D7](#d7) | confirmed, implemented | listing pagination parameters |
| [D8](#d8) | open, out of scope | public `users/*` endpoints absent from the PDF |
| [D9](#d9) | open, P2 | captcha applies beyond login |
| [D10](#d10) | resolved | the `sharing` module is invisible to anonymous callers |
| [D11](#d11) | resolved | only the OAuth2 token unlocks the full explorer spec |
| [D12](#d12) | **PDF is wrong**, implemented | the breadcrumb endpoint is spelled correctly upstream |
| [D13](#d13) | new endpoint, implemented | `upload/has_ddref.json` is a dedupe probe |
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
| [D32](#d32) | **closed**, P4 route settled | `file/verifypassword.json` is inert; the password gate lives in the web front end |
| [D33](#d33) | blocked on an owner account | the whole sharing module is closed to an account user |
| [D34](#d34) | new endpoint, implemented | `users/userlogscursor.json` is keyset paging the PDF omits |
| [D35](#d35) | **spec is wrong**, implemented | `TotalWritten` counts the chunk, not the running total |
| [D36](#d36) | **spec is wrong**, implemented | a deduplicated upload must close *without* `temp_location` |
| [D37](#d37) | confirmed, implemented | a wrong chunk offset is refused with the authoritative resume point |
| [D38](#d38) | **OAuth2 not accepted**, implemented | the chunk upload endpoint requires a real session id |
| [D39](#d39) | **re-diagnosed twice**, handled | writes are refused intermittently with a permission error |
| [D40](#d40) | **measured**, implemented | a real permission denial and a transient one are byte-identical |
| [D41](#d41) | **spec is wrong**, implemented | the download `offset` is off by one at the last byte; `Range` is not |
| [D42](#d42) | **live API is broken**, worked around | `download/all.json` answers a file list with an empty archive |
| [D43](#d43) | confirmed, implemented | `filesettings` ignores an unknown parameter and answers 200 |
| [D44](#d44) | confirmed, handled | a just-uploaded file is not downloadable straight away |
| [D45](#d45) | **measured**, normalised at the Bridge | `users/info.json` mixes bytes and megabytes in one object |

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

**Closed 2026-07-29, none of them exposed.** `temp_key` and `temp_auth` are the
password path, and D32 establishes that nothing produces a value for either:
`verifypassword` never issues a `TempKey`, and the password gate lives in the web
front end. `app` and `preview` are marked "Internal." in the live spec and have no
observable effect on an owner download. Binding a parameter we cannot obtain a
value for, or demonstrate an effect from, would be guessing.

`temp_auth` and `temp_key` remain on the redaction list regardless (§9.4), so a
value arriving from somewhere unforeseen still cannot reach a log.

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

**Resolved 2026-07-27.** The two explanations left open above were tested and
both ruled out, and the real behaviour turns out not to block P3 at all.

What the password actually does — measured on a file with a password set through
`filesettings.json`, made public through `file/access.json`:

| request | result |
|---|---|
| `GET /download/file.json/{id}?session_id=…&test=1` | `{"result":true,"dl_stream_status":true}` — downloadable |
| `GET /download/file.json/{id}?test=1` (no session) | **403**, refused |
| `file/info.json` for the owner | `"Password":"*"` — set, and masked |
| `verifypassword` with the correct password, with or without a session, on a public or private file, with the password set before or together with `file_ispublic` | `{"result":false}` every time |

So the password gates the **anonymous public route only**. An authenticated
owner is never asked for it: the session already proves the right to the bytes.

The refusal wording depends on which gate upstream reaches first — "File
requires password" when the file is otherwise downloadable, "Download
permissions are not enabled for this file" when public download is off — so
callers must key off the status, not the message.
`verifypassword.json` appears to be inert on the API route — it never returns
`true` and never returns a `TempKey`, so the documented "verify, receive a
temporary key, pass it to download" flow cannot be performed and, for one's own
files, does not need to be.

**Consequence for P3:** none. The transfer pipeline downloads as the
authenticated owner, where the password is not a gate. `temp_key` and
`temp_auth` stay out of the download binding until there is a case that needs
them, which would be downloading *somebody else's* password-protected share —
outside v1.0 scope (§1.4), and unreachable with the current account anyway
(D33).

**Closed 2026-07-29.** The public share route was tested, as the note below
predicted it would have to be, and the answer is definite: **there is no API
route by which a password-protected file can be fetched by anyone other than its
owner.**

What the public route actually is, measured on a file made public with a password
set through `file_password` (D43 — the earlier probe used `password` and silently
set nothing, which is what made this take a third round):

| request | result |
|---|---|
| `GET /download/file.json/{id}` anonymously | `403 {"error":{"code":403,"message":"File requires password"}}` |
| `GET https://od.lk/f/{id}` | `200 text/html`, a page titled "secret.txt - OpenDrive" containing an `<input name="file_password">` |
| `GET https://od.lk/d/{id}/{name}` | the same HTML page, not the bytes |
| the same with `?password=`, `?pass=`, `?p=`, `?temp_key=` | the HTML page again; the query is ignored |
| `POST file_password=` to `od.lk/f/{id}` | `200 text/html`, a different page — it responds, but in HTML, and the bytes never arrive without the session cookie the page sets |
| `verifypassword` with the correct password, with or without a session, with or without `sharing_id` or `captcha_response` | `{"result":false}`, and **no `tempkey` field at all** |

Two further notes on `verifypassword`: it insists on `password` and rejects
`file_password` with `400 "`password` is required."`, so the spelling asymmetry
with `filesettings` is real; and on a file with *no* password it answers
`{"result":true,"tempkey":null}` for **any** password including a wrong one. So
it never validates anything in either direction — it reports whether a password
exists, inverted, and never issues the `TempKey` the documented flow needs.

**The conclusion, in one line:** the password gate lives in the `od.lk` web front
end as an HTML form with a cookie, not in the API.

**What this means for P4, so `/v1/download/stream` does not need rework:**

1. **The bridge's own downloads are unaffected.** An authenticated owner is never
   asked for the password — verified live on a file with one set. Every transfer
   in v1.0 scope is an owner transfer, so `/v1/download/stream` can be designed
   without a password path at all.
2. **Setting a password is supported; consuming one is not.** `filesettings`
   with `file_password` works, so the bridge can expose password protection as a
   sharing feature. It simply cannot fetch somebody else's protected file.
3. **The alternative, when the case arrives:** drive the `od.lk` form with a
   cookie jar — an HTTP-client flow against the web front end, not the API. That
   does not belong in `pkg/opendrive`, which speaks the API; it would be a
   separate, clearly-labelled component, and it is out of v1.0 scope (§1.4).
4. **What P4 must do instead:** surface the `403 "File requires password"` as its
   own actionable error rather than a generic refusal, so a user pointed at
   somebody else's protected share is told why rather than shown "forbidden".
   `temp_key` and `temp_auth` stay out of the binding: no route produces a value
   for them.

Covered by `TestSandboxFileVerifyPassword`, which logs loudly if upstream ever
starts distinguishing the two, and by `TestSandboxPasswordGatesThePublicRoute`,
which asserts the owner/anonymous split that makes this a non-issue.

## D33 — the whole sharing module is closed to an account user {#d33}

Every operation in the sharing module — the three listings included — answers
the same way for the test account:

```
GET  /v1/sharing/listsharedusers.json/{session}          → 403 "Account users cannot list shared users"
GET  /v1/sharing/listusers.json/{session}/{folder_id}    → 403 (same message)
GET  /v1/sharing/listsharedfolders.json/{session}/{id}   → 403 (same message)
GET  /v1/sharing/checkaccountusersaccess.json            → 403 (same message)
POST /v1/sharing.json                                    → 403 (same message)
PUT  /v1/sharing/setmode.json                            → 403 (same message)
DELETE /v1/sharing.json/{session}/{sharing_id}           → 403 (same message)
```

The message is the same for all seven regardless of what was asked, including
the write operations, so it is an account-type gate rather than a per-call
permission check. `users/info.json` shows why: `AccessUserID` (60516) differs
from `UserID` (2125533), i.e. the login is an *account user* under the account
owner, and account users are not permitted to share.

This is a different limitation from D25: that one was a per-folder write
permission and has since been granted; this one is a property of the login
itself and cannot be granted from inside the account.

**Consequence:** the sharing bindings are covered by unit and contract tests
only. No sharing response shape in `testdata/fixtures/sharing/` is recorded —
they are constructed from the Swagger declaration and flagged as such — and no
sharing call has ever succeeded against a real server.

**What would settle it:** an *account owner* login. `TestSandboxSharing` checks
`AccountInfo.IsAccountUser()` first: with an account user it asserts the 403
above and stops, and with an owner login it runs the full share, setmode,
list, revoke cycle. So supplying owner credentials is the only step needed to
turn the constructed fixtures into recorded ones.

**Classification note:** the 403 maps to `upstream_error`, not to
`reauth_required`. Mapping a permission refusal onto a credential error would
send the silent re-login state machine chasing a password that was never the
problem (v1.1 §4.5).

## D34 — `users/userlogscursor.json` is keyset paging the PDF omits {#d34}

The PDF documents `users/userlogs.json`, which pages by number and reports
`TotalPages`/`CurrentPage`. The live API also has:

```
GET /v1/users/userlogscursor.json/{session_id}?cursor=…
→ {"NextCursor":"WzE3ODUxMjI5NzEsIjYwNTE2Iiw5MDIyXQ","Logs":[…]}
```

Its own description explains the point: *"Returns one page of activity logs,
newest-first, using keyset (cursor) pagination."* An empty `NextCursor` means
the end.

This matters because the numbered variant pages over a log that is still being
appended to, so entries shift between pages as new ones arrive — the same class
of problem as `folder/list.json` without `last_request_time` (§2.6 #14). The
cursor endpoint is the correct one for anything longer than a glance.

**Resolution:** both are bound — `Logs` for the numbered form and `LogsCursor`
for the keyset form — with the doc comment steering callers to the latter.
Covered by `TestUsersLogsCursorPagesByKeyset` and `TestSandboxUserLogs`.

## D35 — `TotalWritten` counts the chunk, not the running total {#d35}

Whitepaper §2.4 says to check `TotalWritten` against "本地已发送字节数" — the
bytes sent so far — which reads as a running total. It is not. Uploading 1280
bytes in three chunks:

| chunk | bytes sent | `TotalWritten` |
|---|---|---|
| 1 | 426 | 426 |
| 2 | 426 | **426** |
| 3 | 428 | **428** |

A cumulative reading would have expected 426 / 852 / 1280. The server does keep
a running total — it appears in the offset-mismatch message of D37 — but the
per-chunk reply reports only that chunk.

**Consequence:** verifying it as a cumulative counter fails on the second chunk
of every multi-chunk upload, and verifying nothing lets a short write through
silently.

**Resolution:** the pipeline compares `TotalWritten` with the length of the
chunk it just sent. Covered by `TestUploadRejectsAShortChunkWrite` and
`TestSandboxUploadCrossesChunkBoundary`.

## D36 — a deduplicated upload must close *without* `temp_location` {#d36}

When `create_file` answers `RequireHashOnly: 1`, §2.4 says to skip straight to
`close_file_upload`. It does not say what to send, and three of the four
plausible readings fail:

| close call | result |
|---|---|
| with the `TempLocation` from `create_file` | 400 "Invalid upload file size. Total uploaded=0. File size=1280" |
| after `open_file_upload`, with its `TempLocation` | 400, same message |
| after sending a zero-length chunk | 400, same message |
| **with no `temp_location` at all** | **200**, file created with the right size and hash |

So the deduplicated close is the one call in the pipeline that must omit a
parameter it otherwise always sends.

**Resolution:** the dedupe branch closes without `temp_location` and without
`file_compressed`. Covered by `TestUploadDeduplicates` (which asserts the field
is absent) and `TestSandboxUploadDeduplicates`.

## D37 — a wrong chunk offset is refused with the authoritative resume point {#d37}

```
POST /upload/upload_file_chunk2.json/{session}/{file_id}?chunk_offset=999999
→ 400 {"error":{"code":400,"message":"Incorrect chunk offset: uploaded=0, chunk_offset=999999"}}
```

The refusal carries `uploaded=N`: exactly how many bytes upstream holds. That is
the resume mechanism §2.4 asks for, and it is more useful than the per-chunk
`TotalWritten`, because it survives a lost reply or a crash.

**Resolution:** `sendChunkResuming` parses it and does one of three things —
skip a chunk that already landed, re-send only the tail when a partial write
landed (possible because the chunk is still buffered), or fail with a clear
message when the offset is behind the current chunk and the source cannot
rewind. Covered by `TestUploadResumesFromTheServerOffset`,
`TestUploadResendsTheTailAfterAPartialWrite`,
`TestUploadFailsWhenTheResumeOffsetIsUnreachable` and
`TestSandboxUploadRejectsAWrongOffset`.

## D38 — the chunk upload endpoint does not accept OAuth2 {#d38}

Every other endpoint takes `session_id=OAUTH` plus `access_token` in the query
(§2.2 B). `upload_file_chunk2.json` does not — and it does not answer like the
API at all:

| path segment | query | result |
|---|---|---|
| `OAUTH` | `access_token=…` | **401**, an nginx HTML page from openresty |
| `OAUTH` | none | 401, same HTML |
| the access token itself | none | 401, same HTML |
| **a real session id** | none | **200** `{"TotalWritten":…}` |

The HTML body gives it away: the chunk upload is served by a different front end
that authenticates on the session id in the URL and never consults the token.
`create_file`, `open_file_upload` and `close_file_upload` are all fine with
OAuth2 — only the chunk transfer is affected.

**Consequence:** an OAuth2 bridge cannot upload without also holding a session.

**Resolution:** `Request.NeedsSessionID` marks such an endpoint, and the
`SessionIDProvider` interface supplies one. In OAuth2 mode `OAuth2.SessionID`
mints a session with the stored password and caches it in the credential store,
so the user is never involved — which is precisely what persisting the password
was for (§2.2 #1). With `persist_password: false` the upload reports
`reauth_required` with an explanation rather than a bare 401.

Also worth noting: because this front end is not the API, its errors are not the
API's either. A bare HTML 401 classifies as `token_expired`, which would send the
auth state machine chasing a token that was never the problem — another reason
to send the right credential in the first place.

## D39 — writes are refused intermittently with a permission error {#d39}

Integration runs failed in a pattern that looked random: some `create_file` or
`file.json` calls answered `403 "Your user access enables you only to view this
folder"` while others in the same run succeeded, and every call succeeded when
replayed by hand a moment later. A different test failed on each run.

**First diagnosis (wrong).** The login burst was blamed, and sharing one login
across the test binary seemed to fix it. It did not — the suite kept failing,
and the same `-run TestSandbox` selection that passed in the morning failed
consistently by the evening.

**Actual cause: leaked test artefacts.** The capability probe created a copy
(`<name>-copy`) and never removed it, so one folder leaked per run. Once about a
dozen had accumulated in the base folder, writes into that folder began failing
with the misleading permission message. Deleting the 16 leftovers made the suite
pass again immediately, and it has passed on every run since the probe learned
to clean up after itself.

Ruled out along the way, each tested in isolation against the live account:

| hypothesis | test | result |
|---|---|---|
| write rate limit | 14 rapid `create_file` | all 200 |
| login burst | 6 logins, then writes | all 200 |
| request volume | 120 mixed requests | all 200 |
| new-folder permission lag | write into a just-created folder, 0–2 s delay | all 200 |
| session/OAuth interference | session upload, then OAuth writes | all 200 |
| accumulated artefacts | delete 16 leftover folders, rerun | **suite goes green** |

**Second revision, 2026-07-29 — the artefact explanation was also wrong.**

Implementing the recommendation below meant reproducing the condition on demand.
It could not be reproduced, and the accumulation theory does not survive
measurement either. Every hypothesis was tested in isolation against the live
account, roughly 500 operations in total:

| hypothesis | test | result |
|---|---|---|
| accumulated artefacts | 60 files created one after another in a single folder | all 200; no degradation |
| leftover folders | the one leaked folder deleted, then writes retried | writes had already been succeeding |
| operation volume | 300 create/trash/remove operations in 117 s | 10 sporadic `trash` refusals, no sustained failure |
| new-folder permission lag | create a subfolder, then write into it immediately, 6 trials | first write succeeded after 0.4 s every time |
| a refused write poisoning the account | 3 refused writes at the account root, then a write into a granted folder | succeeded 1 s later |
| auth mode | 30 create/trash rounds on OAuth2, 30 on a session | 3 refusals versus 0 — the same order, within noise |

What is left, and what the numbers actually show: **upstream refuses some write
calls at random, with this message, and then accepts the identical call moments
later.** In the 300-operation run, `folder/trash.json` was refused 10 times out
of 100 with no pattern in time or in the state of the folder; `create_file` and
`POST /file.json` show the same behaviour, occasionally in bursts that last tens
of seconds. Nothing on the client side triggers it and nothing on the client
side clears it.

The clean-up rule from the first revision stands on its own merits — a leaked
artefact is a defect regardless — but it was not the cause, and "delete the
leftovers and it goes green" was the flakiness resolving on its own.

**Consequences.**

1. Integration tests must still remove *every* artefact, including intermediate
   ones like the copy a probe makes.
2. The refusal cannot be told apart from a genuine denial by reading it: D40
   measures the two side by side and the messages are byte-identical. It is
   therefore handled by the classification layer
   (`pkg/opendrive/classify.go`, `docs/error-taxonomy.md` T2) rather than by
   any rule about folder contents: a permission-shaped 403 is retried when the
   credential is verified working and the same call has already succeeded on
   the same resource, and reported — with `APIError.Diagnosis()` rather than
   upstream's wording — when it has not.
3. Because a refusal means upstream declined to act, replaying it is safe
   whatever the HTTP method, so writes are retried too
   (`APIError.replaySafe`). Without that the one condition retrying exists for
   would be the one condition a POST sat out.
4. The live suite runs with a more patient retry policy than the SDK default,
   which is the harness accommodating a flaky sandbox rather than a workaround
   in the product.

Covered by `TestSandboxWriteCapabilities`, `TestSandboxAmbiguousPermissionRefusal`.

## D40 — a real permission denial and a transient one are byte-identical {#d40}

D39 ended with a recommendation: treat a 403 carrying the "only to view this
folder" message as retryable rather than fatal. Measuring it before implementing
it showed the recommendation cannot be carried out as written.

Measured on 2026-07-29 with the current account, which has write rights inside
its granted folders and none at the account root:

| request | result |
|---|---|
| `POST /folder.json` with `folder_sub_parent` = the granted base folder | `200`, created |
| `POST /folder.json` with `folder_sub_parent: "0"` | `403 "Your user access enables you only to view this folder, please contact your administrator to discuss your user permissions."` |

That refusal is the same string, byte for byte, that D39 recorded for a folder
the account *could* write to and that recovered the moment the leaked artefacts
were deleted. The message therefore distinguishes nothing: reading it and
retrying would hammer a genuine denial, and reading it and giving up would report
resource pressure as "contact your administrator".

Three further shapes were pinned down at the same time, because the classifier
needs to know what an API answer looks like before it can tell one apart from
something else:

| request | answer |
|---|---|
| `POST /upload/upload_file_chunk2.json/OAUTH/xyz?access_token=…` | `401`, `Content-Type: text/html`, an openresty page (D38) |
| the same path with a real session id | `400 {"error":{"code":400,"message":"`temp_location` is required."}}` — the API, in JSON |
| `GET /nosuchmodule/nope.json/{session}` | `404 {"error":{"code":404,"message":""}}` — a route that does not exist is still answered *by the API* |
| `GET /users/info.json/notasession` | `401 {"error":{"code":401,"message":"Session does not exist, please re-login."}}` |

So "the body is not JSON" is a reliable signal that the request never reached the
API, and it is the only signal that separates D38's 401 from a real one.

**Resolution:** `pkg/opendrive/classify.go`, specified by
`docs/error-taxonomy.md`. The ambiguous 403 is settled with gathered evidence
rather than read off the response — one rate-limited `users/info.json` probe to
establish that the credential still works, plus whether this same operation has
already succeeded on this client. Both branches stay `upstream_error`; they
differ in retryability and in the wording the user is shown
(`APIError.Diagnosis()`).

Also recorded: `folder/exportcsv.json` now succeeds for this account and answers
`200` with `Content-Type: text/csv` — D25 listed it as a 403, so the account's
rights have widened since. A non-JSON body on a *successful* response is normal
and is not what T1 is about; classification only ever looks at failures.

Covered by `TestTaxonomyFormsClassify`,
`TestAmbiguous403WithAWorkingCredentialAndAWitnessIsTransient`,
`TestAmbiguous403WithoutAWitnessIsPermanent` and
`TestSandboxAmbiguousPermissionRefusal`.

## D41 — the download `offset` is off by one at the last byte {#d41}

`download/file.json` documents an `offset` query parameter for resuming. It works
— until the very end of the file. Measured on a 9000 byte file on 2026-07-29,
with a binary search for the boundary:

| `offset` | status | bytes returned |
|---|---|---|
| 0 | 200 | 9000 (the whole file, correctly) |
| 1 | 206 | 8999 |
| 4000 | 206 | 5000 |
| **8998** | **206** | **2** |
| **8999** | **200** | **9000 — the whole file again** |
| 9000 | 200 | 9000 |
| 20000 | 200 | 9000 |

So the largest honoured offset is `size - 2`. Asking for the final byte, which is
a perfectly ordinary resume request, silently answers with the entire file. The
`Range` header has no such problem — `Range: bytes=8999-` returns `206` with
`Content-Range: bytes 8999-8999/9000` and one byte.

**Why this is dangerous rather than merely odd:** a resume appends to a partial
file. A pipeline that sends `offset=N`, gets 200, and appends the body produces a
file that is *longer* than the original with its opening bytes repeated in the
middle — and no error anywhere. The corruption is silent and survives until
someone checks a hash.

**Resolution:** the pipeline sends `Range` rather than `offset`, and it treats
**the status as the only honest signal**: `206` means the resume point was
honoured, anything else means the body starts at zero regardless of what was
asked. When upstream ignores the range the leading bytes are skipped from the
stream instead of being written, so the destination is correct either way and
`DownloadResult.Resumed` reports which happened. The MD5 is taken from the raw
stream before the skip, so a restarted transfer can still be verified end to end.

Covered by `TestDownloadSkipsForwardWhenUpstreamIgnoresTheRange`,
`TestDownloadFileResumesOnDisk`, `TestSandboxDownloadOffsetBoundary`.

## D42 — `download/all.json` answers a file list with an empty archive {#d42}

The endpoint takes `files` and `folders`. Only `folders` produces anything:

| request | result |
|---|---|
| `{"files": "<file id>"}` | **200**, `application/zip`, 98 bytes — a structurally valid ZIP with **no entries** |
| `{"files": "<id>,<id>"}` | 200, the same empty 98 byte archive |
| `{"files": ["<id>"]}` | 400 ``Invalid value specified for `files`. Expecting alpha numeric value`` |
| `{"files": "<numeric id>"}` | 400 `Invalid File ID` |
| `{"folders": "<folder id>"}` | **200**, a real archive containing `<folder>/one.txt`, `<folder>/two.txt` |
| `{}` | 400 `Empty files list` |

The file id is accepted — the array form is rejected specifically for not being a
string, so it is parsed — and then produces nothing. There is no error to notice.

This is the download-side twin of D23 and D27: a success that lies. A "download
these files as a zip" feature built on it would hand users an empty archive and
report success.

**Resolution:** `DownloadService.Archive` takes folder ids only and does not
offer a files parameter, because an unusable parameter that answers 200 is worse
than an absent one. Downloading several files individually is what
`DownloadService.Download` is for. Covered by `TestArchiveSendsFoldersOnly`,
`TestSandboxDownloadArchive`.

Recorded at the same time: **D1 is settled.** `download/all.json` sent
`session_key` — the name the PDF gives it — answers `403 "File is private"`,
i.e. it ignores the parameter and falls back to anonymous. The live spec's
`session_id` is correct and the PDF is wrong. Whitepaper §2.6 #2 should be
amended.

## D43 — `filesettings` ignores an unknown parameter and answers 200 {#d43}

Setting a file password with `{"password": "..."}` returns `200` and a full file
object, and does nothing at all: `file/info.json` still reports `Password: ""`
and the file stays anonymously downloadable. The parameter is `file_password`;
with that name the same call works and `info` reports `Password: "*"`.

Half a debugging round went into this while settling D32, because the 200 and the
returned object both look exactly like success.

**Consequence:** upstream does not validate parameter *names*, only values. A
misspelled field is silently discarded. This is why a binding is not finished
until a live probe confirms the effect actually happened — checking that the call
returned 200 confirms nothing (`CLAUDE.md` rule 2).

The SDK sends `file_password`, and `TestSandboxPasswordDoesNotGateTheOwner`
asserts through `file/info.json` that the password really took, rather than
trusting the 200.

## D44 — a just-uploaded file is not downloadable straight away {#d44}

`close_file_upload.json` returns 200 with the file's full metadata, and for a
short window afterwards the download endpoint does not know about it:

```
POST /upload/close_file_upload.json  → 200 {"FileId":"…","Size":65536,"FileHash":"…"}
GET  /download/file.json/{that id}   → 404 {"error":{"code":404,"message":"File does not exist"}}
```

Seen once in a live suite run and not reproducible on demand, which is what makes
it worth writing down: read-after-write on this API is not guaranteed, so any
code that uploads and then immediately reads has to tolerate a brief `not_found`.
The counterpart is D27 for folders and the deleted-file window recorded in
`TestSandboxDownloadOfADeletedFile`, where visibility lags in the other
direction.

**Resolution — deliberately not in the SDK.** A 404 means "not found", and
teaching the classifier to retry it would mask genuine missing files, which is
the one thing `docs/error-taxonomy.md` exists to prevent. Instead:

- the integration fixtures wait for the cheap `test=1` probe to succeed before
  downloading (`waitUntilDownloadable`);
- P3's job engine must treat "verify immediately after upload" as a step that may
  legitimately need a short wait, not as a failed transfer.

Covered by `TestSandboxDownloadRoundTrip` and every other download fixture,
through `waitUntilDownloadable`.

## D45 — `users/info.json` reports usage in bytes and limits in megabytes {#d45}

The same object carries both, with nothing to say so:

```
StorageUsed  "188726232"     BwUsed  "6901021"
MaxStorage   "1048576"       BwMax   "10240"
UserPlan     "OpenDrive Unlimited Personal Plan - Year"
```

Read as one unit, the account is 180 times over a one-megabyte quota — on an
unlimited plan. Read correctly, `MaxStorage` is 1048576 **MB** (exactly 1 TiB)
and `BwMax` is 10240 MB (exactly 10 GiB), which are ordinary plan sizes, while
the two *used* figures are byte counts of about 180 MB and 6.6 MB.

**Status: inferred, not stated.** Upstream documents no unit for any of the four.
The evidence is that the limits are exact binary round numbers while the usages
are not, that the ratio is precisely 2^20, and that the alternative reading
contradicts the plan name on the same response. One account was available to
measure, so this is a strong inference rather than a proof, and it is written
down here so it can be re-checked against a second account.

**Consequence, and why it is the Bridge's problem:** a client showing
"188 MB of 1 MB used" is worse than showing nothing. `/v1/auth/status` therefore
converts both limits to bytes so that every field in `quota` means the same
thing. The conversion is one named constant (`bytesPerMB` in
`internal/server/auth.go`) so that a future measurement can change it in one
place.

Covered by `TestQuotaLimitsAreNormalisedToBytes`.

---

## D46 — upstream trims whitespace off the ends of a name, silently

**Where:** `POST /folder.json` (and every endpoint that takes a name).
**Found by:** a fuzzer, indirectly. `FuzzNormalizeFolderPath` reported that
normalising was not idempotent — `"0 /"` became `"/0 "`, and normalising *that*
gave `"/0"`. Chasing why led to the question of what a trailing space in a name
even means to upstream, which nobody had asked.

**Measured on the live API**, creating folders and reading the names back out of
the parent listing:

| sent | stored |
|---|---|
| `"odbspace1 "` | `"odbspace1"` |
| `" odbspace2"` | `"odbspace2"` |
| `"odbspace3  x"` | `"odbspace3  x"` |

Surrounding whitespace is removed; interior whitespace is kept. Upstream reports
success and returns a `FolderID` either way, so nothing in the response says the
name was changed — the only way to see it is to read the name back.

**Consequences, both of which were live bugs:**

1. A path asking for `"/a "` could never match anything, because upstream cannot
   store a folder called `"a "`. The bridge would have resolved it to "no such
   folder" while the user was looking straight at the folder in the web client.
   Both `NormalizeFolderPath` and the Bridge's `normalisePath` now trim each
   **segment**, which is what upstream does.

2. Trimming has to happen *before* the traversal check. `" .. "` is not `".."`
   to a string comparison, but it is to upstream, which trims the name first.
   The check ran first and let it through as an ordinary segment name, so a
   request would have gone out for a name upstream reads as `..`. Now the
   segments are trimmed first and the check sees what upstream would see.

The earlier code trimmed the path *as a whole* instead, which produced (1) and
(2) and additionally made normalisation non-idempotent: the trailing space came
off the last name whenever the path had no trailing slash, so two spellings of
one path resolved to two different folders and the answer changed the second
time it was asked.

3. The same disguise works one layer lower. `ValidateName` rejected `"."` and
   `".."` by exact comparison, so `". "` was accepted — and upstream would have
   stored it as `"."`. Fuzzing the Bridge's `joinPath` found it: a rename to
   `". "` produced the response path `"/."`, which is the parent, not the new
   child. `ValidateName` now compares the trimmed form. Names that merely
   contain dots (`.hidden`, `report.pdf`) are unaffected.

4. `joinPath` in the Bridge trims the name it is given, for the same reason: a
   listing entry called `" report"` describes a folder upstream knows as
   `"report"`, and a path built from the padded form would be a cache key no
   lookup could ever match.

Covered by `TestNormalizeFolderPathTrimsEachSegmentNotThePath`,
`TestNormalizeFolderPathRefusesPaddedTraversal`,
`TestValidateNameRejectsPaddedDotNames`, and the fuzz corpus entry that found
the first of them.
