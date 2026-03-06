# Maintenance Guide

## 1. Why k3s Uses kine SQLite

k3s is a lightweight Kubernetes distribution that replaces etcd with **kine**, a shim
layer that translates the etcd v3 API into SQL. By default, kine stores all cluster state
in a single SQLite file:

```
/var/lib/rancher/k3s/server/db/state.db
```

This makes k3s suitable for single-node and edge deployments because:

- No separate etcd process to manage or monitor.
- No cluster quorum required (single node is enough).
- The SQLite file is easy to back up (copy the file while k3s is stopped).
- Low memory footprint compared to a full etcd cluster.

**Trade-off:** SQLite is not designed for high write concurrency. In environments with
many active controllers or frequent object churn (e.g., cert-manager continuously
reconciling ACME challenges), the WAL file can grow large and slow down reads.

---

## 2. When Database Maintenance Is Needed

Routine maintenance is not required under normal, stable workloads. Perform maintenance
when you observe:

- The WAL file (`state.db-wal`) is significantly larger than `state.db` itself.
- `kubectl` commands respond slowly or time out.
- k3s logs show `SQLITE_BUSY` or lock contention errors.
- Disk usage in `/var/lib/rancher/k3s/server/db/` grows unexpectedly.

On this cluster, the primary historical cause of WAL growth was cert-manager
continuously writing ACME challenge state. cert-manager has been removed; under current
workloads the database should remain stable.

---

## 3. WAL Checkpoint Explanation

SQLite in WAL (Write-Ahead Logging) mode writes new changes to a separate file
(`state.db-wal`) rather than modifying the main database file directly. This allows
concurrent reads while a write is in progress.

A **checkpoint** moves committed WAL frames back into the main database file. Until a
checkpoint runs, the WAL file keeps growing.

The `TRUNCATE` checkpoint mode:

```sql
PRAGMA wal_checkpoint(TRUNCATE);
```

This performs a full checkpoint and then truncates the WAL file to zero bytes if all
frames were successfully transferred. It is the most aggressive mode and the correct
choice for maintenance windows.

**Requirement:** k3s must be stopped before running a checkpoint on the live database,
because k3s holds a write lock on `state.db` while running.

---

## 4. VACUUM Explanation

Over time, as Kubernetes objects are created and deleted, SQLite accumulates free pages
inside the database file — space that was used by deleted rows but not returned to the
filesystem. The file size does not shrink automatically.

`VACUUM` rewrites the entire database into a new file, compacting it and releasing free
pages back to the filesystem:

```sql
VACUUM;
```

Run `VACUUM` after a checkpoint for maximum space recovery. Like the checkpoint,
VACUUM requires k3s to be stopped first.

---

## 5. Operational Precautions

- **Always stop k3s before touching the database.** Running SQLite tools against a live
  database that k3s holds open risks corruption.
- **Back up the database file before any maintenance** if the cluster state is valuable.
- **Verify k3s restarts cleanly** after maintenance before declaring success.
- Do not run `VACUUM` frequently on large databases — it rewrites the entire file and can
  take several minutes plus significant disk I/O.

### Maintenance procedure

```bash
# 1. SSH into the server
ssh <SSH_USER_HERE>@<SERVER_IP_HERE>

# 2. Stop k3s (waits for graceful shutdown)
sudo systemctl stop k3s

# 3. Verify k3s is stopped
sudo systemctl is-active k3s
# Expected: inactive

# 4. Run WAL checkpoint (moves WAL frames into main DB, then truncates WAL)
sudo sqlite3 /var/lib/rancher/k3s/server/db/state.db \
  "PRAGMA wal_checkpoint(TRUNCATE);"

# 5. Run VACUUM (compacts the main DB file)
sudo sqlite3 /var/lib/rancher/k3s/server/db/state.db "VACUUM;"

# 6. Restart k3s
sudo systemctl start k3s

# 7. Verify k3s is healthy (wait ~30s for the API server to start)
sudo systemctl is-active k3s
sudo k3s kubectl get nodes
```

Do not include passwords in scripts or shell history. Use interactive SSH sessions for
any privileged maintenance operations.

---

## 6. Future Improvement: etcd Backend

For production clusters or multi-node setups, replacing kine SQLite with a proper etcd
cluster (or an external relational database via kine) eliminates the WAL growth problem
entirely.

To migrate:

1. Provision a 3-node etcd cluster (or use a managed etcd service).
2. Reconfigure k3s with `--datastore-endpoint=<etcd-url>` in the k3s systemd unit.
3. Migrate existing cluster state using `kine migrate` or by re-bootstrapping.

For a single-node demo or edge deployment, kine SQLite with periodic maintenance is
appropriate. Reserve etcd for clusters that require high availability or handle heavy
controller churn.
