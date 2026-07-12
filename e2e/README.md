# E2E tests for sds-elastic

End-to-end coverage for the documented `ElasticCluster` / `ElasticStorageClass`
lifecycle: creation + data round-trip, `VolumeSnapshot` snapshot/restore,
`ReadWriteMany` multi-attach across nodes, the data-safety deletion guards, and
the module disable guard (the `ModuleConfig` validating webhook).

1. `storage-e2e` brings up a nested cluster from `tests/cluster_config.yml`
   (1 master + 5 storage workers).
2. `BeforeSuite` attaches raw VirtualDisks to every worker, waits for
   `sds-node-configurator` to publish consumable `BlockDevice` CRs, then labels
   the storage nodes and OSD BlockDevices via
   `storage-e2e/pkg/testkit.EnsureElasticOSDBlockDevices`.
3. A single shared `ElasticCluster` is created (it bootstraps the vendored Rook
   `CephCluster` — ~15-25 min) and the ordered specs exercise create → data
   round-trip → snapshots → RWX multi-attach → deletion guards → module disable
   on top of it.
4. `AfterSuite` cleans up leftovers and hands the cluster back to `storage-e2e`.

All Elastic/Rook CR provisioning lives in `storage-e2e/pkg/testkit` (the
Elastic helper layer) and `storage-e2e/pkg/kubernetes`; the suite imports them
via `require github.com/deckhouse/storage-e2e` in `e2e/go.mod`.

> **Module dependency order.** The suite is built on the new Elastic helper
> layer in `storage-e2e` (part A of the plan). During development `e2e/go.mod`
> carries a local `replace github.com/deckhouse/storage-e2e => ../../../../../e2e/repos/storage-e2e`.
> Once part A is merged and tagged, drop the `replace` and pin the suite to the
> published pseudo-version. The `sds-elastic/api` module is always consumed via
> `replace github.com/deckhouse/sds-elastic/api => ../api`.

## Why one shared ElasticCluster + Ordered specs

Creating an `ElasticCluster` runs a full Rook bootstrap and is far too slow to
repeat per spec, so the suite uses a **single shared EC** inside one
`Describe(..., Ordered)`. Spec registration goes through builder functions
called in explicit order from the root container
(`createSpecs → snapshotSpecs → rwxSpecs → moduleDisableBlockedSpec →
deleteSpecs → moduleDisableForceSpec → finalManualCleanupSpec`); the
EC-destroying specs run last. `RandomizeAllSpecs` stays **off**.

`snapshotSpecs` (`snapshot_test.go`) and `rwxSpecs` (`rwx_test.go`) run between
create and delete because they need the shared RBD/CephFS `ElasticStorageClass`
objects Ready. Both are careful to **fully reclaim** every PVC/PV, restore
PVC/PV and `VolumeSnapshot` they create (default `reclaimPolicy=Delete`), so the
later delete-guard specs still see only the createSpecs probes bound — a
leftover RBD image would flip `BoundVolumesExist` into `DataPresentInPool`, and a
leftover CephFS subvolume would break the `FilesystemNotEmpty` guard.

### Snapshot specs (`snapshot_test.go`)

For each of RBD Filesystem, RBD Block and CephFS Filesystem (CephFS+Block is
skipped — `cephfs.csi.ceph.com` is Filesystem-only) the suite: writes data
(several files, or a random prefix of a raw block device) and records a
checksum; creates a `VolumeSnapshot` (using the `VolumeSnapshotClass` csi-ceph
auto-creates per `ElasticStorageClass`, named identically to it) and waits for
`readyToUse`; restores a new PVC from the snapshot and asserts the checksum
matches; writes new data to the source and asserts the restored copy is
unchanged (isolation); then deletes the source and proves the restored volume is
still writable.

### RWX specs (`rwx_test.go`)

For RBD Block and CephFS Filesystem the suite provisions a `ReadWriteMany` PVC,
schedules two Pods with **required** pod anti-affinity
(`topologyKey: kubernetes.io/hostname`) so they land on different nodes, asserts
`spec.nodeName` differs, and proves bidirectional shared access (write from one
Pod, read back from the other; Block reads use `dd iflag=direct` to bypass the
reader node's page cache). RBD RWX is Block-only (a shared filesystem on one RBD
image is unsafe).

## Supported run mode

Only the nested-cluster mode driven by `storage-e2e` is supported.

## Requirements

- Go **1.26+**
- A base Deckhouse cluster with the `virtualization` module enabled.
- SSH access to the master node of the base cluster.
- A Deckhouse license and a docker config for the dev registry.
- A block-mode `StorageClass` on the base cluster for VM disks (OS disk + the
  raw OSD disks attached at runtime).

## Environment variables

### `storage-e2e` (nested cluster)

- `TEST_CLUSTER_CREATE_MODE` (**required**):
  one of `alwaysCreateNew`, `alwaysUseExisting`, `commander`.
- `TEST_CLUSTER_CLEANUP`:
  set to `true` to delete the VMs after the run.
- `TEST_CLUSTER_NAMESPACE`:
  the VM namespace on the base cluster **and** the in-cluster namespace the
  suite uses for PVCs/Pods (single source of truth — no separate
  `E2E_NAMESPACE`).
- `TEST_CLUSTER_STORAGE_CLASS`:
  base-cluster `StorageClass` for VM disks (OS + raw OSD disks).
- `YAML_CONFIG_FILENAME`:
  defaults to `cluster_config.yml`.
- `SSH_HOST`, `SSH_USER`, `SSH_PRIVATE_KEY`
- `SSH_PUBLIC_KEY`:
  path to (or inline content of) the SSH public key injected as the VMs'
  authorized key. **Required in `alwaysCreateNew` mode** — set it explicitly
  (e.g. `~/.ssh/id_rsa.pub`); the suite errors with `SSH_PUBLIC_KEY is not set`
  during VM creation if it is empty.
- `SSH_VM_USER`:
  SSH user inside the created VMs (must match the VM image, usually `cloud`).
  **Required in `alwaysCreateNew` mode** — `storage-e2e` does not apply a
  default, so an empty value makes the post-create SSH fail with
  `unable to authenticate [none publickey]` and `SSH_VM_USER (current="")`.
- `SSH_JUMP_HOST`, `SSH_JUMP_USER`, `SSH_JUMP_KEY_PATH`:
  jump-host (bastion) SSH settings used by `alwaysUseExisting`. When the target
  cluster master is only reachable through a bastion — e.g. a nested cluster
  whose master sits behind the base hypervisor — set `SSH_HOST`/`SSH_USER` to
  the **target cluster master** and these to the bastion. Ignored by
  `alwaysCreateNew`.
- `TEST_CLUSTER_FORCE_LOCK_RELEASE`:
  set to `true` to steal a stale `e2e-cluster-lock` ConfigMap left in the
  `default` namespace by a previous crashed/`Ctrl+C` run. Only use when you are
  sure no other run is using the cluster.
- `DKP_LICENSE_KEY`
- `REGISTRY_DOCKER_CFG`
- `SDS_ELASTIC_MODULE_PULL_OVERRIDE`:
  overrides `modulePullOverride` for `sds-elastic` from `tests/cluster_config.yml`
  (which keeps a literal `main` default). Set to `prN` on GitHub, `mrN` on
  GitLab, or `main` for nightly. **The image must include the C3 disable-guard
  webhook** or the module_disable specs will fail. When unset, the static `main`
  tag is used; when set, storage-e2e logs both the static tag and this override
  at config-load time. (This is storage-e2e's generic per-module convention:
  `<MODULE>_MODULE_PULL_OVERRIDE`, e.g. `CSI_CEPH_MODULE_PULL_OVERRIDE`.)

### `sds-elastic` suite knobs

- `E2E_EC_NAME`:
  name of the shared `ElasticCluster`, defaults to `sds-elastic-e2e`.
- `E2E_PVC_SIZE`:
  PVC size for round-trip probes, defaults to `1Gi`.
- `E2E_ROOK_NAMESPACE`:
  namespace the vendored Rook / CephCluster live in, defaults to
  `d8-sds-elastic`.
- `E2E_OSD_BD_LABEL`:
  label (`key` or `key=value`) applied to OSD BlockDevices and used as the EC
  `blockDeviceSelector`. Defaults to the storage-e2e Elastic-layer default.
- `E2E_STORAGE_NODE_LABEL`:
  label (`key` or `key=value`) applied to storage nodes and used as the EC
  `nodeSelector`. Defaults to the storage-e2e Elastic-layer default.
- `E2E_NETWORK_PUBLIC`, `E2E_NETWORK_CLUSTER`:
  optional pins for `ElasticCluster.spec.network.{public,cluster}`.
- `E2E_EC_READY_TIMEOUT`:
  Go duration bounding the EC Ready wait, defaults to 25m.
- `E2E_ESC_READY_TIMEOUT`:
  Go duration bounding the ESC Ready wait, defaults to 10m.
- `E2E_OSD_DISKS_PER_WORKER`:
  number of raw VirtualDisks attached to each worker for OSDs, defaults to `2`.
- `E2E_OSD_DISK_SIZE`:
  size of each raw OSD VirtualDisk, defaults to `20Gi`.
- `E2E_PROBE_IMAGE`:
  container image for the PVC round-trip / snapshot / RWX probe Pods, defaults
  to `busybox:1.36`. Must ship `sh`, `cat`, `dd` (with `iflag=direct`/`oflag`
  support), `sha256sum`, `sync`, `find`, `sort` and `head`.
- `E2E_KEEP_CLUSTER_ON_FAILURE`:
  when truthy (`true`/`1`/`yes`), and at least one spec failed, the nested
  cluster is **not** torn down in `AfterSuite`, so you can inspect the live
  cluster. The suite prints a banner with the namespace, EC, Rook namespace,
  kubeconfig path and first master IP. Remember to delete the VMs manually
  afterwards. Defaults to off.

## Quick start

```bash
export TEST_CLUSTER_CREATE_MODE=alwaysCreateNew
export TEST_CLUSTER_CLEANUP=true
export TEST_CLUSTER_NAMESPACE=e2e-sds-elastic
export TEST_CLUSTER_STORAGE_CLASS=linstor-r2

export SSH_HOST=<master-ip>
export SSH_USER=<ssh-user>
export SSH_PRIVATE_KEY=~/.ssh/id_rsa
export SSH_PUBLIC_KEY=~/.ssh/id_rsa.pub   # required for alwaysCreateNew (VM authorized key)
export SSH_VM_USER=cloud                  # required for alwaysCreateNew (SSH user inside the VMs)

export DKP_LICENSE_KEY=<license>
export REGISTRY_DOCKER_CFG=<base64-docker-config>

# Override the sds-elastic image tag (the C3 webhook build); optional, defaults
# to the literal "main" in cluster_config.yml.
export SDS_ELASTIC_MODULE_PULL_OVERRIDE=main   # or prN / mrN to test a specific PR/MR

cd e2e
make deps
make test
```

For local debugging you can run a subset of specs:

```bash
make test-focus FOCUS="create the shared ElasticCluster"
```

## CI

`.github/workflows/e2e-tests.yml` is a thin caller for the reusable
`deckhouse/storage-e2e/.github/workflows/e2e.yml@main` pipeline
(`cluster_provider: commander`, `cluster_config: e2e/tests/cluster_config.ci.yml`).
It only fires when the PR carries the `e2e/commander/run` label. Other labels:
`e2e/keep-cluster` (skip teardown), `e2e/label:<suite>` (Ginkgo label filter).
The Commander template pre-provisions the storage workers and their raw OSD
block devices (>=4), so in the commander flow `BeforeSuite` skips the runtime
VirtualDisk attach and adopts the devices the template already exposes. The
module image under test is the PR's `pr<N>` tag published by `build_dev`.

## Compile check (no cluster)

```bash
make build
make vet
```
