# Upstream discrepancies — PDF vs live Swagger spec

Per `CLAUDE.md` rule 3 and whitepaper §2.6/§12.2, the live Swagger specification
(`https://dev.opendrive.com/api/v1/resources/*.json`) and real sandbox responses
take precedence over `docs/OpenDrive_API_guide.pdf`. Every divergence found while
implementing the Bridge is recorded here.

**Baseline:** `testdata/spec/`, produced by `tools/fetch-spec`.
See `testdata/spec/manifest.json` for the fetch timestamp, the auth strategy used
and per-file SHA256 checksums.

| ID | Status | Area |
|----|--------|------|
| [D1](#d1) | live spec wins, implemented | `download/all.json` session parameter |
| [D2](#d2) | confirmed, implemented | `session/captcharequired.json` missing from the PDF |
| [D3](#d3) | confirmed, implemented | `/v1` appears in both base path and endpoint paths |
| [D4](#d4) | open, P3 | undocumented `download/file.json` parameters |
| [D5](#d5) | open, P2 | undocumented `file/thumb.json` parameters |
| [D6](#d6) | open, P1 note | `oauth2/grant.json` grant types and `client_id` semantics |
| [D7](#d7) | confirmed, implemented | listing pagination parameters |
| [D8](#d8) | open, out of scope | public `users/*` endpoints absent from the PDF |
| [D9](#d9) | open, P2 | captcha applies beyond login |
| [D10](#d10) | open | no `sharing` module in the resource listing |
| [D11](#d11) | blocked | archive currently covers the public subset only |

---

## D1 — `download/all.json` calls its session parameter `session_id`, not `session_key` {#d1}

*Whitepaper §2.6 #2 (from the PDF)* states that `POST /download/all.json` is the
one endpoint that names the session parameter `session_key`.

*Live spec* (`testdata/spec/download.json`, `POST /v1/download/all.{format}`)
documents the request body as:

> `files` : string — Comma-separated files Ids.
> `folders` : string — Comma-separated folder IDs.
> **`session_id`** : string — Session ID.

**Resolution:** follow the live spec — the SDK sends `session_id` by default.
`Request.SessionParam` exists precisely so a single endpoint can override the
name without a special case in the client, so if the sandbox turns out to accept
only `session_key`, the fix is one field on one call.

**Follow-up:** exercise the real endpoint during P3 (download pipeline) and
record the sandbox's actual behaviour here.

Covered by `TestArchivedSpecDocumentsTheDownloadAllSessionParameter`,
`TestSessionParamOverride`.

## D2 — `session/captcharequired.json` exists online but not in the PDF {#d2}

*Live spec* (`testdata/spec/session.json`):
`GET /v1/session/captcharequired.{format}`, one optional query parameter
`username`, described as:

> Lets the login page render the captcha on first load when the client's IP is
> already throttled, instead of only after a failed submission.

This is the concrete instance of whitepaper §2.6 #1 (the PDF, self-described as
v1.1.7, lags the live surface).

**Resolution:** implemented as `opendrive.CaptchaRequired(ctx, client, username)`
with the `CaptchaStatus` model. Covered by `TestCaptchaRequiredEndpoint` and
`TestArchivedSpecHasEndpointsThePDFOmits`.

## D3 — `basePath` stops at `/api` while every declaration path starts with `/v1` {#d3}

*Live spec:* every module declares `"basePath": "https://dev.opendrive.com/api"`
and paths such as `/v1/session/login.{format}`. The whitepaper's base URL
(§2.1) is `https://dev.opendrive.com/api/v1`, so naively concatenating a path
taken from the spec (or from the PDF's expiring-link section, §2.6 #12) produces
`/api/v1/v1/...`.

**Resolution:** `joinPath` collapses a duplicated trailing base segment, so both
`/session/login.json` and `/v1/session/login.json` resolve to the same URL.
Covered by `TestPathJoinDeduplicatesTheVersionPrefix` and
`TestArchivedSpecPathsCarryTheVersionPrefix`.

## D4 — `download/file.json` accepts parameters the PDF does not list {#d4}

*PDF / whitepaper §2.3:* `session_id`, `offset`, `inline`, `sharing_id`, `test`,
`backup`, `temp_key`.

*Live spec* additionally documents `app` (string), `temp_auth` (string) and
`preview` (int).

**Status:** open. Their semantics are unknown; `temp_auth` looks related to the
`temp_key` flow for password-protected files, and `preview` to inline rendering.
To be probed against the sandbox in P3 before any of them is exposed through the
Bridge API.

## D5 — `file/thumb.json` accepts `time_offset` and `temp_key` {#d5}

*Live spec:* besides `file_id`, `session_id` and `sharing_id`, the endpoint takes
`time_offset` (float — presumably the frame to grab from a video) and `temp_key`.
Neither appears in the PDF's thumbnail section.

**Status:** open, to be confirmed in P2 when the file module is bound.

## D6 — `oauth2/grant.json` documents three grant types and a different `client_id` meaning {#d6}

*Live spec:*

> `grant_type` : string (required) — authorization_code, password, refresh_token
> `client_id` : string (required) — **Partner ID**
> `username`, `password`, `code` : string

Two notes:

1. The whitepaper (§2.2 B) only covers `password` and `refresh_token`. An
   `authorization_code` flow exists upstream and would require a registered
   partner; out of scope for v1.0, worth revisiting for a hosted Bridge.
2. `client_id` is documented as the *partner id*, while the whitepaper's example
   passes the literal `"OpenDrive"`. The SDK defaults to `DefaultClientID =
   "OpenDrive"` and allows an override via `WithClientID`, so a partner
   deployment needs no code change.

The public spec does not document the grant *response*, so the token lifetimes
(86400 s access, 30 days refresh) still come from the PDF. The SDK prefers
`expires_in` from the response when present and falls back to those constants.

**Status:** open until the authenticated spec or a sandbox login confirms the
response shape.

## D7 — listing pagination pairs `offset` with `last_request_time` {#d7}

*Live spec* (`folder/shared.{format}`, the public sibling of `folder/list.json`)
documents `offset`, `last_request_time`, `with_breadcrumbs`, `order_by` and
`order_type`, confirming whitepaper §2.6 #14: paging past the first 100 entries
requires echoing the previous response's `DirUpdateTime`.

Note the spelling here is `with_breadcrumbs` (correct), whereas the standalone
breadcrumb endpoint is `folder/breadcrump.json` (misspelled) — §2.6 #3. Both
spellings are real and neither may be "corrected".

**Resolution:** modelled by `Pagination` (`FirstPage`/`Next`/`Validate`), which
clamps the page size to 100 and refuses a later page without `last_request_time`.
Covered by `TestPaginationProtocol`, `TestArchivedSpecShowsThePaginationParameters`
and `TestUpstreamSpellingConstants`.

## D8 — public `users/*` endpoints absent from the PDF's Users chapter {#d8}

*Live spec* exposes `users/forgotpassword.json`, `users/confirmpasswordreset.json`
and `users/verifyemailsignup.json` without authentication.

**Status:** out of scope for v1.0 (§1.4 covers Users read-only). Recorded so the
drift watcher does not report them as new later.

## D9 — captcha is not limited to login {#d9}

*Live spec:* `POST /v1/file/verifypassword.json` accepts a `captcha_response`
parameter, so password-protected file access can also be throttled behind a
captcha, not just `session/login.json` (§2.6 #11).

**Status:** open. The SDK already maps any captcha response to
`KindCaptchaRequired`, which is never retried; the P2 file module must surface it
on `verifypassword` too.

## D10 — there is no `sharing` module in the resource listing {#d10}

*Whitepaper §2.3* lists a Sharing module (`listsharedfolders.json`,
`listsharedusers.json`, `listusers.json`, `sharing.json`, `setmode.json`).

*Live spec* `resources.json` advertises exactly nine modules: `branding`,
`download`, `file`, `folder`, `session`, `tasks`, `upload`, `users`, `oauth2`.
There is no `sharing` resource; the public `folder` module does expose
`folder/shared.json` and `folder/sharedinfo.json`.

**Hypothesis:** the sharing endpoints live inside the `folder` and `file`
modules and are only visible to an authenticated explorer session.

**Status:** open, resolved by the authenticated fetch (see D11).

## D11 — the committed archive is the public subset only {#d11}

`testdata/spec/manifest.json` currently records `"strategy": "anonymous"`:
10 documents, 21 endpoints, 21 operations. The authenticated explorer exposes
considerably more (the whole `folder` module is 20 endpoints on its own).

`tools/fetch-spec` already implements four authenticated strategies (PHP session
cookie, `session_id` query parameter, session path segment, OAuth2
`access_token`) and keeps whichever reveals the most operations. It needs
`ODB_SPEC_USER` / `ODB_SPEC_PASS` in the environment.

**Status:** blocked on test-account credentials. Until then, the contract tests
that need authenticated modules skip rather than fail.
