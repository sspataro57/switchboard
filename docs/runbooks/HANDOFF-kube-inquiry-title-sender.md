# Handoff to the kube session — inquiry task titles name the sender (swb 384, SWT-87)

An inquiry-lane task is now titled "{sender's display name}: {ask}". Before, it used the model's "asker",
which on José's mail named Katie, who was only quoted inside it. If the ask is empty the subject is used,
and if that is empty too the title is the sender alone, never a dangling "Name: ". The same image also
carries swb 431 (wording only: sessions are named by the swb hook, not ListAgents).

Image: `192.168.50.20:5000/switchboard:0.7.53`
(`sha256:8a8c5e6e07e6269fd7ff1f6e665d79d0f4cba807f2b4cffc8a16e4204e620b8d`), built from `main` at `0f07fd8`.
It includes everything in 0.7.52.

## 1. No migration, no env var, no manifest change beyond the tag

## 2. Roll ONE tag to ALL 13 workloads in ONE apply

Keep the pins. Before replacing the watcher pod, run the SWT-76 check: bridge `send_queue.waiting == 0`.
The promote step (in `pipelined`) is the only workload whose behaviour changes.

## 3. Check

The next inquiry-lane task's title starts with the sending person's name, for example
`José Garcia: …`, not with a name quoted inside the message.

## 4. Rollback

Roll all 13 back to 0.7.52 together.
