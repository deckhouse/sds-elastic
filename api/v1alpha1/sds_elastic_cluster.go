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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SdsElasticCluster is a cluster-scoped aggregate CR that describes the desired
// state of the Rook Ceph cluster managed by the sds-elastic module: storage
// backing for OSDs (LVM or raw devices), the CephCluster itself, block pools,
// filesystems, object stores and the optional CSI/csi-ceph integration.
//
// +kubebuilder:resource:scope=Cluster,shortName=sdsec
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type SdsElasticCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SdsElasticClusterSpec    `json:"spec"`
	Status *SdsElasticClusterStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type SdsElasticClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []SdsElasticCluster `json:"items"`
}

// +k8s:deepcopy-gen=true
type SdsElasticClusterSpec struct {
	// CephVersion is the container image tag for the Ceph daemons,
	// e.g. "v18.2.7". Mapped into spec.cephVersion.image of CephCluster.
	// +kubebuilder:default="v18.2.7"
	CephVersion string `json:"cephVersion,omitempty"`

	// Network configures public/cluster CIDRs for the Ceph cluster.
	// Mapped into spec.network.addressRanges of CephCluster with
	// provider=host.
	// +kubebuilder:validation:Required
	Network NetworkSpec `json:"network"`

	// Mon configures the ceph-mon quorum size.
	// +kubebuilder:default={count: 3}
	Mon DaemonCount `json:"mon,omitempty"`

	// Mgr configures the ceph-mgr replicas.
	// +kubebuilder:default={count: 3}
	Mgr DaemonCount `json:"mgr,omitempty"`

	// Storage selects how OSDs are backed: either via LVM logical
	// volumes carved out of pre-existing LVMVolumeGroups (managed by
	// sds-node-configurator) or via raw block devices.
	// Exactly one of "lvm" or "devices" must be set.
	// +kubebuilder:validation:Required
	Storage StorageSpec `json:"storage"`

	// BlockPools is the list of CephBlockPool resources to create.
	// +optional
	BlockPools []BlockPoolSpec `json:"blockPools,omitempty"`

	// Filesystems is the list of CephFilesystem resources to create.
	// +optional
	Filesystems []FilesystemSpec `json:"filesystems,omitempty"`

	// ObjectStores is the list of CephObjectStore resources to create.
	// +optional
	ObjectStores []ObjectStoreSpec `json:"objectStores,omitempty"`

	// CsiCephIntegration controls automatic creation of csi-ceph
	// resources (CephClusterConnection + CephStorageClass per pool/fs)
	// in the storage.deckhouse.io API group. The controller will read
	// monitors/fsid/userKey from Rook secrets and populate the CR.
	// +optional
	CsiCephIntegration *CsiCephIntegrationSpec `json:"csiCephIntegration,omitempty"`
}

// +k8s:deepcopy-gen=true
type NetworkSpec struct {
	// Public is the CIDR used by Ceph clients (mapped into
	// spec.network.addressRanges.public).
	// +kubebuilder:validation:Required
	Public string `json:"public"`

	// Cluster is the CIDR used for Ceph replication / heartbeat
	// traffic (mapped into spec.network.addressRanges.cluster).
	// +kubebuilder:validation:Required
	Cluster string `json:"cluster"`
}

// +k8s:deepcopy-gen=true
type DaemonCount struct {
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	Count int32 `json:"count,omitempty"`
}

// +k8s:deepcopy-gen=true
type StorageSpec struct {
	// LVM backs OSDs with LVMLogicalVolumes + local PVs, using a
	// pre-created manual StorageClass (see openapi
	// osdStorageClassName in the module values).
	// +optional
	LVM *LVMStorageSpec `json:"lvm,omitempty"`

	// Devices backs OSDs with raw block devices on the host.
	// +optional
	Devices *DevicesStorageSpec `json:"devices,omitempty"`
}

// +k8s:deepcopy-gen=true
type LVMStorageSpec struct {
	// LVGNamePrefix is the prefix of LVMVolumeGroup names; one LLV
	// is created per node as <LVGNamePrefix>-<i>.
	// +kubebuilder:validation:Required
	LVGNamePrefix string `json:"lvgNamePrefix"`

	// ActualLVName is the actual LV name created inside each VG.
	// +kubebuilder:validation:Required
	ActualLVName string `json:"actualLVName"`

	// ActualVGName is the actual VG name on the node used to build
	// the local path /dev/<ActualVGName>/<ActualLVName>.
	// +kubebuilder:validation:Required
	ActualVGName string `json:"actualVGName"`

	// LVSize is the size of each per-node LV (e.g. "100Gi").
	// +kubebuilder:validation:Required
	LVSize resource.Quantity `json:"lvSize"`

	// NodeNamePrefix is the hostname prefix; each node is matched
	// as <NodeNamePrefix>-<i> via kubernetes.io/hostname.
	// +kubebuilder:validation:Required
	NodeNamePrefix string `json:"nodeNamePrefix"`

	// NodeCount is the number of nodes / OSDs to create.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	NodeCount int32 `json:"nodeCount"`
}

// +k8s:deepcopy-gen=true
type DevicesStorageSpec struct {
	// UseAllNodes selects every node for OSD placement.
	// +kubebuilder:default=true
	UseAllNodes bool `json:"useAllNodes,omitempty"`

	// UseAllDevices, when true, ignores DeviceFilter and consumes
	// every eligible device on each selected node.
	// +kubebuilder:default=false
	UseAllDevices bool `json:"useAllDevices,omitempty"`

	// DeviceFilter is a regex that selects which block devices Rook
	// consumes as OSDs on each selected node. Two forms are accepted:
	//   - device-name regex, e.g. "^vd[b-f]" or "^sdc$"; routed to Rook
	//     spec.storage.deviceFilter.
	//   - device-path regex (anchored with "/" or containing "/dev/"),
	//     e.g. "^/dev/sdc$"; routed to Rook spec.storage.devicePathFilter.
	// The controller picks the correct Rook field automatically.
	// +optional
	DeviceFilter string `json:"deviceFilter,omitempty"`
}

// +k8s:deepcopy-gen=true
type BlockPoolSpec struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// FailureDomain is the CRUSH failure domain ("host", "osd", ...).
	// +kubebuilder:default=host
	FailureDomain string `json:"failureDomain,omitempty"`

	// Replicated enables a replicated pool. Exactly one of
	// Replicated or ErasureCoded must be set.
	// +optional
	Replicated *ReplicatedSpec `json:"replicated,omitempty"`

	// ErasureCoded enables an erasure-coded pool. Exactly one of
	// Replicated or ErasureCoded must be set.
	// +optional
	ErasureCoded *ErasureCodedSpec `json:"erasureCoded,omitempty"`
}

// +k8s:deepcopy-gen=true
type ReplicatedSpec struct {
	// +kubebuilder:validation:Minimum=1
	Size int32 `json:"size"`

	// RequireSafeReplicaSize defaults to true in Ceph; set false to
	// allow size=1 unsafe pools.
	// +optional
	RequireSafeReplicaSize *bool `json:"requireSafeReplicaSize,omitempty"`
}

// +k8s:deepcopy-gen=true
type ErasureCodedSpec struct {
	// +kubebuilder:validation:Minimum=1
	DataChunks int32 `json:"dataChunks"`

	// +kubebuilder:validation:Minimum=1
	CodingChunks int32 `json:"codingChunks"`
}

// +k8s:deepcopy-gen=true
type FilesystemSpec struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// MetadataPool configures the CephFS metadata pool.
	// +kubebuilder:validation:Required
	MetadataPool PoolConfig `json:"metadataPool"`

	// DataPools is the ordered list of CephFS data pools.
	// +kubebuilder:validation:MinItems=1
	DataPools []NamedPoolConfig `json:"dataPools"`

	// MetadataServer mirrors spec.metadataServer of CephFilesystem.
	// +optional
	MetadataServer *MetadataServerSpec `json:"metadataServer,omitempty"`

	// PreserveFilesystemOnDelete, when true, keeps the CephFS data
	// when the CR is deleted.
	// +kubebuilder:default=true
	PreserveFilesystemOnDelete bool `json:"preserveFilesystemOnDelete,omitempty"`
}

// +k8s:deepcopy-gen=true
type PoolConfig struct {
	// +kubebuilder:default=host
	FailureDomain string `json:"failureDomain,omitempty"`

	// +optional
	Replicated *ReplicatedSpec `json:"replicated,omitempty"`

	// +optional
	ErasureCoded *ErasureCodedSpec `json:"erasureCoded,omitempty"`
}

// +k8s:deepcopy-gen=true
type NamedPoolConfig struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	PoolConfig `json:",inline"`
}

// +k8s:deepcopy-gen=true
type MetadataServerSpec struct {
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	ActiveCount int32 `json:"activeCount,omitempty"`

	// +kubebuilder:default=true
	ActiveStandby bool `json:"activeStandby,omitempty"`
}

// +k8s:deepcopy-gen=true
type ObjectStoreSpec struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// MetadataPool configures the RGW metadata pool.
	// +kubebuilder:validation:Required
	MetadataPool PoolConfig `json:"metadataPool"`

	// DataPool configures the RGW data pool.
	// +kubebuilder:validation:Required
	DataPool PoolConfig `json:"dataPool"`

	// PreservePoolsOnDelete, when true, keeps the underlying pools
	// when the CR is deleted.
	// +kubebuilder:default=true
	PreservePoolsOnDelete bool `json:"preservePoolsOnDelete,omitempty"`

	// Gateway configures the RGW gateway pods.
	// +kubebuilder:default={port: 80, instances: 1}
	Gateway GatewaySpec `json:"gateway,omitempty"`
}

// +k8s:deepcopy-gen=true
type GatewaySpec struct {
	// +kubebuilder:default=80
	Port int32 `json:"port,omitempty"`

	// +optional
	SecurePort int32 `json:"securePort,omitempty"`

	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Instances int32 `json:"instances,omitempty"`
}

// +k8s:deepcopy-gen=true
type CsiCephIntegrationSpec struct {
	// Enabled toggles automatic creation of CephClusterConnection
	// and CephStorageClass resources for csi-ceph.
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// ConnectionName is the name of the CephClusterConnection CR
	// that this controller manages.
	// +kubebuilder:default="ceph-cluster-connection"
	ConnectionName string `json:"connectionName,omitempty"`

	// UserID overrides the Ceph user ID written to
	// CephClusterConnection.spec.userID. Defaults to the value of
	// ROOK_CEPH_USERNAME from the rook-ceph-mon secret (usually
	// "client.admin", stripped to "admin").
	// +optional
	UserID string `json:"userID,omitempty"`
}

// +k8s:deepcopy-gen=true
type SdsElasticClusterStatus struct {
	// ObservedGeneration is the most recent .metadata.generation
	// reconciled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is a coarse-grained summary derived from Conditions:
	// Pending, InProgress, Ready, Error.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions hold the latest stage states.
	// Known types: StorageReady, CephClusterReady, PoolsReady,
	// FilesystemsReady, ObjectStoresReady, CsiCephReady, Ready.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ClusterID is the FSID of the deployed CephCluster (taken from
	// spec.security.clusterID of CephCluster status or via rook-ceph-mon
	// secret). Echoed back for convenience.
	// +optional
	ClusterID string `json:"clusterID,omitempty"`
}

// Well-known condition types.
const (
	ConditionStorageReady      = "StorageReady"
	ConditionCephClusterReady  = "CephClusterReady"
	ConditionPoolsReady        = "PoolsReady"
	ConditionFilesystemsReady  = "FilesystemsReady"
	ConditionObjectStoresReady = "ObjectStoresReady"
	ConditionCsiCephReady      = "CsiCephReady"
	ConditionReady             = "Ready"
)

// Well-known phases (derived from conditions).
const (
	PhasePending    = "Pending"
	PhaseInProgress = "InProgress"
	PhaseReady      = "Ready"
	PhaseError      = "Error"
)

// Finalizer placed on the SdsElasticCluster CR.
const Finalizer = "storage.deckhouse.io/sds-elastic-cluster"
