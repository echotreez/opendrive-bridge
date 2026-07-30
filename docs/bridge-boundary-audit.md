# Boundary audit — all 45 discrepancies against the Bridge API

P4's central obligation is one sentence: **none of the recorded upstream
discrepancies may reach a user.** This is the line-by-line check of it, done
before packaging rather than after, because a leak found after a release is an
incident and a leak found here is an afternoon.

Every entry in `docs/discrepancies.md` lands in exactly one of three columns:

| verdict | meaning |
|---|---|
| **stopped** | the Bridge handles it; a caller cannot observe it at all |
| **disclosed** | it shapes the API, and `docs/bridge-openapi.yaml` says so in words a caller can act on |
| **n/a** | it never reaches the Bridge surface (tooling, spec archive, out-of-scope modules) |

Nothing is allowed to be "known but unhandled".

---

## Stopped at the boundary

| # | what upstream does | where it stops |
|---|---|---|
| D1 | `download/all.json` ignores `session_key` and falls back to anonymous | SDK sends `session_id`; `TestArchiveSendsFoldersOnly` asserts `session_key` is never sent |
| D3 | `/v1` appears in both base path and endpoint paths | `joinPath` collapses it; a caller never sees a URL |
| D12 | the PDF's misspelled `breadcrump.json` does not exist | endpoint constants are checked against the archived spec |
| D14 | the file resource is `/file.json`, not `/file/file.json` | same |
| D15 | three endpoints use a different verb than documented | same |
| D16 | `move`/`overwrite_if_exists` are required strings | `StringBool`; the Bridge takes real JSON booleans |
| D18 | expiring links pass every argument as a path segment | `SessionInPath` + `PathSegments`; `/v1/share/link` takes a path and a date |
| D19 | the same field is a string in one response and a number in another | `FlexString`/`FlexInt`/`FlexBool`; the Bridge emits one type per field |
| D20 | `users/info.json` returns a `PrivateKey` | on the redaction list; `/v1/auth/status` never carries it |
| D22 | `idbypath` answers `FolderId`, everything else `FolderID` | decoder accepts both |
| D23 | `itembyname` reports "not found" by omitting the arrays | mapped to `not_found`; `/v1/stat` answers 404 |
| D24, D26 | the two `move_copy` endpoints disagree on the type of `move`, and the folder one refuses the documented boolean | SDK sends the string form; `/v1/mv` and `/v1/cp` differ by URL only |
| D25 | a read-only account refuses writes | classified `upstream_error`, never a credential kind |
| D27 | `folder/info.json` still describes a deleted folder | **`/v1/stat` never calls it.** Resolution is idbypath + parent listing; `TestStatNeverAsksInfoJSON` fails the build if that changes |
| D28 | `POST /file.json` rejects an empty `access_folder_id` | SDK defaults it to the root marker |
| D29 | `DELETE /file.json` deletes a file that was never trashed | `/v1/rm` defaults to the trash; `permanent: true` is opt-in and the reply says it cannot be undone |
| D30 | `filefullpath` answers in `DownloadLink`, with backslashes | binding reads the right field and normalises separators |
| D31 | the expiring-link endpoints return one object, not an array | one `ExpiringLink` type; `/v1/share/list` returns an array either way |
| D35 | `TotalWritten` counts the chunk, not the running total | upload pipeline compares per chunk |
| D36 | a deduplicated upload must close *without* `temp_location` | dedupe branch omits it |
| D37 | a wrong chunk offset is refused with the authoritative resume point | `sendChunkResuming` consumes it; never surfaces |
| D38 | the chunk endpoint answers OAuth2 with an HTML 401 from a proxy | `NeedsSessionID` + `SessionIDProvider`; and a non-JSON body can never become a credential verdict (`edge_rejected`) |
| D39, D40 | a transient refusal and a real denial are worded identically | the classifier's probe and witness decide; `/v1/mkdir` on a denied path says *"Your OpenDrive account is not allowed to do this here"*, never "contact your administrator" |
| D41 | the download `offset` is off by one at the last byte | the pipeline sends `Range` and treats a 200 as "not honoured", skipping forward |
| D42 | `download/all.json` answers a file list with an empty archive | **the parameter is not on the Bridge.** `/v1/download/archive` takes folders only; a test asserts it is absent from the spec too |
| D43 | `filesettings` ignores an unknown parameter and answers 200 | the SDK sends the name that works; the live test asserts the effect, not the status |
| D44 | a just-uploaded file is briefly not downloadable | the job engine waits for the `test=1` probe; a 404 is never made retryable, and the timeout reports *"the upload completed but…"* rather than "file does not exist" |
| D45 | `users/info.json` mixes bytes and megabytes | `/v1/auth/status` normalises both limits to bytes |

## Disclosed in the OpenAPI document

Four behaviours cannot be hidden without lying about what the API can do, so the
specification states them plainly instead.

| # | what a caller must know | where it is written |
|---|---|---|
| D7, D17 | paging needs the previous page's `dir_update_time`; there is no page token, and upstream silently re-serves page one without it | `/v1/ls` description and the `Listing` schema; the Bridge **refuses** an offset without it rather than looping |
| D32, D4 | a password-protected file cannot be fetched by anyone but its owner through any API route; `temp_key`/`temp_auth` are unusable | D32's closure; the Bridge exposes no password parameter, and a non-owner meets `403 File requires password` |
| D42 | the archive covers folders, not arbitrary file lists | `/v1/download/archive` description says why |
| D45 | every `quota` figure is in bytes, because upstream's are not consistent | the `Status.quota` schema says so |

## Not on the Bridge surface

| # | why |
|---|---|
| D2, D9 | captcha detection is SDK-internal; a caller sees `captcha_required` with instructions |
| D5 | thumbnails are not a v1.0 endpoint |
| D6 | grant types are an SDK concern; `/v1/auth/login` takes a username and a password |
| D8, D21 | modules outside v1.0 scope (§1.4) |
| D10, D11 | properties of the spec explorer, used by `tools/fetch-spec` |
| D13 | `has_ddref` is an internal optimisation of the upload path |
| D33 | the sharing module is closed to this account type; `/v1/share/*` reports the refusal in the standard envelope |
| D34 | activity logs are not a v1.0 endpoint |

---

## What the audit found

Two gaps, both closed while writing this:

1. **D45 was not in the spec.** The `quota` schema listed four integers with no
   unit. A client would have had no way to know the Bridge had corrected
   anything, and no reason to distrust the raw figures if it ever read them from
   elsewhere. The schema now says the figures are in bytes and why.

2. **D7's paging contract was documented but its consequence was not.** The
   description explained the parameter; it did not say that the Bridge *refuses*
   rather than silently re-serving page one. That refusal is the part a client
   has to code against.

Everything else was already handled. The two behaviours worth re-reading before
P5 are D39/D40 and D44 — they are the ones whose correct handling depends on
runtime evidence rather than on a fixed rule, so they are the ones most likely to
regress quietly.

**Standing rule:** a new entry in `docs/discrepancies.md` is not finished until
it has a row in this table.
