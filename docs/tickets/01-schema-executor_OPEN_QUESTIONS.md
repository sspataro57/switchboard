# 01-schema-executor — Open questions

**ANSWERED 2026-07-11 (Salvador, in session). Folded into the SPEC.**

- Q1: **Create `embeddings` now** — pg-main's CNPG image already has pgvector.
- Q2: **Yes, real `ops` db** — applying migrations to pg-main is part of done.

## 1. `embeddings` table now (pgvector) or deferred?

Create `embeddings` in 0001 with a `vector` column — requires the pgvector
extension available on pg-main's CNPG image AND in the local compose image
(`pgvector/pgvector` instead of stock `postgres`) — OR follow the job-agent
0001 precedent: ship `content_chunks` only now, add `embeddings` in a later
numbered migration when RAG actually lands (no build-order step currently
claims it).

Deferring costs nothing (forward-only migrations make adding it trivial) and
avoids betting on pg-main's extension set unverified; creating now means 0001
matches CLAUDE.md's schema list exactly.

## 2. Does step-1 "done" include applying migrations to the real `ops` db on pg-main?

Apply to the real db now — requires establishing local access (port-forward to
`pg-main-rw.cnpg.svc:5432`, creating the `ops` database, recording the
connection recipe in INSTITUTIONAL_KNOWLEDGE.md "Environment facts", currently
TBD) — OR treat the dockerized local Postgres as sufficient for step 1 and do
the real apply at the start of step 2, which needs pg-main access anyway (the
`upwork_crm` tables live there).

Now front-loads the infra errand and makes the smoke check "real"; deferring
keeps step 1 purely local and bundles the pg-main access work with the step
that can't avoid it.

---

Answer by editing the entries. Say 'questions answered' and I'll fold them into
the SPEC.
