# CLAUDE.md — Instructions for AI developers

1. **Read `OpenDrive-Bridge-Whitepaper.md` in full before writing any code.** It is the single source of truth for scope, architecture, phases (P0–P6), and quality gates. Appendix D lists non-negotiable rules.
2. Follow the phase order in whitepaper §5. Do not skip exit criteria. Current phase: **P2 (P0 and P1 complete: `tools/fetch-spec`, the spec archive, and `pkg/opendrive` client/auth/types/errors are in; next per whitepaper v1.1: first `internal/keystore` (CredentialStore), then P1 alignment to the v1.1 auth state machine, then binding the folder/file/sharing/users endpoints)**.
3. Upstream behavior: trust the live Swagger specs (`https://dev.opendrive.com/api/v1/resources/*.json`) and real sandbox responses over the PDF. Record discrepancies in `docs/discrepancies.md` — there are 21 so far, and several show the PDF is simply wrong (D12, D14, D15). Refresh the archive with `scripts/fetch-spec.sh`, which reads the test credentials from the OS keychain; it must run authenticated, since anonymously the explorer exposes only 21 of 220 operations.
4. Every gotcha in whitepaper §2.6 (14 items) must have at least one test case.
5. Never log passwords/tokens; token redaction in logs is mandatory (§9.4). Persisting credentials is allowed **only** through the CredentialStore (`internal/keystore`: OS keyring or explicitly configured encrypted file) per whitepaper v1.1 §2.2/§9.2 — never in config files, plain files, env dumps, or process args.
6. `docs/` contains official OpenDrive reference material — read-only, never modify.
7. Conventional Commits; every PR includes tests; keep `main` releasable.
8. Coverage gates: `pkg/opendrive` ≥ 85%, `internal/*` ≥ 75% (§6.3).
