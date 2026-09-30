# Handoff to the kube session: split-flap rows twice as fast (swb 989, SWT-102 follow-up)

Template-only change (two timing constants): no env var, no Secret, no migration, no ingress change.

Image: `192.168.50.20:5000/switchboard:0.7.67` (`sha256:3a41a5dcc55cc002468bcd7ded601d4057cb9806c41eff6c02b8b60a4a24c4eb`),
built from `main` at `0566218`. It includes everything in 0.7.66.

## Roll ONE tag to ALL 13 workloads in ONE apply

Keep the pins, and do the SWT-76 check before replacing the watcher pod. Tell the switchboard session when done.

## Rollback

Roll back to 0.7.66.
