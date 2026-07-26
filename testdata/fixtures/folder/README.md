# Recorded upstream responses

Real bodies from the OpenDrive sandbox, captured on 2026-07-26 and sanitised:
folder and file identifiers, owner ids and share links were replaced with
placeholders of the same shape and type. Nothing else was touched — in
particular the mixed types are upstream's own (`Encrypted` is the integer 0 at
the top level and the string "0" inside a folder entry, `Shared` is the string
"False", `UserAccessMode` is an integer in a listing and a string in
useraccessmode.json), which is exactly what the lenient decoders exist for
(whitepaper §2.6 #5, docs/discrepancies.md D19).

These fixtures are the response side of the contract tests; the request side is
checked against `testdata/spec/` (whitepaper §6.1).
