# Recorded users responses

Captured from the sandbox on 2026-07-27 and sanitised harder than the other
fixtures: `users/info.json` returns the account holder's postal address, an
account private key and a user UID. All of those are replaced with `REDACTED`
here, and the email address and numeric ids with placeholders.

Everything else is upstream's own and is the reason the models look the way they
do: quoted numbers, booleans that arrive as `"0"` in one field and `false` in
another, and `AccessUserID` differing from `UserID` because this login is an
account user rather than the account owner (docs/discrepancies.md D33).
