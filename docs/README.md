---
title: "Module sds-elastic"
description: "Deckhouse Kubernetes Platform module for deploying and managing distributed Ceph clusters with block storage, shared filesystems, and S3 object storage."
weight: 1
---

{{< alert level="warning" >}}
The module is in `Experimental` stage. The API, configuration, and custom resources may change without notice; do not use it for production workloads.
{{< /alert >}}

The `sds-elastic` module deploys and manages [Rook Ceph](https://rook.io) in a Deckhouse Kubernetes Platform cluster, turning a set of nodes into a distributed Ceph-backed storage system. 

The module allows for the provision of block storage, shared file systems, and S3-compatible object storage in the cluster for workloads, without the need for manual deployment of Ceph.

Management is performed using a custom resource called [SdsElasticCluster](./cr.html#sdselasticcluster), which describes the entire desired state of the Ceph cluster:
- OSD backing storage
- block pools
- filesystems
- object stores
- optional integration with the [csi-ceph](/modules/csi-ceph/) module.

The module deploys the Rook Ceph operator, the `rook-ceph-operator-config` ConfigMap, and the full set of Ceph CRDs.

## Main Features

- Deploy Ceph cluster from a single [SdsElasticCluster custom resource](./cr.html#sdselasticcluster).
- Two OSD backing modes: [raw block devices](./cr.html#sdselasticcluster-v1alpha1-spec-storage-devices) or [LVM logical volumes](./cr.html#sdselasticcluster-v1alpha1-spec-storage-lvm) managed by the [sds-node-configurator](/modules/sds-node-configurator/) module.
- [Replicated and erasure-coded block pools](./cr.html#sdselasticcluster-v1alpha1-spec-blockpools) with configurable failure domains.
- [CephFS shared filesystems](./cr.html#sdselasticcluster-v1alpha1-spec-filesystems) with separate metadata and data pools.
- [S3-compatible object stores](./cr.html#sdselasticcluster-v1alpha1-spec-objectstores) with configurable RGW gateways.
- [Automatic provisioning](./cr.html#sdselasticcluster-v1alpha1-spec-csicephintegration) of CephClusterConnection and CephStorageClass objects for the [csi-ceph](/modules/csi-ceph/) module when integration is enabled.
- [Per-daemon scheduling](./cr.html#sdselasticcluster-v1alpha1-spec-placement) via standard Kubernetes node affinity, tolerations, and topology spread constraints.

## System Requirements

- Deckhouse Kubernetes Platform of version `1.72` or later with at least three nodes for Ceph daemons (`mon`, `mgr`, `OSD`).
- Two non-overlapping IPv4 CIDRs reachable from every storage node:
  - one for the Ceph public network (client traffic)
  - and one for the cluster network (replication and heartbeat).

    The same CIDR may be used for both if network separation is not needed.
- For the raw-devices layout: at least one unused raw block device on each storage node with no partitions, filesystem, or LVM signatures.
- For the LVM layout: the [sds-node-configurator](/modules/sds-node-configurator/) module enabled and LVMVolumeGroup objects created on the target nodes.
- The [snapshot-controller](/modules/snapshot-controller/) module enabled for VolumeSnapshot support.
- The [csi-ceph](/modules/csi-ceph/) module enabled when [`spec.csiCephIntegration.enabled`](./cr.html#sdselasticcluster-v1alpha1-spec-csicephintegration-enabled) is `true`.

## Limitations

- One Ceph cluster per module instance. The controller always reconciles a single CephCluster named `ceph-cluster` in the `d8-sds-elastic` namespace, regardless of how many SdsElasticCluster objects are created. Multiple SdsElasticCluster objects compete for the same backend; create only one.
- Direct edits of Rook (`ceph.rook.io`) and ObjectBucket (`objectbucket.io`) resources in the `d8-sds-elastic` namespace are rejected by a validating webhook. All changes must go through the SdsElasticCluster.
- Editing the SdsElasticCluster spec is destructive: removing a pool, filesystem, or object store from the spec deletes the corresponding Rook resource and any CephStorageClass derived from it.
- The [`spec.storage.lvm`](./cr.html#sdselasticcluster-v1alpha1-spec-storage-lvm) and [`spec.storage.devices`](./cr.html#sdselasticcluster-v1alpha1-spec-storage-devices) layouts are mutually exclusive; switching between them requires deleting and recreating the SdsElasticCluster object.
- Vendor Rook and Ceph CRDs are bundled with the module and are not user-configurable.
- Only Ceph versions explicitly listed in the [`spec.cephVersion`](./cr.html#sdselasticcluster-v1alpha1-spec-cephversion) enum are supported; arbitrary image references are not accepted.
