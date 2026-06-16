---
title: "Module sds-elastic"
description: "Deckhouse Kubernetes Platform module that bootstraps a Rook Ceph cluster from BlockDevice CRs and exposes pools as csi-ceph StorageClasses."
weight: 1
---

{{< alert level="warning" >}}
The module is in `Experimental` stage. The API, configuration, and custom resources may change without notice; do not use it for production workloads.
{{< /alert >}}

The `sds-elastic` module deploys and manages [Rook Ceph](https://rook.io) in a Deckhouse Kubernetes Platform cluster, turning a set of nodes into a distributed Ceph-backed storage system. The module provisions block volumes (RBD) and shared filesystems (CephFS) backed by `csi-ceph` StorageClasses, without manual Rook deployment.

Management is split across three custom resources from the `storage.deckhouse.io/v1alpha1` API group:

- [`ElasticCluster`](./cr.html#elasticcluster) (`ec`) — declares the desired Ceph cluster: which nodes participate (`storage.nodeSelector`), which BlockDevice CRs back the OSDs (`storage.blockDeviceSelector`) and, optionally, which CIDRs are used for the public and cluster networks. The controller bootstraps a Rook `CephCluster` (mon/mgr/osd) from this declaration.
- [`ElasticStorageClass`](./cr.html#elasticstorageclass) (`esc`) — declares a single Ceph pool plus the matching Kubernetes `StorageClass`, provisioned through the [csi-ceph](/modules/csi-ceph/) module. `spec.replication` (`AvailabilityWithoutConsistency` / `ConsistencyAndAvailability` / `HighRedundancy`) maps to a production-tested pool layout. References its parent `ElasticCluster` by name (`spec.clusterRef`).
- [`ElasticClusterCredential`](./cr.html#elasticclustercredential) (`ecc`) — internal cluster-scoped backup of the Ceph cluster identity (FSID, mon-secret, admin-secret), populated by the controller from the `rook-ceph-mon` Secret. Operators do not manage this CR directly; it exists so the cluster identity survives a `d8-sds-elastic` namespace re-create.

The module deploys the Rook Ceph operator, the `rook-ceph-operator-config` ConfigMap, the full set of Ceph CRDs and the three `storage.deckhouse.io` CRDs listed above.

## Main Features

- Single-CR cluster bootstrap from an [ElasticCluster](./cr.html#elasticcluster) selecting BlockDevice / Node CRs by labels.
- LVM-based OSD layout: per matched BlockDevice the controller provisions one [LVMVolumeGroup](/modules/sds-node-configurator/cr.html#lvmvolumegroup), one [LVMLogicalVolume](/modules/sds-node-configurator/cr.html#lvmlogicalvolume), and one local `PersistentVolume` bound to the helm-managed `sds-elastic-osd` `StorageClass` (provisioner `kubernetes.io/no-provisioner`, `volumeBindingMode: WaitForFirstConsumer`). Rook consumes those PVs as OSDs through `storageClassDeviceSets`.
- Three replication strategies per [ElasticStorageClass](./cr.html#elasticstorageclass): `AvailabilityWithoutConsistency` (2 replicas, `min_size=1`, `requireSafeReplicaSize=false`), `ConsistencyAndAvailability` (3 replicas, `min_size=2`, default), and `HighRedundancy` (4 replicas, `min_size=2`, `requireSafeReplicaSize=true`; tolerates two simultaneous host failures with continued I/O and one extra failure as a recovery margin; requires at least 5 storage nodes). The `ErasureCodedCompact` mode is temporarily disabled and cannot be selected.
- RBD and CephFS pools per [ElasticStorageClass](./cr.html#elasticstorageclass-v1alpha1-spec-type) — one ESC produces one Ceph pool plus one csi-ceph `CephStorageClass` named after the ESC.
- Automatic [csi-ceph](/modules/csi-ceph/) wiring: the controller maintains a single `CephClusterConnection` (1:1 with the parent ElasticCluster) and one `CephStorageClass` per ElasticStorageClass; the user does not edit these vendor CRs by hand.
- Identity backup via [ElasticClusterCredential](./cr.html#elasticclustercredential): FSID and mon/admin secrets are mirrored from the `rook-ceph-mon` Secret so the cluster identity survives a namespace re-create.

## System Requirements

- Deckhouse Kubernetes Platform of version `1.72` or later with at least three nodes for Ceph daemons (`mon`, `mgr`, `OSD`).
- Two non-overlapping IPv4 CIDRs reachable from every storage node:
  - one for the Ceph public network (client traffic)
  - and one for the cluster network (replication and heartbeat).

    The same CIDR may be used for both if network separation is not needed; `spec.network` may also be omitted, in which case Rook listens on every host IP of the storage nodes (host networking).
- The [sds-node-configurator](/modules/sds-node-configurator/) (≥ 0.6.8) module enabled. The module owns the `BlockDevice` and `LVMVolumeGroup` CRDs that `ElasticCluster` selects from and creates LVMVolumeGroups in.
- The [csi-ceph](/modules/csi-ceph/) (≥ 0.5.26) module enabled. The module owns the `CephClusterConnection` and `CephStorageClass` CRDs the controller writes into.
- The [snapshot-controller](/modules/snapshot-controller/) module enabled for VolumeSnapshot support (optional).

## Limitations

- One Ceph cluster per module instance. The controller always reconciles a single Rook `CephCluster` named `ceph-cluster` in the `d8-sds-elastic` namespace, regardless of how many `ElasticCluster` objects are created. Multiple `ElasticCluster` objects compete for the same backend; create only one per cluster.
- The vendored Rook operator registers its CRs under the renamed API group `internal.sdselastic.deckhouse.io` (upstream uses `ceph.rook.io`); this isolates sds-elastic from a user-installed Rook on the same cluster.
- Direct edits of Rook (`internal.sdselastic.deckhouse.io`) resources in the `d8-sds-elastic` namespace are rejected by a validating webhook. All changes must go through `ElasticCluster` / `ElasticStorageClass`.
- RGW and S3 buckets are not supported in this release. The Rook ObjectBucket (OBC) controller is disabled via `ROOK_DISABLE_OBJECT_BUCKET_CLAIM=true`, and the `objectbucket.io` CRDs are not vendored.
- `ElasticCluster.spec.network` is immutable after creation (enforced by CEL in the CRD and mirrored by the validating webhook); changing public/cluster CIDRs on a live cluster invalidates mon endpoints and host-network bindings, so a network change requires deleting and re-creating the `ElasticCluster`.
- `ElasticCluster.spec.storage.nodeSelector` and `spec.storage.blockDeviceSelector` are editable after creation. The validating webhook rejects narrowing edits that would orphan an already-adopted `BlockDevice` (the controller cannot safely release the LVG/LLV/PV plumbing without manual cleanup) and widening edits that would pull in a `BlockDevice` already owned by another `ElasticCluster`. See [USAGE.md](./usage.html#blockdevice-adoption-and-ownership) for the full ownership contract and the manual release procedure.
- `ElasticStorageClass.spec.{clusterRef,type,replication}` are immutable after creation (enforced by CEL and the validating webhook). Replacing a pool requires creating a new `ElasticStorageClass` with a different name.
- The name `sds-elastic-osd` is reserved for the helm-managed internal `StorageClass`; `ElasticStorageClass` resources with this `metadata.name` are rejected by the webhook.
- `ErasureCodedCompact` is temporarily disabled: it is omitted from the `spec.replication` enum and rejected by the validating webhook, so it cannot be selected on any `ElasticStorageClass`.
- `replication: HighRedundancy` requires **at least 5 storage nodes** (4 for the pool's CRUSH placement at `failureDomain=host` and 5 to host a 5-mon quorum). The controller auto-promotes the underlying `CephCluster` to `mon.count=5`, `mgr.count=3` while at least one HighRedundancy ESC is present, and keeps the promotion sticky after the trigger ESC is removed (silently weakening fault tolerance on a live cluster is unsafe). Operators can force a recompute by clearing `ElasticCluster.status.cephTopology` via the status subresource. The ESC validating webhook rejects CREATE of a HighRedundancy ESC when its parent `ElasticCluster` is absent, when fewer than 5 nodes match `spec.storage.nodeSelector`, or when the parent EC's adopted `BlockDevice` resources span fewer than 4 distinct nodes — applying the EC and the HighRedundancy ESC in the same `kubectl apply` is therefore rejected, and the EC must be applied (and its OSD-host set populated) first.
- Vendor Rook and Ceph CRDs are bundled with the module and are not user-configurable.
- `ElasticCluster` / `ElasticStorageClass` teardown is finalizer-driven: deleting them removes the controller-managed Rook `CephCluster` / `CephClusterConnection` and the backing pools / filesystems plus their csi-ceph `CephStorageClass`es. The per-device objects (`LVMVolumeGroup` / `LVMLogicalVolume` / local `PersistentVolume`) and the `BlockDevice` ownership label are intentionally preserved and must be cleaned up manually; full OwnerReferences-driven GC of those is not yet wired (planned, B20 in the backlog). See [USAGE.md](./usage.html#deleting-resources).
