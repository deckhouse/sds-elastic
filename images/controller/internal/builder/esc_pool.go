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

// ESCManagedLabels returns the common label set placed on every Ceph-side
// resource owned by an ElasticStorageClass (CephBlockPool, CephFilesystem,
// csi-ceph CephStorageClass).
func ESCManagedLabels(esc *v1alpha1.ElasticStorageClass) map[string]string {
	return map[string]string{
		external.ManagedByLabelKey:   external.ManagedByLabelValue,
		external.ECClusterLabel:      esc.Spec.ClusterRef,
		external.ECStorageClassLabel: esc.Name,
	}
}

// ECMetadataPoolReplicatedSize is the replication factor applied to the
// CephFilesystem metadata pool regardless of spec.replication. The metadata
// pool is small and benefits from full 3x replication in every layout.
const ECMetadataPoolReplicatedSize = 3

// ESCRBDPoolName returns the CephBlockPool name for an RBD ESC. 1:1 with the
// ESC's metadata.name so users see a single identifier across the API.
func ESCRBDPoolName(esc *v1alpha1.ElasticStorageClass) string {
	return esc.Name
}

// ESCCephFSName returns the CephFilesystem name for a CephFS ESC.
func ESCCephFSName(esc *v1alpha1.ElasticStorageClass) string {
	return esc.Name
}

// ESCCephFSDataPoolName is the data pool name nested inside a CephFilesystem.
// Rook stores it as <fs-name>-<this-suffix> on the Ceph side; here we just
// expose the suffix used in the CR. The parameter is kept on the
// signature for symmetry with ESCCephFSName / ESCCephBlockPoolName even
// though every ESC currently shares the same `data0` pool name.
func ESCCephFSDataPoolName(_ *v1alpha1.ElasticStorageClass) string {
	return "data0"
}

// ESCCephBlockPool builds an unstructured CephBlockPool for an RBD ESC. The
// pool spec is derived from spec.replication:
//   - AvailabilityWithoutConsistency  -> replicated, size=2, requireSafeReplicaSize=false.
//   - ConsistencyAndAvailability      -> replicated, size=3, requireSafeReplicaSize=true.
//   - HighRedundancy                  -> replicated, size=4, requireSafeReplicaSize=true.
//     Survives 2 simultaneous host failures with continued I/O (2 replicas
//     == default min_size=2 keeps PGs active) and one extra failure as a
//     recovery margin before data loss; the controller auto-promotes
//     CephCluster.spec.mon.count to 5 when at least one HighRedundancy ESC
//     is present, see CephTopologyStatus on ElasticClusterStatus.
//   - ErasureCodedCompact             -> rejected by the validating webhook for type=RBD; this builder is only
//     called after webhook acceptance, so an EC value here is a programming
//     bug and we surface it as an error rather than a partial spec.
//
// min_size is not set explicitly: Ceph derives it from the cluster's
// osd_pool_default_min_size (typically size-1).
func ESCCephBlockPool(esc *v1alpha1.ElasticStorageClass, namespace string) (*unstructured.Unstructured, error) {
	repl, err := rbdReplicated(esc.Spec.Replication)
	if err != nil {
		return nil, err
	}
	spec := map[string]interface{}{
		"failureDomain": "host",
		"replicated":    repl,
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephBlockPoolGVK)
	obj.SetName(ESCRBDPoolName(esc))
	obj.SetNamespace(namespace)
	obj.SetLabels(ESCManagedLabels(esc))
	obj.Object["spec"] = spec
	return obj, nil
}

// ESCCephFilesystem builds an unstructured CephFilesystem for a CephFS ESC.
func ESCCephFilesystem(esc *v1alpha1.ElasticStorageClass, namespace string) (*unstructured.Unstructured, error) {
	dataPool, err := cephfsDataPool(esc.Spec.Replication)
	if err != nil {
		return nil, err
	}
	dataPool["name"] = ESCCephFSDataPoolName(esc)
	dataPool["failureDomain"] = "host"

	spec := map[string]interface{}{
		"metadataPool": map[string]interface{}{
			"failureDomain": "host",
			"replicated": map[string]interface{}{
				"size":                   int64(ECMetadataPoolReplicatedSize),
				"requireSafeReplicaSize": true,
			},
		},
		"dataPools":                  []interface{}{dataPool},
		"preserveFilesystemOnDelete": true,
		"metadataServer": map[string]interface{}{
			"activeCount":   int64(1),
			"activeStandby": true,
		},
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephFilesystemGVK)
	obj.SetName(ESCCephFSName(esc))
	obj.SetNamespace(namespace)
	obj.SetLabels(ESCManagedLabels(esc))
	obj.Object["spec"] = spec
	return obj, nil
}

func rbdReplicated(mode v1alpha1.ReplicationMode) (map[string]interface{}, error) {
	switch mode {
	case v1alpha1.ReplicationAvailabilityWithoutConsistency:
		return map[string]interface{}{
			"size":                   int64(2),
			"requireSafeReplicaSize": false,
		}, nil
	case v1alpha1.ReplicationConsistencyAndAvailability, "":
		return map[string]interface{}{
			"size":                   int64(3),
			"requireSafeReplicaSize": true,
		}, nil
	case v1alpha1.ReplicationHighRedundancy:
		return map[string]interface{}{
			"size":                   int64(4),
			"requireSafeReplicaSize": true,
		}, nil
	case v1alpha1.ReplicationErasureCodedCompact:
		return nil, fmt.Errorf("RBD pool does not support replication=%s", mode)
	default:
		return nil, fmt.Errorf("unsupported RBD replication mode %q", mode)
	}
}

func cephfsDataPool(mode v1alpha1.ReplicationMode) (map[string]interface{}, error) {
	switch mode {
	case v1alpha1.ReplicationAvailabilityWithoutConsistency:
		return map[string]interface{}{
			"replicated": map[string]interface{}{
				"size":                   int64(2),
				"requireSafeReplicaSize": false,
			},
		}, nil
	case v1alpha1.ReplicationConsistencyAndAvailability, "":
		return map[string]interface{}{
			"replicated": map[string]interface{}{
				"size":                   int64(3),
				"requireSafeReplicaSize": true,
			},
		}, nil
	case v1alpha1.ReplicationHighRedundancy:
		return map[string]interface{}{
			"replicated": map[string]interface{}{
				"size":                   int64(4),
				"requireSafeReplicaSize": true,
			},
		}, nil
	case v1alpha1.ReplicationErasureCodedCompact:
		return map[string]interface{}{
			"erasureCoded": map[string]interface{}{
				"dataChunks":   int64(2),
				"codingChunks": int64(2),
				"algorithm":    "jerasure",
				"plugin":       "jerasure",
				"technique":    "reed_sol_van",
			},
			"parameters": map[string]interface{}{
				"allow_ec_overwrites": "true",
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported CephFS replication mode %q", mode)
	}
}
