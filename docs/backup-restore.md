# Bucket backup and restore

Bucket backup and restore copies PVC data to object storage and restores it later.
An rclone job inside the cluster does the copy.
The built-in backends are S3-compatible storage, Azure Blob and GCS, and a raw rclone config covers everything else rclone can reach.

See the [CLI reference](cli-reference.md#backup) for all flags.

## Managed bucket mode

In managed mode, `pv-migrate` builds the rclone config from its own flags.
Use it when your backend is one of the built-in ones.

S3-compatible backup:

```bash
pv-migrate backup \
  --source app-data \
  --source-namespace app \
  --backend s3 \
  --bucket pv-backups \
  --endpoint https://s3.example.com \
  --access-key "$ACCESS_KEY" \
  --secret-key "$SECRET_KEY" \
  --name app-data-2026-04-11
```

Restore from that backup:

```bash
pv-migrate restore \
  --dest app-data-restore \
  --dest-namespace app \
  --backend s3 \
  --bucket pv-backups \
  --endpoint https://s3.example.com \
  --access-key "$ACCESS_KEY" \
  --secret-key "$SECRET_KEY" \
  --name app-data-2026-04-11
```

By default, restore copies the backup into the destination and keeps the destination files that are not in the backup.
To make the destination an exact mirror of the backup, opt in to deletion:

```bash
pv-migrate restore \
  --dest app-data-restore \
  --backend s3 \
  --bucket pv-backups \
  --name app-data-2026-04-11 \
  --delete-extraneous-files
```

### Credentials

Credentials can be passed as flags or as environment variables.
A flag wins over the environment variable.

Prefer the environment variables in automation and on shared machines, so that the credentials do not show up in the process list.

Supported environment variables:

- S3: `PV_MIGRATE_S3_ACCESS_KEY`, `PV_MIGRATE_S3_SECRET_KEY`
- Azure: `PV_MIGRATE_AZURE_STORAGE_ACCOUNT`, `PV_MIGRATE_AZURE_STORAGE_KEY`
- GCS: `PV_MIGRATE_GCS_SERVICE_ACCOUNT_JSON`

For GCS, `PV_MIGRATE_GCS_SERVICE_ACCOUNT_JSON` holds the JSON credentials themselves, not a path.
Use `--gcs-service-account-file` to pass a local file instead.

Managed S3 mode uses rclone's generic `Other` provider by default.
Leave it unless your provider needs another rclone mode, and set `--s3-provider` in that case.

Managed GCS mode defaults to `bucket_policy_only = true`.
Set `--gcs-bucket-policy-only=false` for legacy buckets that still use object ACLs.

`--name` identifies the backup inside the bucket.
The default prefix is `pv-migrate`, and prefixes can contain `/` for nesting:

```bash
pv-migrate backup \
  --source app-data \
  --backend s3 \
  --bucket pv-backups \
  --prefix teams/payments/prod \
  --name app-data-2026-04-11
```

## Object layout

In managed mode, backup data is stored under:

```text
<bucket>/<prefix>/<name>/
```

The backup metadata sidecar is stored at:

```text
<bucket>/<prefix>/<name>.meta.yaml
```

The metadata records the backup time and the source PVC.
It is there for inspection, restore does not need it.

For example:

```text
pv-backups/pv-migrate/app-data-2026-04-11/
pv-backups/pv-migrate/app-data-2026-04-11.meta.yaml
```

## Raw rclone config mode

Use raw rclone config mode when you need a backend or an rclone option that `pv-migrate` does not model.
In this mode, `--remote` is the full source or destination path, and `--name`, `--bucket` and `--prefix` are not used to build it.

```bash
pv-migrate backup \
  --source app-data \
  --rclone-config ./rclone.conf \
  --remote myremote:bucket/custom/path
```

Restore with the same raw remote:

```bash
pv-migrate restore \
  --dest app-data-restore \
  --rclone-config ./rclone.conf \
  --remote myremote:bucket/custom/path
```

Managed mode writes the metadata sidecar after the data upload succeeds.
Raw rclone config mode does not, because `pv-migrate` treats the remote as an opaque rclone path.

## Subdirectory backup and restore

Use `--path` to back up or restore a subdirectory inside the PVC:

```bash
pv-migrate backup \
  --source app-data \
  --path uploads \
  --backend s3 \
  --bucket pv-backups \
  --name uploads-2026-04-11
```

The same flag restores into a subdirectory on the target PVC:

```bash
pv-migrate restore \
  --dest app-data-restore \
  --path uploads \
  --backend s3 \
  --bucket pv-backups \
  --name uploads-2026-04-11
```

## Detached mode and progress

Use `--detach` for long backup or restore jobs:

```bash
pv-migrate backup \
  --source app-data \
  --backend s3 \
  --bucket pv-backups \
  --name app-data-2026-04-11 \
  --detach \
  --id app-backup

pv-migrate status app-backup
pv-migrate status app-backup --follow
pv-migrate cleanup app-backup
```

Attached backup and restore runs and `status --follow` read rclone's JSON stats output to show the progress.
To try that out, throttle rclone with `--rclone-extra-args`:

```bash
pv-migrate backup \
  --source app-data \
  --backend s3 \
  --bucket pv-backups \
  --name slow-test \
  --detach \
  --id slow-test \
  --rclone-extra-args '--bwlimit 1M --transfers 1'
```

`--rclone-extra-args` is appended after the rclone flags `pv-migrate` sets itself.
It is for the rclone options that have no flag of their own.
Overriding the built-in stats or JSON log flags breaks the progress parsing.

When `--dry-run`, `--dry-run=true` or `-n` is among the extra arguments, `pv-migrate` also skips writing the metadata sidecar, so a dry run does not change the bucket.

## Archive to a file instead of a bucket

`--archive-file` writes the whole volume as one tar file, rather than syncing it file by file to a bucket.
Its value names where the file goes.

```bash
pv-migrate backup \
  --source mysql-data \
  --archive-file 'nfs:/backups/mysql-%Y-%m-%d_%H%M.tar.zst'
```

A value of the form `s3://<bucket>/<key>` streams the archive into that one object.
tar writes to rclone on a pipe inside the job, so the file never lands on a disk in the pod and needs no second claim.
The credentials come from the same S3 flags and `PV_MIGRATE_S3_*` environment variables the bucket workflow uses, and `--backend` is implied.
An upload of unknown size cannot grow its chunk to fit, so the object is capped at the chunk size times S3's 10,000-part limit, 48 GiB at rclone's default.
The job therefore measures the volume's used bytes as it starts and sizes the chunk so the stream fits under 9,000 parts, assuming nothing compresses and allowing a quarter more for tar's own overhead and writes during the backup.
Each in-flight chunk is held in the job pod's memory, so a multi-terabyte volume means chunks of a few hundred MiB and a pod that needs that much RAM times rclone's upload concurrency.
`--rclone-extra-args` is forwarded to the rclone side of the pipe, after the computed chunk size, so an explicit `--s3-chunk-size` there overrides it.
A dry run in those args uploads nothing and writes no sidecar.

```bash
pv-migrate backup \
  --source mysql-data \
  --endpoint https://s3.example.com --access-key ... --secret-key ... \
  --archive-file 's3://backups/mysql/mysql-%Y-%m-%d_%H%M.tar.zst'
```

A value of the form `<claim>:<path>` puts the file on that claim, at that path inside it.
The claim is mounted into the job next to the volume being backed up.
A bare path with no claim in front of it is a path inside the job's own container, for a volume you mounted there yourself with a `--helm-values` file that lists it under `rclone.pvcMounts` next to the data mount.
Nothing is mounted at a bare path automatically, and `pv-migrate` warns about that, because an unmounted path is ephemeral storage that goes away with the pod.

Compression follows the extension: `.tar.zst` or `.tzst` is zstd, `.tar.gz` or `.tgz` is gzip, `.tar` is none.
`--compression-level` overrides the compressor's own default.

Timestamp tokens in the path are expanded when the command runs, against the current time in UTC: `%Y`, `%m`, `%d`, `%H`, `%M` and `%S`, with `%%` for a literal percent sign.
The example above writes a file such as `mysql-2026-09-14_0307.tar.zst`.
No tokens means no timestamp.
Nothing is added that you did not write.

A `.meta.yaml` sidecar is written next to the archive, named after it, recording the source claim, the time, the format and the compression.

Restore takes the same flag with the name of the file to read.
Tokens are not useful there, since they would name a file that does not exist.
`--delete-extraneous-files` is refused on an archive restore, since tar restores what the archive holds and removes nothing.

```bash
pv-migrate restore \
  --dest mysql-data \
  --archive-file nfs:/backups/mysql-2026-09-14_0307.tar.zst
```

The bucket flags are refused alongside `--archive-file`, since they would name a second destination for the same data.

Both claims are mounted by one pod, so they have to be mountable together.
A `ReadWriteOnce` volume is tied to the node that has it mounted, so two of them already mounted on different nodes cannot work, and `pv-migrate` says so before it creates anything.
One claim already mounted and one free is fine: the job is pinned to the node holding the mounted one.

Unlike the bucket workflow, an archive preserves the POSIX owner, group and mode, along with extended attributes, hard links and sparse files.
A database volume restored from a bucket comes back owned by the wrong user.
Restored from an archive it comes back as it was.

## Backing up a point in time

`--snapshot` cuts a VolumeSnapshot of the claim, backs up a clone of it, and removes both afterwards.
The backup then reads data as it was at one instant rather than a volume that is changing underneath it.

```bash
pv-migrate backup \
  --source mysql-data \
  --snapshot \
  --archive-file 'nfs:/backups/mysql-%Y-%m-%d_%H%M.tar.zst'
```

It needs the VolumeSnapshot CRDs and their controller installed, which the Kubernetes version alone does not guarantee, and a VolumeSnapshotClass.
`--snapshot-class` names the class, and an unset one uses the cluster default.
`--keep-snapshot` leaves the VolumeSnapshot behind once the backup is done.
`--from-snapshot` names a VolumeSnapshot that already exists and backs up a clone of that instead of cutting a new one.

The clone is a new claim in the same namespace, with the source's storage class, access modes and size.
The snapshot API has no in-place revert, so that is the only shape a restore from a snapshot can take.
The live claim being mounted is not a conflict here, since the job mounts the clone and never touches the live volume.

The snapshot and the clone carry the run's instance label, so `pv-migrate cleanup` removes them along with the release if the run is detached, is told not to clean up, or dies before it can.

### Quiescing a database

A snapshot of a database volume is crash-consistent on its own: it is the same as pulling the power, and a database recovers from that through its log.
What a snapshot cannot exclude is a schema change in flight, and for some databases a coherent replication position.
`--flush` closes that gap by taking the database's own backup lock while the snapshot is cut.

```bash
pv-migrate backup \
  --source mysql-data \
  --flush mysql \
  --archive-file 'nfs:/backups/mysql-%Y-%m-%d_%H%M.tar.zst'
```

`--flush` implies `--snapshot` and names the kind of database.
The lock is held for the snapshot cut only, a matter of seconds, and never for the backup, which reads the clone after the lock is gone.

The sequence is: open a client in the database's own pod, take the lock and wait for the client to confirm it, create the VolumeSnapshot, wait until the storage system reports the cut, release the lock, then wait for the snapshot to become usable, clone it and back the clone up.
The lock is released on every failure path after it is taken.

The database's pod is found as the one that has the claim mounted, so there is nothing to name.
`--flush-container` picks the container when that pod has more than one.
The client runs inside that container and takes its credentials from the environment the image already provides, so nothing is passed on a command line.
`--flush-command` replaces the client command, for an image that keeps its client or its credentials somewhere else.
It applies to the kinds that run one client; `mongodb` runs three, to lock, unlock and check, and refuses it.

What each kind does, and what it protects:

| Kind | What runs | Protects |
|---|---|---|
| `mysql` | `LOCK INSTANCE FOR BACKUP`, held in one session across the cut | Blocks DDL, writes continue. Needs `BACKUP_ADMIN`. InnoDB recovers the rest from its redo log. |
| `mariadb` | `BACKUP STAGE START` then `BLOCK_COMMIT`, held in one session, then `END` | Flushes non-transactional tables, blocks DDL, holds commits so the cut is a clean commit boundary. Needs `RELOAD`. |
| `postgres` | `CHECKPOINT`, once, nothing held | Shortens recovery only. The snapshot is crash-consistent through the WAL regardless. Needs superuser or `pg_checkpoint`. |
| `mongodb` | `db.fsyncLock()` before the cut, `db.fsyncUnlock()` after | Flushes, checkpoints and blocks writes. The lock is counted on the server, so the unlock's own reported count is checked: anything but zero means a lock leaked by an earlier run is still held, and the backup fails rather than leaving the database blocked. |
| `scylladb` | `nodetool flush`, once, nothing held | Moves memtables into sealed SSTables. The commitlog replays what arrives after. |

Three of the five hold nothing.
PostgreSQL and ScyllaDB run one command and are done; MongoDB's lock lives on the server rather than in a connection, so it is set and cleared by two separate commands.
MySQL and MariaDB keep the lock only as long as the session that took it, so for those the client is held open on its stdin from the lock until the cut is confirmed.

A few facts worth knowing before relying on a kind:

- `postgres`: `pg_backup_start` is deliberately not used.
  Its label file makes recovery insist on a record written after the cut, which a snapshot cannot contain, and the restore fails with "WAL ends before end of online backup".
  Unlogged tables are truncated on any crash, snapshot included.
  A database with its WAL on a separate claim needs a VolumeGroupSnapshot, which this tool does not do.
- `mongodb`: point it at a hidden secondary, not the primary.
  A run fails if the database is still locked after its unlock, which means an earlier run leaked one; clear it with `db.fsyncUnlock()` until the count reaches zero.
  The credentials go on the client's command line inside the pod, since `mongosh` has no environment variable for a password.
  After a restore, delete the stale `mongod.lock`; for a replica set, drop the `local` database and re-initiate.
- `scylladb`: the flush is per node.
  Consistency across nodes is restored afterwards by `nodetool repair`, not captured at backup time.
- `mariadb`: the client runs with `--skip-reconnect`, because its default reconnect would silently drop the backup stages on a blip and let the cut proceed unprotected.

Grafana and Prometheus need no `--flush`, and none is offered.
Grafana's SQLite runs with the default rollback journal and full sync, which is built to survive power loss, so a snapshot is a consistent database that rolls back any half-finished transaction on next open.
Prometheus starts from a raw snapshot too: its blocks are immutable, its WAL is checksummed and repaired by truncation, and its head chunks are rebuilt from the WAL.
What a raw Prometheus snapshot can lose is the tail of the WAL not yet written back by the kernel.
If that matters, call its snapshot API (`POST /api/v1/admin/tsdb/snapshot`, which needs `--web.enable-admin-api`) just before the backup; it writes the in-memory head out as a real block under `data/snapshots/`, and that directory has to be removed afterwards by hand.

## Permissions and ownership

Bucket backup and restore copies file contents only.
It does not preserve the POSIX owner, group or mode.
Restored files belong to the user the rclone process runs as, and regular files come back with default permissions such as `0644`.

Use `--archive-file` or PVC-to-PVC migration if the owners, groups or modes have to survive the copy.

## Scheduled backups

`pv-migrate backup` can run from a Kubernetes `CronJob`, which gives you scheduled PVC backups to object storage with Kubernetes building blocks only.

> [!WARNING]
> This is not a backup platform. `pv-migrate` does not manage retention, backup catalogs, restore checks, alerting, encryption or application consistency.
> Pause or snapshot the application before the backup if it needs a consistent copy.
> Use bucket lifecycle rules for retention, and your own monitoring.

The Role above is namespace-scoped, which is all a bucket backup needs.

Two things live outside a namespace and so cannot be granted by it. Reading a volume's topology needs `get` on `persistentvolumes` and `list` on `nodes`, and cutting a snapshot reads `volumesnapshotcontents` and `volumesnapshotclasses`. Without them `pv-migrate` still runs: it cannot tell you up front that two claims are pinned to different nodes, and leaves that to the scheduler. Grant them with a ClusterRole when you want the earlier, clearer failure, or when using `--snapshot`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: pv-migrate-cluster
rules:
  - apiGroups: [""]
    resources: ["persistentvolumes", "nodes"]
    verbs: ["get", "list"]
  - apiGroups: ["snapshot.storage.k8s.io"]
    resources: ["volumesnapshotcontents", "volumesnapshotclasses"]
    verbs: ["get", "list"]
```

The example below runs a nightly S3-compatible backup.
It uses the name of the `Job` the CronJob creates as the backup name, so every run writes to its own prefix.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: pv-migrate-backup-s3
  namespace: app
type: Opaque
stringData:
  access-key: replace-me
  secret-key: replace-me
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: pv-migrate-backup
  namespace: app
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: pv-migrate-backup
  namespace: app
rules:
  - apiGroups: [""]
    resources: ["persistentvolumeclaims"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["pods", "pods/log"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["secrets", "serviceaccounts"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["networking.k8s.io"]
    resources: ["networkpolicies"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["list", "watch"]
  # Needed only by --flush, which runs a client in the database's own pod.
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]
  # Needed only by --snapshot, --from-snapshot and --flush.
  - apiGroups: [""]
    resources: ["persistentvolumeclaims"]
    verbs: ["create", "delete"]
  - apiGroups: ["snapshot.storage.k8s.io"]
    resources: ["volumesnapshots"]
    verbs: ["get", "list", "watch", "create", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: pv-migrate-backup
  namespace: app
subjects:
  - kind: ServiceAccount
    name: pv-migrate-backup
    namespace: app
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: pv-migrate-backup
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: app-data-backup
  namespace: app
spec:
  schedule: "0 2 * * *"
  concurrencyPolicy: Forbid
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 3
  jobTemplate:
    spec:
      template:
        spec:
          serviceAccountName: pv-migrate-backup
          restartPolicy: Never
          containers:
            - name: pv-migrate
              image: docker.io/utkuozdemir/pv-migrate:<version>
              env:
                - name: PV_MIGRATE_S3_ACCESS_KEY
                  valueFrom:
                    secretKeyRef:
                      name: pv-migrate-backup-s3
                      key: access-key
                - name: PV_MIGRATE_S3_SECRET_KEY
                  valueFrom:
                    secretKeyRef:
                      name: pv-migrate-backup-s3
                      key: secret-key
                - name: BACKUP_NAME
                  valueFrom:
                    fieldRef:
                      fieldPath: metadata.labels['batch.kubernetes.io/job-name']
              args:
                - backup
                - --source=app-data
                - --source-namespace=app
                - --ignore-mounted
                - --backend=s3
                - --bucket=pv-backups
                - --endpoint=https://s3.example.com
                - --prefix=scheduled/app
                - --name=$(BACKUP_NAME)
```

Notes:

- Replace `<version>` with the release tag you want to run. The image has no shell, so pass the arguments as `args`, as shown.
- The `Job` name is passed as `--name`, so each run writes to its own backup prefix.
- Retention and application consistency are not handled. Use bucket lifecycle policies for retention, and pause or snapshot workloads that need a consistent copy.

## Non-root mode

`backup` and `restore` support `--non-root`.
The rclone container then runs as UID/GID `10000`, with `fsGroup` set to `10000`.

Use it on clusters that enforce restricted pod security.
It comes with the usual non-root filesystem constraints:

- Backup can fail if files are not readable by UID/GID `10000`.
- Restore can fail if the destination volume is not writable by UID/GID `10000` or if the CSI driver does not honor `fsGroup`.

For further customization of the generated manifests, see the [Helm chart values](../internal/helm/pv-migrate).
