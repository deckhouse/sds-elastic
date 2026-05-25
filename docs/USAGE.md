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
