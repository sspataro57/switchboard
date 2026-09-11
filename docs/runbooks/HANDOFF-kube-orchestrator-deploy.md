# Handoff → kube session: deploy orchestratord (SWT-41)

The switchboard session writes this. The code is merged and the image is pushed; the cluster state
is yours.

**Image:** `192.168.50.20:5000/switchboard:<tag>`. The tag and digest are filled in at build time,
along with the commit it was built from on `main`. It is the first image that carries
`/usr/local/bin/orchestratord`: the Dockerfile build line never included it before SWT-41.

## Order matters: do not apply until told the cursor is advanced

The switchboard session runs `orchestrator_cursor_advance` (start from now, owner decision
2026-09-11) BEFORE the Deployment exists. The tool refuses while an orchestratord holds the lock.
A Deployment applied first would replay every event since July. Wait for "cursor advanced", with
the pasted tool output, in this file or in the session.

## Preconditions

- **No migration in SWT-41.** Still compare prod `schema_migrations` against `ls migrations/`
  before pushing the tag (IK landmine). If SWT-40 merged first, its migrations must be applied
  before any workload runs the new tag.
- The entrypoints were verified to start in the image: each fails on its own missing config.
  orchestratord specifically exits with `MQTT_BROKER is not set`.

## What needs doing

### 1. New manifest `kube/switchboard/orchestrator.yaml`

A Deployment `orchestratord` in `ops`:

- `replicas: 1`, `strategy: Recreate`. Two replicas would fight over advisory lock `0x5157_0005`.
  RollingUpdate would start the new pod while the old one still holds the lock.
- `command: ["/usr/local/bin/orchestratord"]`
- `automountServiceAccountToken: false`. It never talks to the Kubernetes API.
- `terminationGracePeriodSeconds: 30`. SIGTERM is handled.
- env, **only**:
  - `DATABASE_URL` from `secret/switchboard-db` key `DATABASE_URL`
  - `MQTT_BROKER=tcp://192.168.50.45:1883`
  - `ORCH_HEALTH_ADDR=:8091`
- **Not** `ORCH_BRIEF_PROJECT` / `ORCH_BRIEF_HOUR`: the morning brief stays off (owner, 2026-09-11).
- **No** `OPS_TOKEN_KEY`, Slack bridge, Pipedream or `OPS_LOCAL_*` env. The process wires no
  sender, and least privilege keeps it that way.
- `livenessProbe`: `httpGet` `/healthz` on port `8091`, `initialDelaySeconds: 20`,
  `periodSeconds: 30`, `failureThreshold: 3`. No readinessProbe and no Service: nothing calls it.
- resources: requests `{cpu: 10m, memory: 32Mi}`, limits `{cpu: 200m, memory: 128Mi}`.
- the dashboard's securityContext: non-root, read-only root fs, drop ALL.
- Header comment: "single-instance by advisory lock 0x5157_0005; an exit with 'another
  orchestratord holds the advisory lock' right after a rollout is expected, not an incident — it
  clears within one restart."

### 2. Bump the dashboard to the same tag

It carries the new Orchestrator section on `/funnel` and the red line on `/tasks`. Bumping the
connectors and classify CronJobs too is uniformity only, and your call.

### 3. Docs

Update `connectors.yaml`'s header comment and `README.md`, which say the orchestrator is "NOT
deployed yet".

## How to tell it worked

```bash
kubectl -n ops logs deploy/orchestratord | head      # {"msg":"orchestratord running",...}
```

- `/funnel` → Orchestrator `ok`, backlog 0.
- `/tasks` → no red line.

Then the switchboard session runs the smoke (SPEC V5, which includes a scale-to-0 stall check).

## Deliberately NOT in this handoff

- **fleetd and hooksd** stay undeployed. orchestratord needs neither: R2 resolves resume targets
  from `task_claims`, and publishing needs only the broker.
- **No change to the classify or connector CronJobs.** Moving them to MQTT-woken consumers is a
  follow-up ticket.
- **No Ingress, no Service** for orchestratord.

## Rollback

`kubectl -n ops scale deploy/orchestratord --replicas=0`, or delete the Deployment. There is
nothing to undo in the database: the cursor stays where the engine left it, and restarting later
catches up.
