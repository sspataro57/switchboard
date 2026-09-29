# Handoff to the kube session: /watch.json for the Pebble face (SWT-101, swb 865)

`GET /watch.json` returns the board's header counts and the waiting and working session names. It is gated only by a
bearer token, and the dashboard reads the token from `SWB_WATCH_TOKEN`. Without the variable, the route answers 404.

Image: `192.168.50.20:5000/switchboard:<TAG>`, built from `main` at `<COMMIT>`. It includes everything in 0.7.63.

## 1. Secret, BEFORE the roll

`ops/swb-watch-token`, key `token`, value from `openssl rand -hex 32`. Keep the value; step 4 needs it.

## 2. deployment/dashboard

Add env `SWB_WATCH_TOKEN` from `secretKeyRef {name: swb-watch-token, key: token}`. Nothing else changes: no port, no probe,
no other workload.

## 3. No ingress or DNS change

Both `ops/dashboard` and `ops/dashboard-tls` already route every path to the service. **Add no public DNS record and no
external exposure of any kind**: `switchboard.sspataro.com` stays LAN-only, resolved by the Pi-hole.

## 4. Roll ONE tag to ALL 13 workloads in ONE apply

No migration. Keep the pins, and do the SWT-76 check before replacing the watcher pod.

Check:
- `curl -s -o /dev/null -w '%{http_code}' https://switchboard.sspataro.com/watch.json` returns 401.
- With `-H "Authorization: Bearer $TOKEN"` it returns 200 and JSON, and its four numbers match the board header.
- If the pod came up without the env var (the Secret is missing), the first curl returns 404, not 401. Fix the Secret.
- Tell the switchboard session when done, and hand the token to Salvador for the Pebble `src/pkjs/config.js`
  (`swbToken`). The token ends up inside the `.pbw`.

## 5. Rollback

Roll back to 0.7.63. `/watch.json` then 302s to login, which the watch reads as "home, swb silent". The Secret can stay.
