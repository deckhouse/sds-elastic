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

// ElasticCluster is a cluster-scoped CR that describes the desired state of a
// Ceph cluster managed by the sds-elastic module. The controller bootstraps a
// Rook CephCluster (mon/mgr/osd) on top of LVM-based local storage discovered
// via blockDeviceSelector. Pool configuration (RBD/CephFS, replication
// strategy) lives separately in ElasticStorageClass resources that reference
// this ElasticCluster by name.
//
// +kubebuilder:resource:scope=Cluster,shortName=ec
// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ElasticCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ElasticClusterSpec    `json:"spec"`
	Status *ElasticClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ElasticClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []ElasticCluster `json:"items"`
}

// +k8s:deepcopy-gen=true
type ElasticClusterSpec struct {
	// Storage selects nodes and BlockDevices used to back OSDs.
	// +kubebuilder:validation:Required
	Storage ElasticClusterStorageSpec `json:"storage"`

	// Network optionally pins Ceph public/cluster networks. When unset,
	// Rook listens on every host IP of storage nodes (host networking).
	// +optional
	Network *ElasticClusterNetwork `json:"network,omitempty"`
}

// +k8s:deepcopy-gen=true
type ElasticClusterStorageSpec struct {
	// NodeSelector matches Kubernetes Nodes considered as storage nodes.
	// +kubebuilder:validation:Required
	NodeSelector *metav1.LabelSelector `json:"nodeSelector"`

	// BlockDeviceSelector matches BlockDevice CRs (managed by
	// sds-node-configurator) eligible for OSD provisioning.
	// +kubebuilder:validation:Required
	BlockDeviceSelector *metav1.LabelSelector `json:"blockDeviceSelector"`
}

// +k8s:deepcopy-gen=true
type ElasticClusterNetwork struct {
	// Public is the CIDR of the public network used for client traffic.
	// +kubebuilder:validation:Required
	Public string `json:"public"`

	// Cluster is the CIDR of the cluster network used for replication
	// and heartbeat traffic.
	// +kubebuilder:validation:Required
	Cluster string `json:"cluster"`
}

// +k8s:deepcopy-gen=true
type ElasticClusterStatus struct {
	// ObservedGeneration is the most recent .metadata.generation
	// reconciled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is a coarse-grained summary derived from Conditions.
	// +kubebuilder:validation:Enum=Pending;InProgress;Ready;Error
	// +optional
	Phase string `json:"phase,omitempty"`

	// CephFSID holds the FSID/UUID of the deployed Ceph cluster.
	// +optional
	CephFSID string `json:"cephFSID,omitempty"`

	// MonEndpoints is the list of ceph-mon endpoints in the form
	// "<host>:<port>", parsed from the rook-ceph-mon-endpoints ConfigMap.
	// +optional
	MonEndpoints []string `json:"monEndpoints,omitempty"`

	// MonMaxID is the highest mon id reached so far (from the
	// rook-ceph-mon-endpoints ConfigMap). Encoded as string for parity
	// with the raw ConfigMap value.
	// +optional
	MonMaxID string `json:"monMaxId,omitempty"`

	// CredentialsRef references the ElasticClusterCredential resource that
	// backs up cluster identity outside the module namespace.
	// +optional
	CredentialsRef *ElasticClusterCredentialRef `json:"credentialsRef,omitempty"`

	// CephVersion describes the requested and currently running Ceph
	// version, plus per-stage progress while a rolling upgrade is in flight.
	// +optional
	CephVersion *CephVersionStatus `json:"cephVersion,omitempty"`

	// Conditions hold the latest stage states. Known types:
	// StorageReady, CephClusterReady, CredentialsReady, CsiCephReady,
	// UpgradeReady, UpgradeInProgress, Ready.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +k8s:deepcopy-gen=true
type ElasticClusterCredentialRef struct {
	// Name of the ElasticClusterCredential resource (1:1 with this
	// ElasticCluster's metadata.name).
	Name string `json:"name"`
}

// +k8s:deepcopy-gen=true
type CephVersionStatus struct {
	// Requested is the Ceph version the controller currently asks Rook
	// to run (matches the module image).
	// +optional
	Requested string `json:"requested,omitempty"`

	// Running is the Ceph version actually present in the cluster,
	// derived from CephCluster.status.cephStatus.ceph.versions.
	// +optional
	Running string `json:"running,omitempty"`

	// UpgradeProgress is populated while a rolling upgrade is in flight.
	// +optional
	UpgradeProgress *UpgradeProgress `json:"upgradeProgress,omitempty"`
}

// +k8s:deepcopy-gen=true
type UpgradeProgress struct {
	// Phase is the current upgrade stage.
	// +kubebuilder:validation:Enum=MonUpgrading;MgrUpgrading;MdsUpgrading;OsdUpgrading;Completed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Completed counts daemons in the current stage already running
	// the requested version.
	// +optional
	Completed int32 `json:"completed,omitempty"`

	// Total counts all daemons in the current stage.
	// +optional
	Total int32 `json:"total,omitempty"`

	// LastTransitionTime is the timestamp of the last upgrade-stage
	// transition.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// Well-known condition types for ElasticCluster.
const (
	ECConditionStorageReady      = "StorageReady"
	ECConditionCephClusterReady  = "CephClusterReady"
	ECConditionCredentialsReady  = "CredentialsReady"
	ECConditionCsiCephReady      = "CsiCephReady"
	ECConditionUpgradeReady      = "UpgradeReady"
	ECConditionUpgradeInProgress = "UpgradeInProgress"
	ECConditionReady             = "Ready"
)

// Well-known UpgradeProgress phases.
const (
	UpgradePhaseMon       = "MonUpgrading"
	UpgradePhaseMgr       = "MgrUpgrading"
	UpgradePhaseMds       = "MdsUpgrading"
	UpgradePhaseOsd       = "OsdUpgrading"
	UpgradePhaseCompleted = "Completed"
)

// ElasticClusterKind is the kind constant used for OwnerReferences and
// dynamic GVK lookups.
const ElasticClusterKind = "ElasticCluster"
