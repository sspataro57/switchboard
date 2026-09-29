# Demo mode (SWT-99)

Filters the web dashboard to a chosen set of projects and source accounts for a live demo. It is switched
ONLY here, in psql: no page shows a control or a hint that anything is hidden. Ingestion, the orchestrator,
workers, MCP and sends keep running on everything. It is a read filter on the dashboard plus a refusal of
dashboard verbs on hidden rows. Spec: `docs/tickets/demo-mode_SPEC.md`.

Prerequisite: migration 0048 (`tasks.demo_hidden`) is applied in production.

## 1. Before the demo

Connect with `psql -h 192.168.50.49 -U ops -d ops`.

**Slugs.** Check that the five visible projects are spelled as below. A misspelled slug hides that project,
which errs toward privacy.

```sql
SELECT slug FROM projects ORDER BY slug;
```

**Source accounts.** Choose which accounts `/sources`, `/funnel`, board senders and the task page's
source-message section may show:

```sql
SELECT id, provider, account_email FROM source_accounts ORDER BY provider, account_email;
```

List an account **only if ALL of its traffic is safe to show**. Never `upwork_crm@pg-main`, never a
personal mailbox, and never a mailbox that mixes personal or other-client mail with the demo's work,
because its counts cannot be split per message.

- Accounts match by **email alone, across providers**. `salvador@handsonconnect.org` is both a Gmail
  mailbox that also receives Foundry mail and a Jira account. Listing it shows both, so leave it out.
- Leaving an account out only costs its `/sources` and `/funnel` rows, and the source-message section
  of tasks it fed. Every task still shows.
- For an Avviato demo, the two Slack workspace accounts are the safe set:
  `t0360b84u@slack-web.local` and `t0hpr78rx@slack-web.local`.

**Audit visible-project tasks whose own text names hidden work, and hide them.** The term list is data.
Edit it freely and add names as they come up. The audit is only as good as the list, so after switching
on, skim the demo board and hide anything it missed (hand-pick lines below).

```sql
\set terms 'foundry|upwork|lyle|grady|ahs|saka|town-ai|bank|medical|hoa|personal'
\set visible '{collaboratory,a-millon,reengine,switchboard,homelab}'

-- 1. Audit: visible-project tasks whose own text names a term. Review before step 2.
SELECT t.id, p.slug, t.status, left(t.title, 80) AS title,
       (t.title ~* :'terms') AS in_title, (COALESCE(t.body,'') ~* :'terms') AS in_body,
       EXISTS (SELECT 1 FROM task_events e WHERE e.task_id = t.id AND e.payload::text ~* :'terms') AS in_events,
       EXISTS (SELECT 1 FROM feedback_requests f WHERE f.task_id = t.id
                 AND (f.question ~* :'terms' OR COALESCE(f.answer,'') ~* :'terms')) AS in_feedback
  FROM tasks t JOIN projects p ON p.id = t.project_id
 WHERE p.slug = ANY(:'visible'::text[]) AND NOT t.demo_hidden
   AND (t.title ~* :'terms' OR COALESCE(t.body,'') ~* :'terms'
        OR EXISTS (SELECT 1 FROM task_events e WHERE e.task_id = t.id AND e.payload::text ~* :'terms')
        OR EXISTS (SELECT 1 FROM feedback_requests f WHERE f.task_id = t.id
                     AND (f.question ~* :'terms' OR COALESCE(f.answer,'') ~* :'terms')))
 ORDER BY p.slug, t.id;

-- 2. Hide the hits (same WHERE). Their subtasks follow automatically, including ones created later.
UPDATE tasks t SET demo_hidden = true
  FROM projects p
 WHERE p.id = t.project_id AND p.slug = ANY(:'visible'::text[]) AND NOT t.demo_hidden
   AND (t.title ~* :'terms' OR COALESCE(t.body,'') ~* :'terms'
        OR EXISTS (SELECT 1 FROM task_events e WHERE e.task_id = t.id AND e.payload::text ~* :'terms')
        OR EXISTS (SELECT 1 FROM feedback_requests f WHERE f.task_id = t.id
                     AND (f.question ~* :'terms' OR COALESCE(f.answer,'') ~* :'terms')));

-- Hand-pick instead:   UPDATE tasks SET demo_hidden = true  WHERE id IN (...);
-- Unhide one:          UPDATE tasks SET demo_hidden = false WHERE id = N;
```

`demo_hidden` has no effect while demo mode is off, so hidden tasks can stay marked between demos.

## 2. On

```sql
INSERT INTO ops_flags (name, value) VALUES ('demo_mode',
  '{"on":true,
    "projects":["collaboratory","a-millon","reengine","switchboard","homelab"],
    "source_accounts":["t0360b84u@slack-web.local","t0hpr78rx@slack-web.local"]}')
ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value, updated_at = now();
```

It takes effect on the next request. An open board updates on its next change broadcast, or within
60 s. No restart is needed.

## 3. Off

```sql
UPDATE ops_flags SET value = jsonb_set(value, '{on}', 'false'), updated_at = now() WHERE name = 'demo_mode';
```

## How it fails

- A `demo_mode` value that doesn't decode, or whose `on` isn't a bool, fails **closed**: demo is on
  and nothing is visible, and the board looks empty. Fix the JSON.
- If the flag can't be read (a database problem), every authenticated page answers 503. It never
  falls back to the full view.
- A hidden task's page and a nonexistent task's page are identical, and actions on a hidden id refuse
  with "not found".
