---
title: "sds-elastic module"
description: "sds-elastic module: distributed block storage on top of the Rook Ceph operator."
weight: 1
---

{{< alert level="warning" >}}
The module is in `Experimental` stage. The API, configuration and Custom
Resources may change without notice; do not rely on it for production
workloads.
{{< /alert >}}

The `sds-elastic` module deploys and manages the
[Rook Ceph operator](https://rook.io) in a Deckhouse Kubernetes cluster,
turning a set of nodes into a distributed block storage backend backed by
Ceph.

The module ships the Rook Ceph operator Deployment, the
`rook-ceph-operator-config` ConfigMap and the full set of Ceph CRDs.
A single [SdsElasticCluster](./cr.html#sdselasticcluster) custom resource
declares the desired Ceph cluster (storage backing for OSDs, the
`CephCluster` itself, block pools, file systems, object stores) and the
optional integration with the [csi-ceph](/modules/csi-ceph/) module.

{{< alert level="info" >}}
The [snapshot-controller](/modules/snapshot-controller/) module must be
enabled for volume snapshot support.

When `spec.csiCephIntegration.enabled` is `true`, the
[csi-ceph](/modules/csi-ceph/) module must be enabled as well: the
controller automatically creates `CephClusterConnection` and
`CephStorageClass` resources for every block pool and file system.

When `spec.storage.lvm` is used, the
[sds-node-configurator](/modules/sds-node-configurator/) module must be
enabled and the underlying `LVMVolumeGroup` resources must already exist
on the target nodes.
{{< /alert >}}

## System requirements and recommendations

### Requirements

- A Deckhouse Kubernetes cluster with at least three nodes that will host
  Ceph daemons (mon, mgr, OSD).
- Two non-overlapping IPv4 CIDRs reachable from every storage node — one
  for the Ceph public network (client traffic) and one for the cluster
  network (replication and heartbeat). The same CIDR may be reused for
  both fields if the cluster does not separate the two networks.
- For the *bare devices* layout: at least one unused raw block device on
  every storage node (no partitions, no file system, no LVM signatures);
  the device path must match the configured `deviceFilter`.
- For the *LVM* layout: the
  [sds-node-configurator](/modules/sds-node-configurator/) module enabled
  and an `LVMVolumeGroup` (or `LVMVolumeGroupSet`) per node that creates
  the volume group referenced by `spec.storage.lvm.actualVGName`.
- The [snapshot-controller](/modules/snapshot-controller/) module enabled
  to support `VolumeSnapshot` resources.
- The [csi-ceph](/modules/csi-ceph/) module enabled when
  `spec.csiCephIntegration.enabled` is `true`.

## Quickstart guide

Run all commands on a machine that has administrator access to the
Kubernetes API.

### Enabling required modules

Enable `sds-elastic` together with its companion modules. The example
below enables `sds-node-configurator` (needed for the LVM layout),
`snapshot-controller` (needed for snapshots) and `csi-ceph` (needed for
`csiCephIntegration`):

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

### Choosing a storage layout

`sds-elastic` supports two mutually exclusive layouts for OSD backing
storage, controlled by `spec.storage`:

- `spec.storage.devices` — Rook consumes raw block devices directly on
  every selected node. Recommended when each storage node already has a
  dedicated empty disk for Ceph.
- `spec.storage.lvm` — Rook consumes per-node logical volumes provisioned
  by the [sds-node-configurator](/modules/sds-node-configurator/) module
  on top of pre-created `LVMVolumeGroup` resources. Useful when storage
  nodes do not have a dedicated disk and OSDs must share an existing VG.

Exactly one of the two fields must be set; the CRD validates this.

## Example: cluster on bare block devices

This example deploys a 3-replica Ceph cluster that consumes the raw
device `/dev/sdc` on every node carrying the
`node-role.deckhouse.io/storage` label, creates a single replicated
`CephBlockPool` (`replicapool`) and provisions matching `csi-ceph`
resources so that workloads can consume the pool through a regular
`StorageClass`.

1. Label the nodes that should run Ceph daemons:

   ```shell
   d8 k label node <node-name> node-role.deckhouse.io/storage=
   ```

1. Apply the cluster manifest:

   ```shell
   d8 k apply -f - <<EOF
   apiVersion: storage.deckhouse.io/v1alpha1
   kind: SdsElasticCluster
   metadata:
     name: bare-devices
   spec:
     cephVersion: v19.2.3
     network:
       # CIDR of the public Ceph network used by clients.
       public: 10.12.0.0/16
       # CIDR of the cluster Ceph network used for replication and heartbeat.
       cluster: 10.12.0.0/16
     storage:
       devices:
         useAllNodes: true
         useAllDevices: false
         # Regular expression matching device paths to be consumed by OSDs.
         deviceFilter: "^/dev/sdc$"
     blockPools:
       - name: replicapool
         replicated:
           size: 3
           requireSafeReplicaSize: true
     csiCephIntegration:
       enabled: true
     placement:
       all:
         nodeAffinity:
           requiredDuringSchedulingIgnoredDuringExecution:
             nodeSelectorTerms:
               - matchExpressions:
                   - { key: node-role.deckhouse.io/storage, operator: Exists }
   EOF
   ```

1. Wait until the resource reports `Ready`:

   ```shell
   d8 k get sdselasticcluster bare-devices -w
   ```

   The `Phase` column is expected to switch from `Pending` to
   `InProgress` and finally to `Ready`.

1. Verify that Ceph daemons are running:

   ```shell
   d8 k -n d8-sds-elastic get pod -owide
   ```

1. Verify the `csi-ceph` resources created by the integration:

   ```shell
   d8 k get cephclusterconnection
   d8 k get cephstorageclass
   d8 k get sc
   ```

   The output should contain a `CephClusterConnection`, a
   `CephStorageClass` named `replicapool` and a corresponding Kubernetes
   `StorageClass` ready to be consumed by `PersistentVolumeClaim`
   resources.

## Example: cluster on top of LVM

This example deploys a Ceph cluster on three nodes named
`sds-elastic-test-5-ubuntu-0`, `sds-elastic-test-5-ubuntu-1` and
`sds-elastic-test-5-ubuntu-2`. Every node must already host a volume
group called `vg-ceph` with enough free space; below the volume group is
created by the [sds-node-configurator](/modules/sds-node-configurator/)
module from raw disks via an `LVMVolumeGroupSet`.

1. Create the volume groups on the target nodes:

   ```shell
   d8 k apply -f - <<EOF
   apiVersion: storage.deckhouse.io/v1alpha1
   kind: LVMVolumeGroupSet
   metadata:
     name: lvg-ceph
   spec:
     strategy: PerNode
     nodeSelector:
       matchExpressions:
         - key: kubernetes.io/hostname
           operator: In
           values:
             - sds-elastic-test-5-ubuntu-0
             - sds-elastic-test-5-ubuntu-1
             - sds-elastic-test-5-ubuntu-2
     lvmVolumeGroupTemplate:
       type: Local
       actualVGNameOnTheNode: vg-ceph
       metadata:
         labels:
           app: ceph-osd
       blockDeviceSelector:
         matchExpressions:
           - key: status.blockdevice.storage.deckhouse.io/type
             operator: In
             values:
               - "disk"
   EOF
   ```

   Wait until every per-node `LVMVolumeGroup` is `Ready`:

   ```shell
   d8 k get lvmvolumegroup -l app=ceph-osd
   ```

1. Apply the cluster manifest. The controller creates a 20 GiB
   `LVMLogicalVolume` named `lv-osd` inside `vg-ceph` on each of the
   three nodes and feeds the resulting devices to Rook as OSDs:

   ```shell
   d8 k apply -f - <<EOF
   apiVersion: storage.deckhouse.io/v1alpha1
   kind: SdsElasticCluster
   metadata:
     name: ceph-lvm
   spec:
     cephVersion: v19.2.3
     network:
       public: 10.12.0.0/16
       cluster: 10.12.0.0/16
     mon:
       count: 3
     mgr:
       count: 3
     storage:
       lvm:
         # Prefix of the LVMVolumeGroup names; per-node groups are
         # matched as <lvgNamePrefix>-<i>.
         lvgNamePrefix: lvg-ceph
         # Existing volume group on the node (must already be present,
         # for example created by sds-node-configurator).
         actualVGName: vg-ceph
         # Name of the logical volume to be created in every VG.
         actualLVName: lv-osd
         # Size of every per-node LV.
         lvSize: 20Gi
         # Hostname prefix; nodes are matched as <nodeNamePrefix>-<i>
         # against the kubernetes.io/hostname label.
         nodeNamePrefix: sds-elastic-test-5-ubuntu
         # Number of nodes/OSDs to provision.
         nodeCount: 3
     blockPools:
       - name: replicapool
         failureDomain: host
         replicated:
           size: 3
   EOF
   ```

1. Wait until the resource reports `Ready`:

   ```shell
   d8 k get sdselasticcluster ceph-lvm -w
   ```

1. Verify that Ceph daemons are running and the OSDs back the expected
   logical volumes:

   ```shell
   d8 k -n d8-sds-elastic get pod -owide
   d8 k get lvmlogicalvolume
   ```

## Checking cluster health

Inspect the high-level status of the resource — the controller exposes
the most relevant conditions on the `SdsElasticCluster` CR:

```shell
d8 k describe sdselasticcluster <cluster-name>
```

Useful conditions include `StorageReady`, `CephClusterReady`,
`PoolsReady`, `FilesystemsReady`, `ObjectStoresReady`, `CsiCephReady`
and the aggregate `Ready` condition.

For a deeper Ceph-level inspection, use the toolbox pod shipped with
Rook:

```shell
d8 k -n d8-sds-elastic exec -it deploy/rook-ceph-tools -- ceph status
d8 k -n d8-sds-elastic exec -it deploy/rook-ceph-tools -- ceph osd tree
```
