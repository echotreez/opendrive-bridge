# Sharing fixtures

The sharing module could not be recorded from the sandbox. The test account is
an *account user*, and upstream refuses every sharing operation for that account
type — reads included — with the body in `account_user_denied.json`
(docs/discrepancies.md D33). That refusal is real, captured on 2026-07-27.

The success shapes in this directory are therefore **constructed** from the
Swagger declaration and the PDF rather than recorded, and the tests that use
them say so. They must be re-recorded against an account-owner login before the
sharing bindings can be called verified.
