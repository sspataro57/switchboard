# Handoff to the kube session: availability in Miami time, 24h calendar freshness (swb 1084)

Image: `192.168.50.20:5000/switchboard:0.7.69` (`sha256:7f4098922f6355463c70d776b7406f771300847fc4fc881463bf68be12fead0c`),
built from `main` at `5f95c0e`. It includes everything in 0.7.67 (0.7.68 was an intermediate build; skip it).

Code changes: `AVAIL_TZ` now defaults to America/New_York (was Europe/Rome), and `AVAIL_MAX_SYNC_AGE` defaults
to 24h (was 1h). Salvador chose 24h on 2026-10-01 because the calendar is read only twice a day.

## 1. deployment/dashboard: REMOVE the `AVAIL_MAX_SYNC_AGE=150m` env entry

Removing it lets the new 24h default apply. If you'd rather keep it explicit, set it to `24h`; any other value
overrides his choice. Set no `AVAIL_TZ`: the default is right. No other manifest change, no Secret, no migration.

## 2. Roll ONE tag to ALL 13 workloads in ONE apply

Keep the pins, and do the SWT-76 check before replacing the watcher pod.

Check: on `https://switchboard.sspataro.com/funnel` the calendar rows are green at any hour (last calendar sync
under 24h). Tell the switchboard session when done.

## Rollback

Roll back to 0.7.67 and restore `AVAIL_MAX_SYNC_AGE=150m`.
