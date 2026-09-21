# Handoff to the kube session — installed board app starts refreshing (SWT-72)

Salvador installed the board as a full-screen app on the tablet. The app's start address is now
`/tasks?refresh=on`, so it opens on the auto-refreshing board. The web app manifest moved to a new
file name because everything under `/static/` is cached `immutable` for a year.

Image tag and digest are in the message that accompanies this file (built from `main` after the
merge). It supersedes 0.7.37 and carries nothing else new.

## 1. No migration, no env var, no manifest change beyond the tag

Newest migration is still 0038 (applied). Only `deployment/dashboard` changes behaviour. Roll the
same tag to all 11 workloads as usual, keeping the pins. As before: check no Send is in flight
before replacing the dashboard pod.

## 2. Post-roll check

```bash
curl -s https://switchboard.sspataro.com/static/manifest-v2.webmanifest | grep start_url
#   "start_url": "/tasks?refresh=on",
curl -s -o /dev/null -w '%{http_code}\n' https://switchboard.sspataro.com/static/manifest.webmanifest
#   404 — the old name is gone on purpose
```

## 3. Rollback

Set the tag back to 0.7.37. An app installed from the new manifest keeps working either way; its
start address simply carries a query the old image also understands.
