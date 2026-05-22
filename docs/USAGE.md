---
title: "Usage"
description: "Deploying and managing Ceph clusters with the sds-elastic module: enabling companion modules, choosing a storage layout, and working examples."
weight: 50
---

## Enabling Required Modules

{{< alert level="warning" >}}
The `sds-elastic` module is in `Experimental` stage. Experimental modules are not enabled by default. Set `allowExperimentalModules: true` in the `deckhouse` ModuleConfig before enabling the module.
{{< /alert >}}

Enable `sds-elastic` together with its companion modules.
The example below enables `sds-node-configurator` (required for the LVM layout),
`snapshot-controller` (required for snapshots), and `csi-ceph` (required for `csiCephIntegration`):

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

## Choosing a Storage Layout

`sds-elastic` supports two mutually exclusive layouts for OSD backing storage, selected via `spec.storage`:

- `spec.storage.devices` — Rook consumes raw block devices directly on every selected node.
  Recommended when each storage node already has a dedicated empty disk for Ceph.
- `spec.storage.lvm` — Rook consumes per-node logical volumes provisioned by the
  [sds-node-configurator](/modules/sds-node-configurator/) module on top of pre-created
  LVMVolumeGroup objects. Use this when storage nodes share disks with other workloads.

Exactly one of the two fields must be set; the CRD validates this constraint.

## Example: Cluster on Raw Block Devices

This example deploys a three-replica Ceph cluster that consumes the raw device `/dev/sdc` on every
node carrying the `node-role.deckhouse.io/storage` label, creates a single replicated CephBlockPool
named `replicapool`, and provisions matching `csi-ceph` resources so that workloads can consume the pool
through a regular StorageClass.

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

1. Wait until the SdsElasticCluster reports `Ready`:

   ```shell
   d8 k get sdselasticcluster bare-devices -w
   ```

   The `Phase` column is expected to switch from `Pending` to `InProgress` and finally to `Ready`.

1. Verify that Ceph daemons are running:

   ```shell
   d8 k -n d8-sds-elastic get pod -owide
   ```

1. Verify the `csi-ceph` objects created by the integration:

   ```shell
   d8 k get cephclusterconnection
   d8 k get cephstorageclass
   d8 k get sc
   ```

   The output should contain a CephClusterConnection named `ceph-cluster-connection`,
   a CephStorageClass named `sds-elastic-rbd-replicapool`, and a corresponding Kubernetes
   StorageClass with the same name ready to be consumed by PersistentVolumeClaim resources.
   Names follow the pattern `sds-elastic-rbd-<pool>` for block pools and
   `sds-elastic-cephfs-<filesystem>` for CephFS filesystems.

## Example: Cluster on LVM

This example deploys a Ceph cluster on three nodes named `sds-elastic-test-5-ubuntu-0`,
`sds-elastic-test-5-ubuntu-1`, and `sds-elastic-test-5-ubuntu-2`. Every node must already host
a volume group called `vg-ceph` with enough free space; below the volume group is created by the
[sds-node-configurator](/modules/sds-node-configurator/) module from raw disks via an LVMVolumeGroupSet.

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

   Wait until every per-node LVMVolumeGroup is `Ready`:

   ```shell
   d8 k get lvmvolumegroup -l app=ceph-osd
   ```

1. Apply the cluster manifest.
   The controller creates a 20 GiB LVMLogicalVolume named `lv-osd` inside `vg-ceph` on each of the
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
         # Prefix of the LVMVolumeGroup names; per-node groups are matched as <lvgNamePrefix>-<i>.
         lvgNamePrefix: lvg-ceph
         # Existing volume group on the node (must already be present).
         actualVGName: vg-ceph
         # Name of the logical volume to be created in every volume group.
         actualLVName: lv-osd
         # Size of every per-node logical volume.
         lvSize: 20Gi
         # Hostname prefix; nodes are matched as <nodeNamePrefix>-<i> against the kubernetes.io/hostname label.
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

1. Wait until the SdsElasticCluster reports `Ready`:

   ```shell
   d8 k get sdselasticcluster ceph-lvm -w
   ```

1. Verify that Ceph daemons are running and the OSDs back the expected logical volumes:

   ```shell
   d8 k -n d8-sds-elastic get pod -owide
   d8 k get lvmlogicalvolume
   ```

## Deleting a Cluster

Delete the SdsElasticCluster resource to tear down the Ceph cluster:

```shell
d8 k delete sdselasticcluster <cluster-name>
```

The controller performs graceful teardown in strict reverse order:

1. CephStorageClass objects (csi-ceph).
1. CephClusterConnection (csi-ceph).
1. CephObjectStore, CephFilesystem, CephBlockPool (Rook).
1. `rook-ceph-tools` Deployment and CephCluster (Rook).
1. Local PVs and LVMLogicalVolume objects created by the module.

At each step the controller waits for the upstream operator (Rook or csi-ceph) to perform its own cleanup: drain pools, wipe OSD devices, release `cephx` auth, etc. Until cleanup at one step completes, the next step does not start.

### Force-deletion Recovery

If Ceph daemons never reached quorum or Rook cannot drain the cluster, graceful teardown will not progress and the SdsElasticCluster will remain in `Terminating` state indefinitely. In this case, force-deletion is available.

{{< alert level="danger" >}}
Force-deletion strips foreign finalizers from Rook and csi-ceph resources, bypassing their cleanup logic. OSD devices will not be wiped, `cephx` auth entries will not be removed from any external clients, and any remaining data on disks must be cleaned up manually. Use only as a recovery procedure for stuck teardowns.
{{< /alert >}}

To enable force-deletion, add the `storage.deckhouse.io/force-delete` annotation set to `"true"` on the SdsElasticCluster:

```shell
d8 k annotate sdselasticcluster <cluster-name> storage.deckhouse.io/force-delete=true
```

After the annotation is applied, the controller waits for a grace window (5 minutes by default) since `deletionTimestamp`, then strips foreign finalizers and completes teardown.

## Disabling the Module

{{< alert level="danger" >}}
Disabling the module stops the controller and the Rook operator. Data stored in Ceph clusters managed by this module may become unavailable or be lost. Always delete every SdsElasticCluster object before disabling the module.
{{< /alert >}}

1. Delete every SdsElasticCluster object and wait for the controller to fully tear down the underlying Ceph cluster:

   ```shell
   d8 k get sdselasticclusters.storage.deckhouse.io
   ```

   Wait until the command returns `No resources found`.

1. Disable the module. Disabling requires the `modules.deckhouse.io/allow-disabling: "true"` label on the ModuleConfig:

   ```shell
   d8 k label moduleconfig sds-elastic modules.deckhouse.io/allow-disabling=true --overwrite
   d8 k patch moduleconfig sds-elastic --type=merge -p '{"spec":{"enabled":false}}'
   ```

## Checking Cluster Health

Inspect the high-level status of the SdsElasticCluster — the controller exposes the most relevant conditions
on the [SdsElasticCluster](cr.html#sdselasticcluster) CR:

```shell
d8 k describe sdselasticcluster <cluster-name>
```

Useful conditions include `StorageReady`, `CephClusterReady`, `PoolsReady`, `FilesystemsReady`,
`ObjectStoresReady`, `CsiCephReady`, and the aggregate `Ready` condition.

For a deeper Ceph-level inspection, use the toolbox pod shipped with Rook:

```shell
d8 k -n d8-sds-elastic exec -it deploy/rook-ceph-tools -- ceph status
d8 k -n d8-sds-elastic exec -it deploy/rook-ceph-tools -- ceph osd tree
```
