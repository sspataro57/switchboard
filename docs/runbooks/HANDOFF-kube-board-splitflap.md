# Handoff to the kube session: board split-flap style (SWT-102, swb 973)

A client-side animation style for `/tasks`, opt-in per browser via the "flaps" toggle. Template-only change:
no env var, no Secret, no migration, no ingress change.

Image: `192.168.50.20:5000/switchboard:0.7.65` (`sha256:71e56f6358fa73607045e7962e6b3dd0b8692258add1f104cc7e6980fd71ce27`), built from `main` at `0dced29`. It includes everything in 0.7.64.

## Roll ONE tag to ALL 13 workloads in ONE apply

Keep the pins, and do the SWT-76 check before replacing the watcher pod.

Check: the board at `https://switchboard.sspataro.com/tasks` shows a small FLAPS button after "Advanced filter".
Tell the switchboard session when done; it verifies the animation live.

## Rollback

Roll back to 0.7.64. The stored preference is harmless on the old image (nothing reads it).
