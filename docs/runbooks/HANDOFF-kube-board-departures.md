# Handoff to the kube session — board-departures (SWT-67)

The board at `/tasks` is now an airport departures display: dark split-flap rows in two panel
columns that page instead of scrolling, a sign header with tallies and a clock, a ticker footer,
and a machine-status list on a phone. A new page, `/kiosk`, holds the board full-screen on the
tablet. Spec: `docs/tickets/board-departures_SPEC.md` (B21 is the full-screen amendment).

Image: **`192.168.50.20:5000/switchboard:0.7.33`**
(`sha256:b59f479d7c1a77c0f86887958aba696a8c07c58c92577f514bef1a66e5a252da`), built from `main`
at `8961480`. It supersedes 0.7.32 (built from `343f531`) and carries nothing else new.

## 1. No migration, no env var, no manifest change beyond the tag

Nothing in `migrations/` changed (newest is still 0037). No env var, port, volume or secret. The
fonts, icons and web app manifest are embedded in the binary.

Two routes are new on the dashboard, both on the existing port:

| route | auth | what |
|---|---|---|
| `GET /static/{path...}` | **none, deliberately** | embedded fonts, two icons, the web app manifest. Browsers fetch these without credentials; there is no task data in them. Real files only — a miss or a directory is a bare 404. |
| `GET /kiosk` | session, like every page | the full-screen shell: frames `/tasks?refresh=on` |

If anything in front of the dashboard (ingress auth, an oauth2-proxy) gates every path, **`/static/`
must be let through unauthenticated**, or the board renders in fallback fonts and the manifest
never loads. `/healthz` is the existing precedent.

Every response now carries `X-Frame-Options: SAMEORIGIN`. Do not add a `DENY` at the ingress: it
would break `/kiosk`, which frames the board on purpose.

## 2. Roll the tag

Only `deployment/dashboard` changes behaviour. Roll the same tag to all 11 workloads in one apply
as usual, keeping the pins (classify-promote `--lane personal`; pipelined
`PIPELINE_STAGES=gate,route,route_apply,inquiry,inquiry_promote`; `MS_OAUTH_CLIENT_ID` on
connector-google).

## 3. Post-roll check

```bash
curl -sI http://switchboard.home.arpa/static/fonts/b612mono-400.woff2 | grep -iE '^HTTP|content-type|cache-control'
#   200, font/woff2, public, max-age=31536000, immutable      — and NOT a 302 to a login page
curl -sI http://switchboard.home.arpa/static/manifest.webmanifest | grep -iE '^HTTP|content-type'
#   200, application/manifest+json
curl -s -o /dev/null -w '%{http_code}\n' http://switchboard.home.arpa/static/fonts/    # 404: nothing is listed
```

Then open `/tasks` in a browser: a dark board, amber text, panels flipping pages every 9 s. `FULL`
opens `/kiosk`; one tap there goes full-screen and the board keeps refreshing inside it.

## 4. Rollback

Roll `deployment/dashboard` back to 0.7.32. No schema or data is involved; the old board comes
back as it was.

## 5. FOLLOW-UP (separate piece of work, wanted): a certificate the tablet trusts

Salvador chose both full-screen paths. `/kiosk` works today over plain http. The second path is
the installed app: the image already ships a web app manifest (`display: fullscreen`,
`start_url: /tasks`) and the meta tags, but Chrome only treats a site as installable on a
**secure origin with a certificate the device trusts**. Today the tablet shows a warning triangle
on `switchboard.home.arpa`.

What is needed, in whatever way fits the cluster:

- HTTPS on the dashboard's hostname with a certificate the tablet (Android Chrome) trusts — a
  public CA via DNS-01 on a real domain, or a private CA whose root is installed on the tablet.
- First establish how the dashboard is exposed at all: switchboard's notes still say "no Ingress,
  port-forward only", yet Salvador reaches it at `http://switchboard.home.arpa`. One of the two is
  stale; tell the switchboard session which, so its notes get fixed.
- Once HTTPS is trusted: Chrome's menu on `/tasks` offers "Install app"; the installed board
  launches full-screen with no browser bars, and the screen wake lock (already attempted by the
  page, silently a no-op on http) starts working. Nothing to change in this repo.
