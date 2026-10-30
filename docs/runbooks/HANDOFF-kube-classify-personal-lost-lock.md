# Handoff to the kube session: classify-personal no longer fails on a lost lock (SWT-112, swb 2907)

pipelined holds `0x5157_0022` on every sweep, so classify-personal losing it recorded Failed Jobs for nothing (three on
2026-10-29). `classify run --lane personal` now logs "skipping this run" and exits 0.

Image: `192.168.50.20:5000/switchboard:0.7.73`
(`sha256:db00c28261ef9cd7a728fae82453a8514b80632926996c0bc81efe5b8a7b9cf8`), built from `main` at `af9c23a`.
It includes everything in 0.7.72.

Narrower than the kube session's ask, on purpose. These still fail the run when they lose a lock:
- **classify-residue:** it runs daily over `--since 72h`, so three misses can drop messages for good.
- **classify-promote:** SWT-30 made this an error so that a lost lock cannot look like an empty inbox.

## 0. Already done

Nothing. There is no migration.

## 1. Roll ONE tag to ALL 13 workloads in ONE apply

Only the classify-personal CronJob changes behaviour. Keep the pins, and do the SWT-76 check before replacing the watcher
pod. No env var, port or probe change.

## 2. Manifest comment

`switchboard/classify.yaml` lines 7-10 say an overlapping run exits 1 with "another classify run holds the advisory lock;
exiting". Update it: the personal lane now logs "skipping this run" and exits 0; the residue lane still exits 1. The
"do not alert on it" advice no longer applies to a Failed classify-personal Job, which now means a real failure.

## 3. Check

- `kubectl -n ops get cronjob classify-personal` shows 0.7.73 in its image.
- The next tick's Job completes.
- A long lock holder now delays classify-personal without a Failed Job. "skipping this run" on every tick in its logs is
  the sign.

## 4. Rollback

Roll all 13 back to 0.7.72.
