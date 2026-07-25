# CLAUDE.md — Instructions for AI developers

1. **Read `OpenDrive-Bridge-Whitepaper.md` in full before writing any code.** It is the single source of truth for scope, architecture, phases (P0–P6), and quality gates. Appendix D lists non-negotiable rules.
2. Follow the phase order in whitepaper §5. Do not skip exit criteria. Current phase: **P0 (scaffolding done; implement `tools/fetch-spec` next)**.
3. Upstream behavior: trust the live Swagger specs (`https://dev.opendrive.com/api/v1/resources/*.json`) and real sandbox responses over the PDF. Record discrepancies in `docs/discrepancies.md`.
4. Every gotcha in whitepaper §2.6 (14 items) must have at least one test case.
5. Never log or persist passwords/tokens; token redaction in logs is mandatory (§9.4).
6. `Doc/` contains official OpenDrive reference material — read-only, never modify.
7. Conventional Commits; every PR includes tests; keep `main` releasable.
8. Coverage gates: `pkg/opendrive` ≥ 85%, `internal/*` ≥ 75% (§6.3).
