# OpenDrive's own documentation, and why it is not in this repository

This project was built against OpenDrive's REST API Guide. That document is not
here, and cannot be: its copyright page says

> No part of this document may be reproduced or transmitted in any form or by
> any means, electronic or mechanical, for any purpose, without the express
> written permission of OpenDrive, Inc. Under the law, reproducing includes
> translating into another language or format.

— which rules out committing it to a public repository, and rules out shipping a
converted copy of it either. It was in this repository's history while the
repository was private; it has been removed from the history as well as from the
working tree.

## Where to get it

- **The API explorer**, which needs no paperwork and is the most current thing
  OpenDrive publishes: <https://dev.opendrive.com/api/explorer/>
- **The machine-readable specs** behind it, one JSON file per module:
  `https://dev.opendrive.com/api/v1/resources.json`, then the per-module files
  it lists. `scripts/fetch-spec.sh` archives them. Note that anonymously the
  explorer exposes 21 operations out of 220 — it has to be fetched signed in to
  see the whole surface.
- **The PDF guide** itself: ask OpenDrive at <support@opendrive.com>. The
  version this project was built against is *REST API Guide v1.1.7, 10/2023*.

If you have a copy, put it at `docs/OpenDrive_API_guide.pdf`. That path is in
`.gitignore`, so it will sit alongside the code without ever being committed.

## Which source wins

Not the PDF. In descending order of authority:

1. **`docs/discrepancies.md`** — 46 places where this project measured the live
   API doing something other than what it was told. Several show the PDF is
   simply wrong: the breadcrumb endpoint is spelled correctly online and
   misspelled in the guide (D12), the file resource is `/file.json` and not
   `/file/file.json` (D14), and `access.json` versus `filesettings.json` have
   their HTTP verbs the other way round (D15).
2. **`docs/error-taxonomy.md`** — what an upstream failure actually means, and
   whether it may be retried. Upstream's own status codes and messages are not
   authoritative here, for reasons that document sets out one disguise at a time.
3. **The live Swagger specs**, which are newer than the PDF.
4. **The PDF**, last.

Nothing in this repository is derived from the text of the guide. The
discrepancy notes and the error taxonomy are records of requests made and
responses received — facts about the API's behaviour, gathered by running it.
That is why they can be published when the document they disagree with cannot.

## The sample code is a different matter

`docs/api-samples/` holds OpenDrive's official PHP, C# and JavaScript samples,
which they publish under the MIT licence. They stay, with attribution: MIT,
© OpenDrive, Inc. They are worth keeping because they are the only place some of
the upload protocol's sequencing is written down at all — the four-step chunked
upload in particular was read out of `test_file_upload_chunked.php` before it was
confirmed on the wire.
