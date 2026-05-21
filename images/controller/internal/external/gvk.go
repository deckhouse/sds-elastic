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

// Rook (ceph.rook.io/v1).
var (
	CephClusterGVK = schema.GroupVersionKind{
		Group:   "ceph.rook.io",
		Version: "v1",
		Kind:    "CephCluster",
	}
	CephBlockPoolGVK = schema.GroupVersionKind{
		Group:   "ceph.rook.io",
		Version: "v1",
		Kind:    "CephBlockPool",
	}
	CephFilesystemGVK = schema.GroupVersionKind{
		Group:   "ceph.rook.io",
		Version: "v1",
		Kind:    "CephFilesystem",
	}
	CephObjectStoreGVK = schema.GroupVersionKind{
		Group:   "ceph.rook.io",
		Version: "v1",
		Kind:    "CephObjectStore",
	}
)

// sds-node-configurator (storage.deckhouse.io/v1alpha1).
var (
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
	RookCephMonSecretName       = "rook-ceph-mon"
	RookCephMonSecretUsernameKey = "ceph-username"
	RookCephMonSecretKeyKey      = "ceph-secret"
	RookCephMonSecretFSIDKey     = "fsid"

	RookCephMonEndpointsConfigMap = "rook-ceph-mon-endpoints"
	RookCephMonEndpointsDataKey   = "data"
)

// Labels applied to every resource managed by the controller.
const (
	ManagedByLabelKey   = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "sds-elastic"
	ClusterOwnerLabel   = "storage.deckhouse.io/sds-elastic-cluster"
)
