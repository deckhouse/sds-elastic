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
	"path"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

const (
	// ECOSDStorageClassDeviceSetName is the storageClassDeviceSets[0].name
	// used in CephCluster.spec.storage. A single set is sufficient because
	// every OSD is backed by the same internal StorageClass
	// (ReservedOSDStorageClassName) regardless of the underlying BlockDevice.
	ECOSDStorageClassDeviceSetName = "set1"
)

// ECCephClusterDataDirHostPath returns the dataDirHostPath used by Rook for
// mon and OSD bookkeeping data. Per ElasticCluster to keep multiple clusters
// (future) on the same host isolated.
func ECCephClusterDataDirHostPath(ec *v1alpha1.ElasticCluster) string {
	return path.Join(external.CephDataDirHostPathPrefix, ec.Name)
}

// ECCephCluster builds the unstructured CephCluster CR for an ElasticCluster.
// Hardcoded production-ready values:
//   - cephVersion.image                 — provided by the caller (module image).
//   - dataDirHostPath                   — /opt/deckhouse/sds/elastic/<ec>.
//   - mon.count=3, allowMultiplePerNode=false, no volumeClaimTemplate
//     (mon data sits on the host path; survival across namespace deletion
//     is achieved by ElasticClusterCredential + (in a future release) the
//     monClusterMap status field).
//   - mgr.count=2, allowMultiplePerNode=false, pg_autoscaler enabled.
//   - network.provider=host. addressRanges populated only when
//     ec.Spec.Network is set; otherwise Rook listens on every host IP.
//   - network.connections.{encryption,compression}=false, requireMsgr2=false.
//   - dashboard.enabled=false (no public surface for the demo).
//   - skipUpgradeChecks=false, continueUpgradeAfterChecksEvenIfNotHealthy=false.
//   - One storageClassDeviceSet bound to ReservedOSDStorageClassName; count
//     is supplied by the caller (it equals the number of LLV/PV pairs the
//     storage stage produced). volumeClaimTemplates[0].spec.resources.requests.
//     storage is set to pvcStorageRequest, which the caller must compute as
//     min(BD.size) over the BDs adopted in the storage stage. The K8s PV
//     binder requires PV.capacity >= PVC.requests.storage, so passing the
//     smallest PV's capacity guarantees every set1-data-* PVC binds to one
//     of the local-PVs built by ECOSDPersistentVolume(). Setting the request
//     larger than any PV capacity (e.g. 1Pi) used to leave PVCs Pending and
//     OSD prepare jobs unscheduled.
//   - placement.all is fully assembled by the caller via ECPlacementAll(),
//     including nodeAffinity AND tolerations. Per Rook semantics, placement
//     under "all" propagates to every Ceph role (mon/mgr/osd/mds).
func ECCephCluster(
	ec *v1alpha1.ElasticCluster,
	namespace, cephImage string,
	osdCount int32,
	pvcStorageRequest resource.Quantity,
	placementAll map[string]interface{},
) *unstructured.Unstructured {
	monSpec := map[string]interface{}{
		"count":                int64(3),
		"allowMultiplePerNode": false,
	}

	mgrSpec := map[string]interface{}{
		"count":                int64(2),
		"allowMultiplePerNode": false,
		"modules": []interface{}{
			map[string]interface{}{"name": "pg_autoscaler", "enabled": true},
		},
	}

	network := map[string]interface{}{
		"provider": "host",
		"connections": map[string]interface{}{
			"encryption":   map[string]interface{}{"enabled": false},
			"compression":  map[string]interface{}{"enabled": false},
			"requireMsgr2": false,
		},
	}
	if ec.Spec.Network != nil {
		network["addressRanges"] = map[string]interface{}{
			"public":  []interface{}{ec.Spec.Network.Public},
			"cluster": []interface{}{ec.Spec.Network.Cluster},
		}
	}

	storage := map[string]interface{}{
		"useAllNodes":           false,
		"useAllDevices":         false,
		"onlyApplyOSDPlacement": false,
		"storageClassDeviceSets": []interface{}{
			map[string]interface{}{
				"name":            ECOSDStorageClassDeviceSetName,
				"count":           int64(osdCount),
				"portable":        false,
				"tuneDeviceClass": true,
				"volumeClaimTemplates": []interface{}{
					map[string]interface{}{
						"metadata": map[string]interface{}{"name": "data"},
						"spec": map[string]interface{}{
							"resources": map[string]interface{}{
								"requests": map[string]interface{}{
									"storage": pvcStorageRequest.String(),
								},
							},
							"storageClassName": external.ReservedOSDStorageClassName,
							"volumeMode":       "Block",
							"accessModes":      []interface{}{"ReadWriteOnce"},
						},
					},
				},
			},
		},
	}

	placement := map[string]interface{}{}
	if len(placementAll) > 0 {
		placement["all"] = placementAll
	}

	spec := map[string]interface{}{
		"cephVersion": map[string]interface{}{
			"image":            cephImage,
			"allowUnsupported": false,
		},
		"dataDirHostPath":                            ECCephClusterDataDirHostPath(ec),
		"skipUpgradeChecks":                          false,
		"continueUpgradeAfterChecksEvenIfNotHealthy": false,
		"waitTimeoutForHealthyOSDInMinutes":          int64(10),
		"removeOSDsIfOutAndSafeToRemove":             false,
		"mon":                                        monSpec,
		"mgr":                                        mgrSpec,
		"dashboard": map[string]interface{}{
			"enabled": false,
			"ssl":     false,
		},
		"network": network,
		"storage": storage,
		"labels":  ecCephClusterLabels(),
		"priorityClassNames": map[string]interface{}{
			"mon": "system-node-critical",
			"osd": "system-node-critical",
			"mgr": "system-cluster-critical",
		},
		"crashCollector": map[string]interface{}{"disable": false},
		"logCollector": map[string]interface{}{
			"enabled":     true,
			"periodicity": "daily",
			"maxLogSize":  "100M",
		},
		"cleanupPolicy": map[string]interface{}{
			"confirmation": "",
			"sanitizeDisks": map[string]interface{}{
				"method":     "quick",
				"dataSource": "zero",
				"iteration":  int64(1),
			},
			"allowUninstallWithVolumes": false,
		},
		"disruptionManagement": map[string]interface{}{
			"managePodBudgets":      true,
			"osdMaintenanceTimeout": int64(30),
			"pgHealthCheckTimeout":  int64(0),
		},
		"healthCheck": map[string]interface{}{
			"daemonHealth": map[string]interface{}{
				"mon":    map[string]interface{}{"disabled": false, "interval": "45s"},
				"osd":    map[string]interface{}{"disabled": false, "interval": "60s"},
				"status": map[string]interface{}{"disabled": false, "interval": "60s"},
			},
			"livenessProbe": map[string]interface{}{
				"mon": map[string]interface{}{"disabled": false},
				"mgr": map[string]interface{}{"disabled": false},
				"osd": map[string]interface{}{"disabled": false},
			},
			"startupProbe": map[string]interface{}{
				"mon": map[string]interface{}{"disabled": false},
				"mgr": map[string]interface{}{"disabled": false},
				"osd": map[string]interface{}{"disabled": false},
			},
		},
	}
	if len(placement) > 0 {
		spec["placement"] = placement
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephClusterGVK)
	obj.SetName(ec.Name)
	obj.SetNamespace(namespace)
	obj.SetLabels(ECManagedLabels(ec))
	obj.Object["spec"] = spec
	return obj
}

// ECCephClusterName returns the canonical CephCluster name for an EC. The
// CephCluster lives in the module namespace; its name is the EC name 1:1.
func ECCephClusterName(ec *v1alpha1.ElasticCluster) string {
	return ec.Name
}

func ecCephClusterLabels() map[string]interface{} {
	return map[string]interface{}{
		"mon":        map[string]interface{}{"ceph-component": "mon"},
		"prepareosd": map[string]interface{}{"ceph-component": "osd-prepare"},
		"osd":        map[string]interface{}{"ceph-component": "osd"},
		"mgr": map[string]interface{}{
			"ceph-component":                        "mgr",
			"prometheus.deckhouse.io/custom-target": "ceph",
			"prometheus.deckhouse.io/port":          "9283",
		},
	}
}
