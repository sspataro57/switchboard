# Handoff to the kube session: split-flap retune (swb 986, SWT-102 follow-up)

Template-only change to the opt-in board "flaps" style: no env var, no Secret, no migration, no ingress change.

Image: `192.168.50.20:5000/switchboard:0.7.66` (`sha256:883560b23b0b58f2af4ee7cd54c70862713f7c07ffc09754aec302744da9afa8`),
built from `main` at `283f79d`. It includes everything in 0.7.65.

## Roll ONE tag to ALL 13 workloads in ONE apply

Keep the pins, and do the SWT-76 check before replacing the watcher pod. Tell the switchboard session when done.

## Rollback

Roll back to 0.7.65.
