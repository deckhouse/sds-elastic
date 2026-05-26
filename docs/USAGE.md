---
title: "Usage"
description: "Deploying and managing a Ceph cluster with the sds-elastic module: enabling companion modules, declaring an ElasticCluster + ElasticStorageClass, and consuming the resulting StorageClass."
weight: 50
---

## Enabling Required Modules

{{< alert level="warning" >}}
The `sds-elastic` module is in `Experimental` stage. Experimental modules are not enabled by default. Set `allowExperimentalModules: true` in the `deckhouse` ModuleConfig before enabling the module.
{{< /alert >}}

Enable `sds-elastic` together with its companion modules:

- [`sds-node-configurator`](/modules/sds-node-configurator/) — owns the `BlockDevice` and `LVMVolumeGroup` CRDs that `ElasticCluster` selects from.
- [`csi-ceph`](/modules/csi-ceph/) — owns the `CephClusterConnection` and `CephStorageClass` CRDs the controller writes into.
- [`snapshot-controller`](/modules/snapshot-controller/) — required for VolumeSnapshot support (optional).

```shell
d8 k apply -f - <<EOF
apiVersion: v1
kind: List
items:
  - apiVersion: deckhouse.io/v1alpha1
    kind: ModuleConfig
    metadata:
      name: sds-node-configurator
    spec:
      enabled: true
      version: 1
  - apiVersion: deckhouse.io/v1alpha1
    kind: ModuleConfig
    metadata:
      name: snapshot-controller
    spec:
      enabled: true
      version: 1
  - apiVersion: deckhouse.io/v1alpha1
    kind: ModuleConfig
    metadata:
      name: csi-ceph
    spec:
      enabled: true
      version: 1
  - apiVersion: deckhouse.io/v1alpha1
    kind: ModuleConfig
    metadata:
      name: sds-elastic
    spec:
      enabled: true
      version: 1
EOF
```

Wait until every module reaches the `Ready` state:

```shell
d8 k get module sds-node-configurator snapshot-controller csi-ceph sds-elastic -w
```

## Preparing Storage Nodes

`ElasticCluster` consumes `BlockDevice` CRs (managed by `sds-node-configurator`) selected by labels and provisions one OSD per matched device.

1. Pick the nodes that will host Ceph daemons and label them. The example uses `node-role.deckhouse.io/storage`:

   ```shell
   d8 k label node <node-name> node-role.deckhouse.io/storage=
   ```

1. Make sure each storage node has at least one unused raw block device (no partitions, filesystem, or LVM signatures). `sds-node-configurator` discovers them and creates a corresponding `BlockDevice` CR. Verify:

   ```shell
   d8 k get blockdevices.storage.deckhouse.io -o wide
   ```

1. Add a label that the `ElasticCluster` will use to select OSD-eligible devices. The example uses `app=elastic-osd`:

   ```shell
   d8 k label blockdevice <bd-name> app=elastic-osd
   ```

## Deploying an ElasticCluster

The example below bootstraps a Ceph cluster on every node carrying the `node-role.deckhouse.io/storage` label, consuming every `BlockDevice` labelled `app=elastic-osd`.

```shell
d8 k apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: ElasticCluster
metadata:
  name: ceph-prod
spec:
  storage:
    nodeSelector:
      matchExpressions:
        - { key: node-role.deckhouse.io/storage, operator: Exists }
    blockDeviceSelector:
      matchLabels:
        app: elastic-osd
  network:
    public: 10.12.0.0/16
    cluster: 10.12.0.0/16
EOF
```

Wait until the `ElasticCluster` reports `Ready`:

```shell
d8 k get elasticcluster ceph-prod -w
```

The `Phase` column is expected to switch from `Pending` to `InProgress` and finally to `Ready`. The full per-stage progression is exposed through conditions: `StorageReady` → `CephClusterReady` → `CredentialsReady` → `CsiCephReady` → aggregate `Ready`.

Verify the underlying objects:

```shell
d8 k get lvmvolumegroup -l sds-elastic.deckhouse.io/cluster=ceph-prod
d8 k get lvmlogicalvolume -l sds-elastic.deckhouse.io/cluster=ceph-prod
d8 k get pv -l sds-elastic.deckhouse.io/cluster=ceph-prod
d8 k -n d8-sds-elastic get pod -owide
```

The controller also creates an internal [ElasticClusterCredential](./cr.html#elasticclustercredential) that mirrors `rook-ceph-mon` Secret fields:

```shell
d8 k get elasticclustercredential ceph-prod -o yaml
```

## BlockDevice Adoption and Ownership

Once an `ElasticCluster` selects a `BlockDevice` for the first time, the controller patches it with the `sds-elastic.deckhouse.io/cluster=<cluster-name>` label. The label is the durable record of which cluster owns the device and drives several behaviors:

- **Single owner per BlockDevice.** If a `BlockDevice` matches the `blockDeviceSelector` of two `ElasticCluster` resources, the second one cannot adopt it. The controller refuses to overwrite the existing label and surfaces `StorageReady=False` with `Reason=OwnershipConflict` and a message listing each contested BD and its current owner. No LVMVolumeGroup, LVMLogicalVolume, or local PersistentVolume is created until every conflict is resolved — even free BDs in the selector remain unadopted while a conflict is pending.

  To resolve a conflict, decide which cluster should own the BD and clear the label on the other side:

  ```shell
  d8 k label blockdevice <bd-name> sds-elastic.deckhouse.io/cluster-
  ```

  Or remove the conflicting `ElasticCluster` entirely. The next reconcile picks the BD up.

- **Sticky adoption — adopted BlockDevices stay with the cluster.** Once a BD has been labelled by the controller, it remains part of the cluster's working set even if it later drifts out of `blockDeviceSelector` or `nodeSelector` (for example, the operator narrows the selector, the device's labels change, or its node is relabelled). This is intentional: the OSD on top of it is already provisioned, the local PV is bound to a specific node, and dropping it from the working set would shrink `CephCluster.spec.storageClassDeviceSets[0].count` and risk data unavailability. The cluster's OSD count is therefore monotonic for the lifetime of an `ElasticCluster` — it can grow when new BDs match the selector but never shrinks on its own.

  As a side effect, `sds-node-configurator` flips `BlockDevice.status.consumable` to `false` once a VG appears on the device. Sticky adoption prevents this from kicking the BD out of the working set on the very next reconcile.

- **Releasing a BlockDevice.** There is no automatic disown path on this experimental stage (planned as part of B20 — OwnerReferences and finalizer-driven teardown). To safely retire a BD from a cluster, either delete the entire `ElasticCluster` (the controller-managed objects are removed, see `Deleting Resources` below) or, if you must shrink one cluster only, manually delete the corresponding `LVMLogicalVolume` and `LVMVolumeGroup`, and only then clear the label:

  ```shell
  d8 k delete lvmlogicalvolume <name>
  d8 k delete lvmvolumegroup <name>
  d8 k label blockdevice <bd-name> sds-elastic.deckhouse.io/cluster-
  ```

  Doing this while pools still hold useful data risks losing replicas.

- **Editing the selectors after creation.** `ElasticCluster.spec.storage.nodeSelector` and `spec.storage.blockDeviceSelector` are editable after creation — `kubectl edit elasticcluster <name>` and adjust the matchers. The validating webhook on UPDATE enforces two safety rails:

  - **Orphan-guard.** If an edit would leave an already-adopted BD outside the new selector pair (its labels no longer match `blockDeviceSelector`, or its `status.nodeName` is no longer in the set produced by `nodeSelector`), the webhook rejects the request and lists the offending BDs. Adopted BDs cannot be released automatically — follow the manual procedure above first.
  - **Pre-flight conflict detection.** If a widening edit would pull in a BD already labelled by another `ElasticCluster`, the webhook rejects the request and reports the contested BDs along with their current owners. Resolve the conflict (clear the label, or delete the other EC) before retrying.

  `spec.network` remains immutable on UPDATE: changing the public/cluster CIDRs on a live cluster invalidates mon endpoints and host-network bindings, and there is no safe automatic remediation. To change the network configuration, delete and re-create the `ElasticCluster`.

## Declaring StorageClasses

Pools and the matching csi-ceph StorageClasses are declared per [ElasticStorageClass](./cr.html#elasticstorageclass). One ESC produces one Ceph pool + one `CephStorageClass` named after the ESC.

### RBD pool with default replication (3 replicas)

```shell
d8 k apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: ElasticStorageClass
metadata:
  name: ceph-prod-rbd
spec:
  clusterRef: ceph-prod
  type: RBD
  replication: ConsistencyAndAvailability
EOF
```

### CephFS pool with erasure coding (k=2, m=2)

```shell
d8 k apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: ElasticStorageClass
metadata:
  name: ceph-prod-cephfs
spec:
  clusterRef: ceph-prod
  type: CephFS
  replication: ErasureCodedCompact
EOF
```

`ErasureCodedCompact` requires at least 4 storage nodes and is rejected for `type: RBD` (csi-ceph does not yet provision RBD volumes on erasure-coded pools).

### Pool that survives two simultaneous host failures (`HighRedundancy`)

```shell
d8 k apply -f - <<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: ElasticStorageClass
metadata:
  name: ceph-prod-rbd-hr
spec:
  clusterRef: ceph-prod
  type: RBD
  replication: HighRedundancy
EOF
```

`HighRedundancy` produces a 4-replica pool (`size=4`, `min_size=2`, `requireSafeReplicaSize=true`):

- two simultaneous host failures keep I/O continuous (2 replicas equal `min_size`);
- a third simultaneous failure pauses I/O but does not lose data — Ceph backfills the surviving copy onto free cluster space and resumes;
- data loss only at the fourth simultaneous failure.

The mode requires at least **5 storage nodes** (4 for the pool's CRUSH placement at `failureDomain=host` and 5 to host a 5-mon quorum). The first time you create a `HighRedundancy` ESC against an `ElasticCluster`, the controller automatically promotes the underlying `CephCluster` to `mon.count=5`, `mgr.count=3` (the standard topology is `3, 2`). The promotion is **sticky**: deleting the last `HighRedundancy` ESC does NOT roll the counts back, because silently weakening a live cluster's fault-tolerance guarantee is unsafe.

A validating webhook gates ESC creation on the same thresholds so the sticky promotion cannot fire on an undersized cluster. CREATE of an ESC with `replication: HighRedundancy` is rejected when:

- the parent `ElasticCluster` referenced by `spec.clusterRef` does not exist;
- fewer than 5 nodes match `ElasticCluster.spec.storage.nodeSelector` (the 5-mon quorum floor);
- adopted `BlockDevice` resources of the parent EC live on fewer than 4 distinct nodes (the 4-replica CRUSH placement floor).

So the bootstrap order is fixed: apply the `ElasticCluster` first, wait until at least four storage nodes have adopted BDs (check via `kubectl get bd -l sds-elastic.deckhouse.io/cluster=<ec>` or `EC.status.phase=Ready`), and only then apply the `HighRedundancy` ESC. Trying to ship the EC and the HR ESC in the same `kubectl apply` is rejected by admission — the EC arrives first, but its adopted-BD set is still empty when the ESC admission runs.

The audit trail lives on `ElasticCluster.status.cephTopology`:

```shell
d8 k get elasticcluster ceph-prod -o jsonpath='{.status.cephTopology}'
# {"monCount":5,"mgrCount":3,"reason":"HighRedundancyESCPresent","lastPromotedAt":"2026-…"}
```

Possible `reason` values: `Standard`, `HighRedundancyESCPresent`, `StickyHighWaterMark`. To force a recompute (for example, after deliberately scaling down to a smaller cluster), clear the field via the status subresource and trigger a reconcile:

```shell
d8 k patch elasticcluster ceph-prod \
  --type=merge --subresource=status \
  -p '{"status":{"cephTopology":null}}'
```

Wait until each ESC reports `Ready`:

```shell
d8 k get elasticstorageclass -w
```

The conditions transition is `PoolReady` → `CsiStorageClassReady` → aggregate `Ready`.

Verify the resulting csi-ceph objects and Kubernetes StorageClasses:

```shell
d8 k get cephclusterconnection
d8 k get cephstorageclass
d8 k get sc
```

A `CephClusterConnection` named after the parent `ElasticCluster` (`ceph-prod`) and one `CephStorageClass` per `ElasticStorageClass` (`ceph-prod-rbd`, `ceph-prod-cephfs`) are expected. Each csi-ceph `CephStorageClass` produces a Kubernetes `StorageClass` with the same name, ready to be consumed by `PersistentVolumeClaim` resources.

The internal helm-managed `StorageClass` `sds-elastic-osd` (provisioner `kubernetes.io/no-provisioner`, `volumeBindingMode: WaitForFirstConsumer`) backs OSD-local `PersistentVolume`s and is intentionally not user-facing — `ElasticStorageClass` resources cannot reuse this name (the validating webhook rejects them).

## Deleting Resources

Delete an `ElasticStorageClass` to remove the corresponding pool and `CephStorageClass`:

```shell
d8 k delete elasticstorageclass ceph-prod-rbd
```

Delete the `ElasticCluster` to remove all controller-managed objects (LVMVolumeGroup / LVMLogicalVolume / local PV / Rook CephCluster) bound to it:

```shell
d8 k delete elasticcluster ceph-prod
```

{{< alert level="warning" >}}
Finalizer-based GC for ElasticCluster / ElasticStorageClass is planned (B20 in the backlog) but not yet implemented. On the experimental stage deletion clears controller-owned objects but does not orchestrate Rook teardown end-to-end; manual cleanup of OSD devices / `cephx` entries / leftover Rook CRs may be required. Do not delete the `ElasticCluster` while pools still hold useful data.
{{< /alert >}}

## Disabling the Module

{{< alert level="danger" >}}
Disabling the module stops the controller and the Rook operator. Data stored in Ceph clusters managed by this module may become unavailable or be lost. Always delete every `ElasticCluster`, `ElasticStorageClass` and `ElasticClusterCredential` object before disabling the module.
{{< /alert >}}

1. Delete every `ElasticStorageClass` and wait until the controller has removed the pools and csi-ceph StorageClasses:

   ```shell
   d8 k get elasticstorageclasses.storage.deckhouse.io
   ```

   Wait until the command returns `No resources found`.

1. Delete every `ElasticCluster` and wait for cluster teardown:

   ```shell
   d8 k get elasticclusters.storage.deckhouse.io
   ```

   Wait until the command returns `No resources found`.

1. Verify that no `ElasticClusterCredential` remains:

   ```shell
   d8 k get elasticclustercredentials.storage.deckhouse.io
   ```

1. Disable the module. Disabling requires the `modules.deckhouse.io/allow-disabling: "true"` label on the ModuleConfig:

   ```shell
   d8 k label moduleconfig sds-elastic modules.deckhouse.io/allow-disabling=true --overwrite
   d8 k patch moduleconfig sds-elastic --type=merge -p '{"spec":{"enabled":false}}'
   ```

## Checking Cluster Health

The controller exposes coarse-grained progress on each CR through conditions. For an `ElasticCluster`:

```shell
d8 k describe elasticcluster <cluster-name>
```

Useful conditions: `StorageReady`, `CephClusterReady`, `CredentialsReady`, `CsiCephReady`, `UpgradeReady`, `UpgradeInProgress`, and the aggregate `Ready`.

For an `ElasticStorageClass`:

```shell
d8 k describe elasticstorageclass <esc-name>
```

Useful conditions: `PoolReady`, `CsiStorageClassReady`, and the aggregate `Ready`.

For a deeper Ceph-level inspection, exec into a Rook toolbox pod:

```shell
d8 k -n d8-sds-elastic exec -it deploy/rook-ceph-tools -- ceph status
d8 k -n d8-sds-elastic exec -it deploy/rook-ceph-tools -- ceph osd tree
```
