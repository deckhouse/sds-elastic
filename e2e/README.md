# E2E tests for sds-elastic

End-to-end coverage for the documented `ElasticCluster` / `ElasticStorageClass`
lifecycle: creation + data round-trip, the data-safety deletion guards, and the
module disable guard (the `ModuleConfig` validating webhook).

1. `storage-e2e` brings up a nested cluster from `tests/cluster_config.yml`
   (1 master + 5 storage workers).
2. `BeforeSuite` attaches raw VirtualDisks to every worker, waits for
   `sds-node-configurator` to publish consumable `BlockDevice` CRs, then labels
   the storage nodes and OSD BlockDevices via
   `storage-e2e/pkg/testkit.EnsureElasticOSDBlockDevices`.
3. A single shared `ElasticCluster` is created (it bootstraps the vendored Rook
   `CephCluster` — ~15-25 min) and the ordered specs exercise create → data
   round-trip → deletion guards → module disable on top of it.
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
(`createSpecs → moduleDisableBlockedSpec → deleteSpecs → moduleDisableForceSpec
→ finalManualCleanupSpec`); the EC-destroying specs run last.
`RandomizeAllSpecs` stays **off**.

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
- `DKP_LICENSE_KEY`
- `REGISTRY_DOCKER_CFG`
- `MODULE_IMAGE_TAG`:
  expanded into `modulePullOverride` for `sds-elastic` in
  `tests/cluster_config.yml`. Set to `prN` on GitHub, `mrN` on GitLab, or
  `main` for nightly. **Must include the C3 disable-guard webhook** or the
  module_disable specs will fail. storage-e2e fails fast at config-load time if
  the variable is unset.

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
  container image (must ship `sh` + `cat`) for the PVC round-trip probe Pods,
  defaults to `busybox:1.36`.

## Quick start

```bash
export TEST_CLUSTER_CREATE_MODE=alwaysCreateNew
export TEST_CLUSTER_CLEANUP=true
export TEST_CLUSTER_NAMESPACE=e2e-sds-elastic
export TEST_CLUSTER_STORAGE_CLASS=linstor-r2

export SSH_HOST=<master-ip>
export SSH_USER=<ssh-user>
export SSH_PRIVATE_KEY=~/.ssh/id_rsa

export DKP_LICENSE_KEY=<license>
export REGISTRY_DOCKER_CFG=<base64-docker-config>

export MODULE_IMAGE_TAG=main   # or prN / mrN to test a specific PR/MR

cd e2e
make deps
make test
```

For local debugging you can run a subset of specs:

```bash
make test-focus FOCUS="create the shared ElasticCluster"
```

## Compile check (no cluster)

```bash
make build
make vet
```
