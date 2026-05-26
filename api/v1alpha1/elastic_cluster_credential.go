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

// ElasticClusterCredential is an internal cluster-scoped CR that backs up
// the Ceph cluster identity (FSID, mon-secret, admin-secret) outside the
// module namespace. It is managed exclusively by the sds-elastic controller
// and its lifecycle is bound to the parent ElasticCluster via ownerReferences
// (cascade delete on EC removal).
//
// In the current release only the BACK-SYNC direction is implemented: the
// controller populates spec from the rook-ceph-mon Secret. The reverse
// RESTORE flow (re-creating the Secret from spec on namespace re-create)
// will be added in a subsequent release.
//
// +kubebuilder:resource:scope=Cluster,shortName=ecc
// +kubebuilder:subresource:status
// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ElasticClusterCredential struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ElasticClusterCredentialSpec    `json:"spec"`
	Status *ElasticClusterCredentialStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ElasticClusterCredentialList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []ElasticClusterCredential `json:"items"`
}

// ElasticClusterCredentialSpec is intentionally not
// "required: [fsid, monSecret, adminSecret]": the controller back-syncs fields
// incrementally and the resource exists in a transient empty state between
// Get-or-Create and the first successful read of the rook-ceph-mon Secret.
// The fully-populated state (Phase=Populated with all three fields set) is
// enforced by the validating webhook.
//
// +k8s:deepcopy-gen=true
type ElasticClusterCredentialSpec struct {
	// FSID is the Ceph cluster FSID/UUID, copied from data.fsid of the
	// rook-ceph-mon Secret. Immutable after first population.
	// +optional
	FSID string `json:"fsid,omitempty"`

	// MonSecret is copied from data.mon-secret of the rook-ceph-mon
	// Secret.
	// +optional
	MonSecret string `json:"monSecret,omitempty"`

	// AdminSecret is copied from data.admin-secret of the rook-ceph-mon
	// Secret. Used by csi-ceph as the userKey for the
	// CephClusterConnection in the current release.
	// +optional
	AdminSecret string `json:"adminSecret,omitempty"`
}

// +k8s:deepcopy-gen=true
type ElasticClusterCredentialStatus struct {
	// ObservedGeneration is the most recent .metadata.generation
	// reconciled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is a coarse-grained summary of the back-sync progress.
	// +kubebuilder:validation:Enum=Pending;Populated;Error
	// +optional
	Phase string `json:"phase,omitempty"`

	// LastSyncTime is the timestamp of the last successful BACK-SYNC
	// from the rook-ceph-mon Secret.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
}

// Well-known phases for ElasticClusterCredential.
const (
	ECCPhasePending   = "Pending"
	ECCPhasePopulated = "Populated"
	ECCPhaseError     = "Error"
)

// ElasticClusterCredentialKind is the kind constant used for OwnerReferences
// and dynamic GVK lookups.
const ElasticClusterCredentialKind = "ElasticClusterCredential"
