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

	// Health is the latest Ceph health summary surfaced by Rook
	// (CephCluster.status.ceph.health and the matching detail blocks).
	// Drives the top-level OK / Warn / Err indicator on the UI dashboard.
	// +optional
	Health *CephHealthStatus `json:"health,omitempty"`

	// Capacity reports cluster-wide storage usage as observed by Rook.
	// Sourced from CephCluster.status.ceph.capacity. Empty until the
	// CephCluster reports its first capacity probe.
	// +optional
	Capacity *CephCapacityStatus `json:"capacity,omitempty"`

	// OSDs summarises OSD daemon count and the version histogram Rook
	// publishes under CephCluster.status.ceph.versions.osd. `desired` is
	// the OSD count the controller asked Rook for (== matched
	// BlockDevices); `knownToCeph` is the sum of CephCluster.status.ceph.
	// versions.osd values (daemons that have reported a version to Ceph
	// since the last heartbeat); `byVersion` is the version → count
	// breakdown, useful during rolling upgrades to spot daemons still on
	// the old version.
	// +optional
	OSDs *OSDStatus `json:"osds,omitempty"`

	// Mons reports the number of Ceph monitor daemons known to Ceph and
	// their version histogram. Sourced from CephCluster.status.ceph.
	// versions.mon. UI uses `knownToCeph` to render a coarse quorum
	// indicator (`knownToCeph >= floor(spec.mon.count/2)+1`).
	// +optional
	Mons *DaemonStatus `json:"mons,omitempty"`

	// Mgrs reports the number of Ceph manager daemons known to Ceph and
	// their version histogram. Sourced from CephCluster.status.ceph.
	// versions.mgr. Rook deploys a single active manager by default
	// (with optional standbys), so `knownToCeph == 1` is the typical
	// healthy value.
	// +optional
	Mgrs *DaemonStatus `json:"mgrs,omitempty"`

	// CephTopology records the effective mon/mgr counts the controller
	// asked Rook to apply. Promotion is sticky (monotonic): once a
	// HighRedundancy ESC raises the counts to the high-availability
	// profile (mon=5, mgr=3), they are never lowered back even if the
	// trigger is removed. See CephTopologyStatus for the rationale.
	// +optional
	CephTopology *CephTopologyStatus `json:"cephTopology,omitempty"`

	// Conditions hold the latest stage states. Known types:
	// StorageReady, CephClusterReady, CredentialsReady, CsiCephReady,
	// UpgradeReady, UpgradeInProgress, Ready.
	//
	// Well-known StorageReady reasons (machine-readable) when the stage
	// is not yet Ready:
	//   - NoBlockDevices             — selector matched no BDs.
	//   - WaitingForBlockDeviceCRD   — sds-node-configurator BD CRD is
	//     missing (module not deployed yet).
	//   - WaitingForLVMVolumeGroupCRD / WaitingForLVMLogicalVolumeCRD
	//     — same, for the LVG/LLV CRDs.
	//   - WaitingForBlockDevices     — some BDs failed validation
	//     (empty nodeName or unparseable size).
	//   - WaitingForLVMVolumeGroup   — LVG CRs upserted but
	//     status.phase != Ready on at least one member.
	//   - WaitingForLVMLogicalVolume — LLV CRs upserted but
	//     status.phase != Created on at least one member.
	//   - WaitingForPersistentVolume — local PVs created but K8s
	//     binder has not transitioned them to Available/Bound.
	//
	// The condition message lists the laggard resource names with their
	// observed phase so a UI can dispatch on Reason and inspect Message
	// for the per-resource breakdown.
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

// CephHealthStatus mirrors the CephCluster.status.ceph health summary
// in a typed shape. UI renders `Status` as a colour token (HEALTH_OK
// green, HEALTH_WARN yellow, HEALTH_ERR red) and a list of the active
// `Checks` so the operator does not need to drill into the raw Rook CR.
//
// +k8s:deepcopy-gen=true
type CephHealthStatus struct {
	// Status is one of HEALTH_OK / HEALTH_WARN / HEALTH_ERR. Missing
	// when CephCluster has not yet published a health assessment.
	// +kubebuilder:validation:Enum=HEALTH_OK;HEALTH_WARN;HEALTH_ERR
	// +optional
	Status string `json:"status,omitempty"`

	// Message is the short human-readable summary Rook attaches to
	// the health probe. Empty when status is HEALTH_OK.
	// +optional
	Message string `json:"message,omitempty"`

	// LastChecked is the timestamp of the latest health probe Rook
	// successfully observed against the cluster.
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`

	// Checks lists the active health checks (warnings and errors)
	// reported by Ceph. Empty on HEALTH_OK. The slice is bounded so
	// the status never grows past the etcd object size limit.
	// +optional
	Checks []CephHealthCheck `json:"checks,omitempty"`
}

// +k8s:deepcopy-gen=true
type CephHealthCheck struct {
	// Name is the Ceph check identifier, for example MON_DOWN,
	// OSD_NEARFULL, POOL_NO_REDUNDANCY.
	Name string `json:"name"`

	// Severity mirrors the per-check severity Rook publishes
	// (HEALTH_WARN / HEALTH_ERR).
	// +optional
	Severity string `json:"severity,omitempty"`

	// Message is the human-readable description Rook surfaces
	// alongside the check.
	// +optional
	Message string `json:"message,omitempty"`
}

// CephCapacityStatus mirrors CephCluster.status.ceph.capacity. The
// raw byte counters Rook publishes are exposed as resource.Quantity
// (BinarySI) so a UI / kubectl printer column can render "1.2Ti" out
// of the box, matching the convention sds-node-configurator uses on
// its own size fields (LVMVolumeGroupStatus.{vgSize,vgFree}).
//
// +k8s:deepcopy-gen=true
type CephCapacityStatus struct {
	// Total is the total raw capacity of the cluster as seen by Rook
	// (sum over all OSDs). On the wire: a Kubernetes Quantity string,
	// for example "500Gi" or "1.2Ti".
	// +optional
	Total resource.Quantity `json:"total,omitempty"`

	// Used is the consumed raw capacity (data + metadata + Ceph
	// overhead). Quantity, see Total.
	// +optional
	Used resource.Quantity `json:"used,omitempty"`

	// Available is the free raw capacity. Note that the usable
	// capacity for client I/O depends on pool-level replication
	// (~ Available / pool.size for replicated pools). Quantity, see
	// Total.
	// +optional
	Available resource.Quantity `json:"available,omitempty"`

	// UsedPercent is Used / Total * 100, formatted with two decimal
	// places. Stored as a string so a trailing decimal is preserved
	// verbatim for the UI without floating-point parsing.
	// +optional
	UsedPercent string `json:"usedPercent,omitempty"`

	// LastUpdated is the timestamp of the latest capacity probe Rook
	// successfully observed against the cluster.
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
}

// OSDStatus reports OSD daemon count plus the per-version histogram
// Rook publishes under CephCluster.status.ceph.versions.osd. There is
// intentionally no "running" / "byNode" surface here — Rook does not
// expose either on CephCluster.status, and we explicitly chose not to
// list rook-ceph-osd Pods just to derive them. Operators still see
// per-Pod state via `kubectl get pod -l app=rook-ceph-osd`.
//
// +k8s:deepcopy-gen=true
type OSDStatus struct {
	// Desired is the OSD count the controller asked Rook for, equal
	// to the number of BlockDevices ensureStorage adopted for this
	// ElasticCluster. Surfaced verbatim, even before Rook has scheduled
	// any rook-ceph-osd Pod, so a UI can render "Provisioning N OSDs"
	// from the moment the EC is created.
	// +optional
	Desired int32 `json:"desired,omitempty"`

	// KnownToCeph is sum(CephCluster.status.ceph.versions.osd values).
	// Rook removes a daemon entry once it has not reported a version
	// for a while, so this is a "alive in Ceph's eyes" approximation —
	// not Pod readiness, not OSD up/in. Useful as a rough "9 of 9 OSDs
	// reachable" indicator.
	// +optional
	KnownToCeph int32 `json:"knownToCeph,omitempty"`

	// ByVersion reports the per-version histogram Rook publishes for
	// OSD daemons. Sorted by count desc, version asc. Empty when no
	// OSD has reported a version yet (cluster bootstrapping).
	// +optional
	ByVersion []DaemonVersionCount `json:"byVersion,omitempty"`
}

// DaemonStatus is the shared shape for mon and mgr observability,
// derived from CephCluster.status.ceph.versions.{mon,mgr}. Same caveats
// apply as for OSDStatus — these are Ceph-level counts, not Pod
// readiness counters.
//
// +k8s:deepcopy-gen=true
type DaemonStatus struct {
	// KnownToCeph is sum(versions[<kind>] values) for this daemon kind.
	// +optional
	KnownToCeph int32 `json:"knownToCeph,omitempty"`

	// ByVersion is the per-version histogram for this daemon kind.
	// Same ordering as OSDStatus.ByVersion.
	// +optional
	ByVersion []DaemonVersionCount `json:"byVersion,omitempty"`
}

// DaemonVersionCount is a single (version, count) entry of the
// CephCluster.status.ceph.versions.<kind> histogram. The Version is
// the full Ceph version string Rook publishes verbatim, for example
// "ceph version 19.2.3 (...) squid (stable)".
//
// +k8s:deepcopy-gen=true
type DaemonVersionCount struct {
	// Version is the Ceph version string Rook reports for this bucket.
	Version string `json:"version"`

	// Count is the number of daemons of the parent kind reporting this
	// version.
	Count int32 `json:"count"`
}

// CephTopologyStatus encodes the effective Ceph daemon counts the
// controller has asked Rook to apply for this ElasticCluster's
// underlying CephCluster, plus an audit trail explaining the most
// recent change.
//
// Promotion is monotonic ("sticky high-water-mark"): once MonCount /
// MgrCount have been raised — typically because at least one
// HighRedundancy ESC was created against this EC, demanding a
// double-fault-tolerant mon quorum — they are not lowered back even
// when the trigger is removed. Silently weakening the fault-tolerance
// guarantee on a live cluster would invalidate any disaster-recovery
// promise an operator made downstream and is treated as an explicit
// out-of-band action: clearing this status field by hand causes the
// next reconcile to recompute from defaults.
//
// +k8s:deepcopy-gen=true
type CephTopologyStatus struct {
	// MonCount is the effective Rook mon.count applied to
	// CephCluster.spec. The controller derives the desired value from
	// (a) the standard pair (3, 2) and (b) any HighRedundancy ESC
	// referencing this EC (which raises it to 5), then takes the
	// element-wise max with the previously-recorded value to enforce
	// stickiness.
	// +kubebuilder:validation:Minimum=1
	MonCount int32 `json:"monCount"`

	// MgrCount is the effective Rook mgr.count. Same machinery as
	// MonCount: standard 2, HighRedundancy 3, monotonic max.
	// +kubebuilder:validation:Minimum=1
	MgrCount int32 `json:"mgrCount"`

	// Reason is a machine-readable explanation of the latest change.
	// Known values:
	//   - "Standard": defaults are in effect; no promotion has ever
	//     happened (or the operator has manually reset the field).
	//   - "HighRedundancyESCPresent": at least one ESC with
	//     replication=HighRedundancy currently references this EC,
	//     and the desired counts derived from that match the recorded
	//     ones.
	//   - "StickyHighWaterMark": the recorded counts are higher than
	//     what the live ESC inventory currently demands; the prior
	//     promotion is being preserved.
	// +optional
	Reason string `json:"reason,omitempty"`

	// LastPromotedAt is the timestamp of the most recent (Mon|Mgr)Count
	// increase. Empty until the first promotion. Reads of this field
	// alongside Reason let a UI distinguish "we are currently in HA
	// mode because an r4 ESC exists" from "we are still in HA mode
	// because the high-water-mark was never reset, even though the
	// triggering ESC is gone".
	// +optional
	LastPromotedAt *metav1.Time `json:"lastPromotedAt,omitempty"`
}

// CephTopology Reason values. Exposed as constants because dashboards
// and the controller dispatcher both consume them.
const (
	CephTopologyReasonStandard                 = "Standard"
	CephTopologyReasonHighRedundancyESCPresent = "HighRedundancyESCPresent"
	CephTopologyReasonStickyHighWaterMark      = "StickyHighWaterMark"
)

// +k8s:deepcopy-gen=true
type CephVersionStatus struct {
	// Requested is the Ceph version the controller currently asks Rook
	// to run (matches the module image).
	// +optional
	Requested string `json:"requested,omitempty"`

	// Running is the Ceph version actually present in the cluster,
	// derived from CephCluster.status.version.version.
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

// Well-known condition reasons that surface on ElasticCluster.status.
// These are the strings UI / dashboards filter on, so they are part of
// the public contract.
const (
	// ECReasonOwnershipConflict is set on StorageReady when at least
	// one BlockDevice matching ec.spec.storage.blockDeviceSelector is
	// already labelled `sds-elastic.deckhouse.io/cluster=<otherEC>`.
	// The controller refuses to overwrite the foreign claim — the
	// operator must clear the label manually (or delete the
	// conflicting EC) before this EC can adopt the BD.
	ECReasonOwnershipConflict = "OwnershipConflict"
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

// Supported Ceph container image tags built by the sds-elastic module
// (images/ceph/werf.inc.yaml, one variant per tag).
const (
	CephVersionV1923 = "v19.2.3"
	CephVersionV2021 = "v20.2.1"
)

// DefaultCephVersion is used when spec.cephVersion is omitted.
const DefaultCephVersion = CephVersionV2021

// SupportedCephVersions lists every allowed spec.cephVersion value.
var SupportedCephVersions = []string{CephVersionV1923, CephVersionV2021}

// Status.phase values shared across ElasticCluster and ElasticStorageClass.
const (
	PhasePending    = "Pending"
	PhaseInProgress = "InProgress"
	PhaseReady      = "Ready"
	PhaseError      = "Error"
)
