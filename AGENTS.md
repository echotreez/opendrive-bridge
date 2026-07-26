# AGENTS.md — Instructions for AI developers

1. **Read `OpenDrive-Bridge-Whitepaper.md` in full before writing any code.** It is the single source of truth for scope, architecture, phases (P0–P6), and quality gates. Appendix D lists non-negotiable rules.
2. Follow the phase order in whitepaper §5. Do not skip exit criteria. Current phase: **P2, endpoint-binding half** — P0, P1 (aligned to whitepaper v1.1: `keystore_unavailable`/`reauth_required`, `Identity()`/`AuthState()`, the silent re-login state machine), the P2 credential-persistence half (`internal/keystore`) and the folder module (`pkg/opendrive/folder.go` + `path.go`, `internal/cache`) are complete. Next: bind file, then sharing and users, against `testdata/spec/`.
   - New bindings follow the folder module's pattern: a service struct, one method per endpoint with the verb and session placement in its doc comment, a row in the module contract test (`folder_contract_test.go` shows the shape), and a recorded fixture in `testdata/fixtures/` for anything whose response shape came off the wire.
   - Credentials go through `opendrive.CredentialStore` only; `internal/keystore.Open` chooses the backend and is the "no persistence = configuration error" gate. `MemoryTokenStore` is test-only and reports itself ephemeral. Never introduce another path that stores a password.
   - Known gap: no Windows keyring backend yet (macOS Keychain and Linux Secret Service are driven through their CLIs, with the secret on stdin so it never reaches process arguments). Windows currently needs the encrypted-file backend with an explicit `ODB_STATE_KEY`; a DPAPI / Credential Manager backend is owed before the P5 release.
3. Upstream behavior: trust the live Swagger specs (`https://dev.opendrive.com/api/v1/resources/*.json`) and real sandbox responses over the PDF. Record discrepancies in `docs/discrepancies.md` — there are 21 so far, and several show the PDF is simply wrong (D12, D14, D15). Refresh the archive with `scripts/fetch-spec.sh`, which reads the test credentials from the OS keychain; it must run authenticated, since anonymously the explorer exposes only 21 of 220 operations.
4. Every gotcha in whitepaper §2.6 (14 items) must have at least one test case.
5. Never log passwords/tokens; token redaction in logs is mandatory (§9.4). Persisting credentials is allowed **only** through the CredentialStore (`internal/keystore`: OS keyring or explicitly configured encrypted file) per whitepaper v1.1 §2.2/§9.2 — never in config files, plain files, env dumps, or process args.
6. `docs/` contains official OpenDrive reference material — read-only, never modify.
7. Conventional Commits; every PR includes tests; keep `main` releasable.
8. Coverage gates: `pkg/opendrive` ≥ 85%, `internal/*` ≥ 75% (§6.3).

## Imported Claude Cowork project instructions

本project是根据官方提供的指引和API代码样本,开发一个用于OpenDrive.com的Cloud Storage的API段. 官方也提供了API 样本于地址: https://dev.opendrive.com/api/explorer/
开发结果要求达到官方指引文档指定的所有功能和安全保护.
