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
// +kubebuilder:validation:Enum=AvailabilityWithoutConsistency;ConsistencyAndAvailability;ErasureCodedCompact
type ReplicationMode string

const (
	// ReplicationAvailabilityWithoutConsistency is a 2-replica setup with
	// min_size=1 and requireSafeReplicaSize=false. Maximises availability
	// at the cost of split-brain risk; suitable for non-critical workloads.
	ReplicationAvailabilityWithoutConsistency ReplicationMode = "AvailabilityWithoutConsistency"

	// ReplicationConsistencyAndAvailability is the production default:
	// 3 replicas with min_size=2.
	ReplicationConsistencyAndAvailability ReplicationMode = "ConsistencyAndAvailability"

	// ReplicationErasureCodedCompact uses k=2,m=2 with jerasure/reed_sol_van
	// and allow_ec_overwrites=true. Storage-efficient (1.5x overhead vs 3x
	// for replicated), requires at least 4 storage nodes, and is rejected
	// when combined with type=RBD (csi-ceph does not yet provision RBD on
	// erasure-coded pools).
	ReplicationErasureCodedCompact ReplicationMode = "ErasureCodedCompact"
)

// DefaultReplication is applied when spec.replication is omitted.
const DefaultReplication = ReplicationConsistencyAndAvailability

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

// ElasticStorageClassKind is the kind constant used for OwnerReferences and
// dynamic GVK lookups.
const ElasticStorageClassKind = "ElasticStorageClass"

// ReservedOSDStorageClassName is the helm-managed name of the internal
// Kubernetes StorageClass that binds local PVs (created by sds-elastic) to
// PVCs expected by Rook storageClassDeviceSets. The webhook rejects
// ElasticStorageClass resources with this metadata.name.
const ReservedOSDStorageClassName = "sds-elastic-osd"
