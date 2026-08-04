# CLAUDE.md

Project knowledge for Alfred lives in [`ALFRED.md`](./ALFRED.md) at the repo root. Read it before answering questions about this codebase.

It covers: what Alfred is, the two-workflow split (TicketWorkflow / ChatWorkflow), the 2026-05-22 hybrid agent loop refactor, key non-obvious patterns (dynamic tool dispatch, `request_approval` interception, two-layer idempotency, epoch transitions, TieredContextBuilder), bootstrap / config / secrets, the database schema, the two Hindsight banks (don't confuse runtime memory with the `alfred-dev` developer bank), the agentic-backbone roadmap, dev workflow, strict user conventions (no em dashes, first-person, Forgejo policy, Gemini review style), and a file map.

Also pull persistent context from the `alfred-dev` Hindsight bank via `mcp__hindsight-memory-alfred-dev__recall` when starting work on a non-trivial task.
