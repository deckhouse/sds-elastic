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

package builder

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

const (
	// DefaultConnectionName is the default CephClusterConnection name used
	// when the user omits spec.csiCephIntegration.connectionName.
	DefaultConnectionName = "ceph-cluster-connection"

	// RBDDefaultFSType is the default fs type for RBD-backed PVs.
	RBDDefaultFSType = "ext4"
)

// CephClusterConnection builds an unstructured CephClusterConnection CR
// (storage.deckhouse.io/v1alpha1, owned by csi-ceph).
func CephClusterConnection(cluster *v1alpha1.SdsElasticCluster, name, clusterID, userID, userKey string, monitors []string, hasCephFS bool) *unstructured.Unstructured {
	mons := make([]interface{}, 0, len(monitors))
	for _, m := range monitors {
		mons = append(mons, m)
	}
	spec := map[string]interface{}{
		"clusterID": clusterID,
		"monitors":  mons,
		"userID":    userID,
		"userKey":   userKey,
	}
	if hasCephFS {
		spec["cephFS"] = map[string]interface{}{
			"subvolumeGroup": "csi",
		}
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephClusterConnectionGVK)
	obj.SetName(name)
	obj.SetLabels(ManagedLabels(cluster.Name))
	obj.Object["spec"] = spec
	return obj
}

// CephStorageClassRBDName returns the deterministic SC name for an RBD pool.
func CephStorageClassRBDName(poolName string) string {
	return fmt.Sprintf("sds-elastic-rbd-%s", poolName)
}

// CephStorageClassCephFSName returns the deterministic SC name for a CephFS.
func CephStorageClassCephFSName(fsName string) string {
	return fmt.Sprintf("sds-elastic-cephfs-%s", fsName)
}

// CephStorageClassRBD builds an unstructured CephStorageClass CR of type RBD.
func CephStorageClassRBD(cluster *v1alpha1.SdsElasticCluster, connectionName, poolName string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"clusterConnectionName": connectionName,
		"reclaimPolicy":         "Delete",
		"type":                  "RBD",
		"rbd": map[string]interface{}{
			"defaultFSType": RBDDefaultFSType,
			"pool":          poolName,
		},
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephStorageClassGVK)
	obj.SetName(CephStorageClassRBDName(poolName))
	obj.SetLabels(ManagedLabels(cluster.Name))
	obj.Object["spec"] = spec
	return obj
}

// CephStorageClassCephFS builds an unstructured CephStorageClass CR of type
// CephFS.
func CephStorageClassCephFS(cluster *v1alpha1.SdsElasticCluster, connectionName, fsName string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"clusterConnectionName": connectionName,
		"reclaimPolicy":         "Delete",
		"type":                  "CephFS",
		"cephFS": map[string]interface{}{
			"fsName": fsName,
		},
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephStorageClassGVK)
	obj.SetName(CephStorageClassCephFSName(fsName))
	obj.SetLabels(ManagedLabels(cluster.Name))
	obj.Object["spec"] = spec
	return obj
}
