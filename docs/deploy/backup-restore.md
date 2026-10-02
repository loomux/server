# Backup and restore (LOOM-92)

Loomux keeps all state in a single SQLite file. This document covers how
that file is backed up and how to restore it.

## What is backed up

Three mechanisms cover the database:

1. **Online SQLite backup.** A Kubernetes CronJob (declared in theWyseKube's
   `services/base/loomux/60-backup-cronjob.yaml`) runs nightly and executes
   `sqlite3 /data/loomux.db ".backup to /backup/loomux-<timestamp>.db"`.
   This produces a transaction-consistent copy independent of the live file.
   The backup PVC retains 14 days of copies.

2. **PVC-level backup.** The `loomuxd-backup` PVC is listed in
   `infrastructure/base/pbs-backup/21-backup-script-configmap.yaml` and is
   snapshotted nightly by the Proxmox Backup Server (PBS) PVC backup job.

3. **Pre-migration snapshot.** Before each `loomuxd` pod start, an init
   container copies `/data/loomux.db` to
   `/data/loomux.db.pre-migration-<timestamp>`, keeping the last 5 copies.
   This protects against a failed or bad goose migration.

## Restore a backup

These steps assume you have access to a consistent backup file (either from
`/backup` in the running cluster or from PBS).

1. Suspend Flux for the loomux workloads, then stop `loomuxd` so nothing writes to
   the database while you restore. **Suspend first:** the Deployment is GitOps-managed
   with `replicas: 1`, so Flux scales it straight back up on its next reconcile
   (within ~10 minutes) — mid-restore, if you skip this. (Learned the hard way during
   the 2026-10-02 orphan-row cleanup.)
   ```bash
   flux suspend kustomization services -n flux-system
   kubectl -n loomux scale deployment loomuxd --replicas=0
   kubectl -n loomux wait --for=delete pod -l app=loomuxd --timeout=180s
   ```

2. Start a debug pod that mounts both the data and backup PVCs:
   ```bash
   kubectl -n loomux run loomux-restore --rm -it --restart=Never \
     --image=docker.io/keinos/sqlite3:3.46.1 \
     --overrides='{
       "spec": {
         "volumes": [
           {"name":"data","persistentVolumeClaim":{"claimName":"loomuxd-data"}},
           {"name":"backup","persistentVolumeClaim":{"claimName":"loomuxd-backup"}}
         ],
         "containers": [
           {
             "name":"restore",
             "image":"docker.io/keinos/sqlite3:3.46.1",
             "command":["sh"],
             "stdin":true,
             "tty":true,
             "volumeMounts": [
               {"name":"data","mountPath":"/data"},
               {"name":"backup","mountPath":"/backup"}
             ]
           }
         ]
       }
     }'
   ```

3. Verify and copy the desired backup over the live database:
   ```bash
   ls -la /backup/
   sqlite3 /backup/loomux-YYYYMMDD-HHMMSS.db "PRAGMA integrity_check;"
   cp -p /data/loomux.db /data/loomux.db.pre-restore-$(date +%Y%m%d-%H%M%S)  # keep what you're replacing
   cp /backup/loomux-YYYYMMDD-HHMMSS.db /data/loomux.db
   rm -f /data/loomux.db-wal /data/loomux.db-shm   # stale WAL/SHM from the old DB must not be replayed onto the restored one
   # The debug pod runs as root: restore the ownership/mode loomuxd (uid 10001, fsGroup 10001) needs,
   # or it starts but can't write ("attempt to write a readonly database").
   chown 1000:10001 /data/loomux.db && chmod 664 /data/loomux.db
   ls -la /data/
   ```

4. Exit the debug pod, scale `loomuxd` back up, and resume Flux:
   ```bash
   kubectl -n loomux scale deployment loomuxd --replicas=1
   kubectl -n loomux rollout status deployment/loomuxd
   flux resume kustomization services -n flux-system
   ```

5. Check that the pod starts cleanly and migrations run without errors:
   ```bash
   kubectl -n loomux logs deployment/loomuxd -c loomuxd --tail=50
   ```

## Restore from PBS

If the in-cluster backup PVC is also lost, restore the `loomuxd-backup`
archive from Proxmox Backup Server to a new PVC or a host path, then follow
the steps above using the restored `/backup` directory.
