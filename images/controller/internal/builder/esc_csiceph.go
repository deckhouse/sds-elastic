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

// ESCCephStorageClassName is the csi-ceph CephStorageClass name. 1:1 with the
// ESC's metadata.name (which is also the K8s StorageClass name produced by
// csi-ceph downstream — single identifier across the API).
func ESCCephStorageClassName(esc *v1alpha1.ElasticStorageClass) string {
	return esc.Name
}

// ESCCephStorageClass builds an unstructured CephStorageClass CR for csi-ceph
// based on an ElasticStorageClass. The connectionName is the EC name (1:1 with
// the CephClusterConnection produced by ECCephClusterConnection).
//
// For type=RBD the pool field is set to the CephBlockPool name (== ESC name).
// For type=CephFS the fsName field is set to the CephFilesystem name.
func ESCCephStorageClass(
	esc *v1alpha1.ElasticStorageClass,
	connectionName string,
) (*unstructured.Unstructured, error) {
	spec := map[string]interface{}{
		"clusterConnectionName": connectionName,
		"reclaimPolicy":         "Delete",
		"type":                  string(esc.Spec.Type),
	}

	switch esc.Spec.Type {
	case v1alpha1.StorageClassTypeRBD:
		spec["rbd"] = map[string]interface{}{
			"defaultFSType": "ext4",
			"pool":          ESCRBDPoolName(esc),
		}
	case v1alpha1.StorageClassTypeCephFS:
		spec["cephFS"] = map[string]interface{}{
			"fsName": ESCCephFSName(esc),
		}
	default:
		return nil, fmt.Errorf("unsupported ElasticStorageClass type %q", esc.Spec.Type)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephStorageClassGVK)
	obj.SetName(ESCCephStorageClassName(esc))
	obj.SetLabels(ESCManagedLabels(esc))
	obj.Object["spec"] = spec
	return obj, nil
}
