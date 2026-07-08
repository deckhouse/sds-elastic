/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ElasticStorageClass is a cluster-scoped CR that declares a single Ceph pool
// plus the corresponding Kubernetes StorageClass, provisioned through the
// csi-ceph module. The controller maps spec.replication to a production-tested
// pool layout and creates a 1:1-named CephStorageClass in csi-ceph.
//
// +kubebuilder:resource:scope=Cluster,shortName=esc
// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ElasticStorageClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ElasticStorageClassSpec    `json:"spec"`
	Status *ElasticStorageClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ElasticStorageClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []ElasticStorageClass `json:"items"`
}

// StorageClassType selects the Ceph backend used by the StorageClass.
// +kubebuilder:validation:Enum=RBD;CephFS
type StorageClassType string

const (
	// StorageClassTypeRBD provisions block volumes via Rados Block Device.
	StorageClassTypeRBD StorageClassType = "RBD"

	// StorageClassTypeCephFS provisions shared-filesystem volumes via
	// CephFS subvolumes.
	StorageClassTypeCephFS StorageClassType = "CephFS"
)

// ReplicationMode encodes the high-level replication strategy.
// The controller translates each value into pool-level settings (see
// constants below for the precise semantics).
//
// ErasureCodedCompact is intentionally absent from the enum: it is
// temporarily disabled and cannot be selected (see the constant below).
// +kubebuilder:validation:Enum=AvailabilityWithoutConsistency;ConsistencyAndAvailability;HighRedundancy
type ReplicationMode string

const (
	// ReplicationAvailabilityWithoutConsistency is a 2-replica setup with
	// min_size=1 and requireSafeReplicaSize=false. Maximises availability
	// at the cost of split-brain risk; suitable for non-critical workloads.
	ReplicationAvailabilityWithoutConsistency ReplicationMode = "AvailabilityWithoutConsistency"

	// ReplicationConsistencyAndAvailability is the production default:
	// 3 replicas with min_size=2.
	ReplicationConsistencyAndAvailability ReplicationMode = "ConsistencyAndAvailability"

	// ReplicationHighRedundancy is a 4-replica setup with min_size=2 and
	// requireSafeReplicaSize=true. Designed to tolerate two simultaneous
	// host failures with continued I/O (2 replicas == min_size keeps the
	// pool active) and to keep one extra copy as a recovery margin —
	// data loss only occurs at the fourth simultaneous failure, while
	// the third failure pauses I/O until Ceph backfills the surviving
	// copy onto free cluster space.
	//
	// Operational implications:
	//   - Storage overhead is 4x (vs 3x for ConsistencyAndAvailability).
	//   - Minimum 5 storage nodes are required: 4 for the pool's CRUSH
	//     placement (failureDomain=host) and 5 to host a 5-mon quorum
	//     (the controller auto-promotes CephCluster.spec.mon.count to 5
	//     when at least one HighRedundancy ESC is present, so the mon
	//     plane survives the same two simultaneous host failures the
	//     data plane is sized for).
	//   - The promotion is sticky: deleting the last HighRedundancy ESC
	//     does NOT roll mon.count back to 3 — silently weakening the
	//     fault-tolerance guarantee on a live cluster is unsafe. See
	//     CephTopologyStatus on ElasticClusterStatus for the
	//     high-water-mark machinery.
	ReplicationHighRedundancy ReplicationMode = "HighRedundancy"

	// ReplicationErasureCodedCompact uses k=2,m=2 with jerasure/reed_sol_van
	// and allow_ec_overwrites=true. Storage-efficient (1.5x overhead vs 3x
	// for replicated), requires at least 4 storage nodes.
	//
	// TEMPORARILY DISABLED: the value is intentionally omitted from the
	// ReplicationMode enum (CRD + kubebuilder marker) and rejected by the
	// validating webhook, so it cannot be selected on any ElasticStorageClass.
	// The constant and the builder branches that consume it are kept so the
	// mode can be re-enabled later once CephFS-on-EC is fully supported.
	ReplicationErasureCodedCompact ReplicationMode = "ErasureCodedCompact"
)

// DefaultReplication is applied when spec.replication is omitted.
const DefaultReplication = ReplicationConsistencyAndAvailability

// PgAutoscaleMode selects the Ceph pg_autoscale_mode applied to the pool.
// It maps 1:1 to the Rook pool `parameters.pg_autoscale_mode` value.
//
// Leaving spec.pgAutoscaleMode empty inherits the cluster default (the
// pg_autoscaler mgr module is enabled cluster-wide, so the effective default
// is "on").
// +kubebuilder:validation:Enum=on;off;warn
type PgAutoscaleMode string

const (
	// PgAutoscaleModeOn lets the pg_autoscaler manage pg_num automatically.
	PgAutoscaleModeOn PgAutoscaleMode = "on"

	// PgAutoscaleModeOff freezes pg_num at its current value. Combined with an
	// explicit spec.pgNum this pins the PG count and stops the autoscaler from
	// triggering OSD data movement — the intended setup for test/nested
	// clusters that must not rebalance.
	PgAutoscaleModeOff PgAutoscaleMode = "off"

	// PgAutoscaleModeWarn only emits a health warning with the suggested
	// pg_num instead of changing it.
	PgAutoscaleModeWarn PgAutoscaleMode = "warn"
)

// +k8s:deepcopy-gen=true
type ElasticStorageClassSpec struct {
	// ClusterRef is the name of the ElasticCluster this storage class
	// belongs to. The referenced cluster must exist and be in the Ready
	// phase before pool provisioning starts.
	// +kubebuilder:validation:Required
	ClusterRef string `json:"clusterRef"`

	// Type selects RBD (block) or CephFS (shared filesystem) backend.
	// +kubebuilder:validation:Required
	Type StorageClassType `json:"type"`

	// Replication picks the high-level replication strategy.
	// +kubebuilder:default=ConsistencyAndAvailability
	Replication ReplicationMode `json:"replication,omitempty"`

	// PgNum, when set, pins the pool's target pg_num. Rook renders it into the
	// pool `parameters.pg_num`. Omitted (0) leaves PG sizing to Ceph. To keep
	// the count fixed, set PgAutoscaleMode=off as well — otherwise the
	// autoscaler may override this value.
	//
	// Restricted to powers of two (Ceph distributes PGs evenly only for
	// powers of two) with a 512 ceiling. This is only a shape guard: the
	// validating webhook additionally rejects a pgNum that would push the
	// projected PGs-per-OSD of the target cluster past a safe threshold,
	// since a safe maximum depends on the cluster's OSD count.
	// +kubebuilder:validation:Enum=16;32;64;128;256;512
	// +optional
	PgNum int32 `json:"pgNum,omitempty"`

	// PgAutoscaleMode, when set, controls the pool's pg_autoscale_mode
	// (on/off/warn). Set it to off to stop the autoscaler from moving data
	// between OSDs — useful for test/nested clusters. Omitted inherits the
	// cluster default ("on").
	// +optional
	PgAutoscaleMode PgAutoscaleMode `json:"pgAutoscaleMode,omitempty"`
}

// +k8s:deepcopy-gen=true
type ElasticStorageClassStatus struct {
	// ObservedGeneration is the most recent .metadata.generation
	// reconciled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is a coarse-grained summary derived from Conditions.
	// +kubebuilder:validation:Enum=Pending;InProgress;Ready;Error
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions hold the latest stage states. Known types:
	// PoolReady, CsiStorageClassReady, Ready.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// Well-known condition types for ElasticStorageClass.
const (
	ESCConditionPoolReady            = "PoolReady"
	ESCConditionCsiStorageClassReady = "CsiStorageClassReady"
	ESCConditionReady                = "Ready"
)

// Deletion (teardown) reasons set on the aggregate Ready condition while
// an ElasticStorageClass is being deleted. Domain-level on purpose: they
// never name the underlying vendor (Rook/csi-ceph) resources.
//
//   - ESCReasonBoundVolumesExist: PersistentVolumes provisioned from this
//     StorageClass are still bound; the operator must delete the PVCs
//     first. Non-bypassable guard (force never lifts it).
//   - ESCReasonDataPresentInPool: the RBD pool still holds data; deleting
//     it is destructive, so it requires the force-deletion annotation.
//   - ESCReasonFilesystemNotEmpty: the filesystem still has volumes; the
//     operator must delete the remaining PersistentVolumes first (there is
//     no force path for CephFS).
//   - ESCReasonTerminating: teardown is in progress (backend resources are
//     being removed).
const (
	ESCReasonBoundVolumesExist  = "BoundVolumesExist"
	ESCReasonDataPresentInPool  = "DataPresentInPool"
	ESCReasonFilesystemNotEmpty = "FilesystemNotEmpty"
	ESCReasonTerminating        = "Terminating"
)

// ElasticStorageClassKind is the kind constant used for OwnerReferences and
// dynamic GVK lookups.
const ElasticStorageClassKind = "ElasticStorageClass"

// ReservedOSDStorageClassName is the helm-managed name of the internal
// Kubernetes StorageClass that binds local PVs (created by sds-elastic) to
// PVCs expected by Rook storageClassDeviceSets. The webhook rejects
// ElasticStorageClass resources with this metadata.name.
const ReservedOSDStorageClassName = "sds-elastic-osd"
