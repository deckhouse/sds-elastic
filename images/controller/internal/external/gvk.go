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

// Package external collects GroupVersionKind constants for resources owned by
// other modules (Rook, sds-node-configurator, csi-ceph). All of them are
// manipulated as unstructured.Unstructured to avoid hard build-time
// dependencies on the corresponding Go modules.
package external

import "k8s.io/apimachinery/pkg/runtime/schema"

// Rook (internal.sdselastic.deckhouse.io/v1).
//
// The API group is renamed from the upstream ceph.rook.io to
// internal.sdselastic.deckhouse.io by the vendored Rook operator build
// (see images/operator/patches/). All clients here address the renamed
// group so that sds-elastic does not interfere with a user-installed
// upstream Rook on the same cluster.
var (
	CephClusterGVK = schema.GroupVersionKind{
		Group:   "internal.sdselastic.deckhouse.io",
		Version: "v1",
		Kind:    "CephCluster",
	}
	CephBlockPoolGVK = schema.GroupVersionKind{
		Group:   "internal.sdselastic.deckhouse.io",
		Version: "v1",
		Kind:    "CephBlockPool",
	}
	CephFilesystemGVK = schema.GroupVersionKind{
		Group:   "internal.sdselastic.deckhouse.io",
		Version: "v1",
		Kind:    "CephFilesystem",
	}
	CephObjectStoreGVK = schema.GroupVersionKind{
		Group:   "internal.sdselastic.deckhouse.io",
		Version: "v1",
		Kind:    "CephObjectStore",
	}
)

// sds-node-configurator (storage.deckhouse.io/v1alpha1).
var (
	BlockDeviceGVK = schema.GroupVersionKind{
		Group:   "storage.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "BlockDevice",
	}
	LVMVolumeGroupGVK = schema.GroupVersionKind{
		Group:   "storage.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "LVMVolumeGroup",
	}
	LVMLogicalVolumeGVK = schema.GroupVersionKind{
		Group:   "storage.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "LVMLogicalVolume",
	}
)

// csi-ceph (storage.deckhouse.io/v1alpha1).
var (
	CephClusterConnectionGVK = schema.GroupVersionKind{
		Group:   "storage.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "CephClusterConnection",
	}
	CephStorageClassGVK = schema.GroupVersionKind{
		Group:   "storage.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "CephStorageClass",
	}
)

// LVMLogicalVolume finalizer used in the instruction (manual creation).
const LVMLogicalVolumeManualFinalizer = "storage.deckhouse.io/manual-creation"

// Rook secret/configmap names used to source CephClusterConnection data.
const (
	RookCephMonSecretName        = "rook-ceph-mon"
	RookCephMonSecretUsernameKey = "ceph-username"
	RookCephMonSecretFSIDKey     = "fsid"

	// RookCephMonSecretAdminSecretKey is the post-1.13 Rook key for the
	// cephx admin user secret. Newer Rook releases write the rotated
	// admin key here, while older releases (and a number of forks
	// downstream of Deckhouse) only keep RookCephMonSecretCephSecretKey
	// populated. Both are checked in order by the ECC reconciler.
	RookCephMonSecretAdminSecretKey = "admin-secret"

	// RookCephMonSecretCephSecretKey is the legacy Rook key for the
	// cephx admin user secret. Always present (Rook keeps writing it
	// for backward compatibility), so the ECC reconciler treats it as
	// the canonical fallback when admin-secret is absent.
	RookCephMonSecretCephSecretKey = "ceph-secret"

	// RookCephMonSecretMonSecretKey holds the shared mon daemon secret
	// in the rook-ceph-mon Secret. Backed up to ECC.spec.monSecret.
	RookCephMonSecretMonSecretKey = "mon-secret"

	RookCephMonEndpointsConfigMap   = "rook-ceph-mon-endpoints"
	RookCephMonEndpointsDataKey     = "data"
	RookCephMonEndpointsMaxMonIDKey = "maxMonId"
)

// Labels applied to every resource managed by the controller.
const (
	ManagedByLabelKey   = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "sds-elastic"

	// ECClusterLabel marks resources owned by a specific ElasticCluster
	// (LVG, LLV, local PV, Rook CRs derived from it). The value is the
	// ElasticCluster's metadata.name.
	ECClusterLabel = "sds-elastic.deckhouse.io/cluster"

	// ECStorageClassLabel marks resources owned by a specific
	// ElasticStorageClass (Ceph pool / filesystem, csi-ceph SC).
	ECStorageClassLabel = "sds-elastic.deckhouse.io/storage-class"
)

// Naming and host paths used by ElasticCluster reconciliation.
const (
	// ReservedOSDStorageClassName matches v1alpha1.ReservedOSDStorageClassName
	// and is duplicated here so the builder layer does not import the api
	// module just to read a string constant. Keep in sync.
	ReservedOSDStorageClassName = "sds-elastic-osd"

	// CephDataDirHostPathPrefix is the parent directory for all per-EC
	// mon/osd hostPath data: dataDirHostPath = <prefix>/<ec-name>.
	CephDataDirHostPathPrefix = "/opt/deckhouse/sds/elastic"
)
