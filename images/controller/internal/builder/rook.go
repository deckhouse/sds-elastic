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
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

const (
	// CephClusterName is the well-known name of the CephCluster CR managed
	// by this controller in the module namespace.
	CephClusterName = "ceph-cluster"

	// CephOSDStorageClassDeviceSet is the name of the OSD storage class
	// device set created when LVM storage mode is selected.
	CephOSDStorageClassDeviceSet = "set1"
)

// CephCluster builds the unstructured CephCluster CR, equivalent to the YAML
// from instruction.md "Поднимаем Ceph кластер с использованием Rook".
func CephCluster(cluster *v1alpha1.SdsElasticCluster, namespace, osdStorageClassName string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"cephVersion": map[string]interface{}{
			"image":              fmt.Sprintf("quay.io/ceph/ceph:%s", defaultIfEmpty(cluster.Spec.CephVersion, "v18.2.7")),
			"allowUnsupported":   true,
		},
		"dataDirHostPath":                            "/var/lib/rook",
		"skipUpgradeChecks":                          false,
		"continueUpgradeAfterChecksEvenIfNotHealthy": false,
		"waitTimeoutForHealthyOSDInMinutes":          int64(10),
		"mon": map[string]interface{}{
			"count":                int64(daemonOrDefault(cluster.Spec.Mon.Count, 3)),
			"allowMultiplePerNode": false,
		},
		"mgr": map[string]interface{}{
			"count":                int64(daemonOrDefault(cluster.Spec.Mgr.Count, 3)),
			"allowMultiplePerNode": false,
			"modules": []interface{}{
				map[string]interface{}{"name": "pg_autoscaler", "enabled": true},
			},
		},
		"dashboard": map[string]interface{}{
			"enabled": false,
			"ssl":     false,
		},
		"annotations": map[string]interface{}{
			"mgr": map[string]interface{}{
				"prometheus.deckhouse.io/sample-limit": "10000",
			},
		},
		"network": map[string]interface{}{
			"provider": "host",
			"addressRanges": map[string]interface{}{
				"public":  []interface{}{cluster.Spec.Network.Public},
				"cluster": []interface{}{cluster.Spec.Network.Cluster},
			},
			"connections": map[string]interface{}{
				"encryption":   map[string]interface{}{"enabled": false},
				"compression":  map[string]interface{}{"enabled": false},
				"requireMsgr2": false,
			},
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
		"labels": map[string]interface{}{
			"mon":        map[string]interface{}{"ceph-component": "mon"},
			"prepareosd": map[string]interface{}{"ceph-component": "osd-prepare"},
			"osd":        map[string]interface{}{"ceph-component": "osd"},
			"mgr": map[string]interface{}{
				"ceph-component":                              "mgr",
				"prometheus.deckhouse.io/custom-target":       "ceph",
				"prometheus.deckhouse.io/port":                "9283",
			},
		},
		"removeOSDsIfOutAndSafeToRemove": false,
		"priorityClassNames": map[string]interface{}{
			"mon": "system-node-critical",
			"osd": "system-node-critical",
			"mgr": "system-cluster-critical",
		},
		"storage":              buildStorage(cluster, osdStorageClassName),
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

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephClusterGVK)
	obj.SetName(CephClusterName)
	obj.SetNamespace(namespace)
	obj.SetLabels(ManagedLabels(cluster.Name))
	obj.Object["spec"] = spec
	return obj
}

func buildStorage(cluster *v1alpha1.SdsElasticCluster, osdStorageClassName string) map[string]interface{} {
	storage := map[string]interface{}{
		"useAllNodes":           true,
		"useAllDevices":         false,
		"onlyApplyOSDPlacement": false,
	}

	if cluster.Spec.Storage.Devices != nil {
		d := cluster.Spec.Storage.Devices
		storage["useAllNodes"] = d.UseAllNodes
		storage["useAllDevices"] = d.UseAllDevices
		// Rook has two separate fields: `deviceFilter` matches the device
		// short name ("sdc"), while `devicePathFilter` matches the full
		// path ("/dev/sdc"). Users routinely write `^/dev/sdc$` in their
		// CR, which silently never matches the short-name regex and the
		// OSD prepare jobs come back with empty `osd versions detected`.
		// Auto-route the filter to the correct Rook field by looking at
		// whether the regex contains the `/dev/` prefix or a literal "/".
		if d.DeviceFilter != "" {
			if isDevicePathRegex(d.DeviceFilter) {
				storage["devicePathFilter"] = d.DeviceFilter
			} else {
				storage["deviceFilter"] = d.DeviceFilter
			}
		}
		return storage
	}

	if cluster.Spec.Storage.LVM != nil {
		lvm := cluster.Spec.Storage.LVM
		storage["deviceFilter"] = "^vd[b-f]"
		storage["storageClassDeviceSets"] = []interface{}{
			map[string]interface{}{
				"name":            CephOSDStorageClassDeviceSet,
				"count":           int64(lvm.NodeCount),
				"portable":        false,
				"tuneDeviceClass": true,
				"volumeClaimTemplates": []interface{}{
					map[string]interface{}{
						"metadata": map[string]interface{}{"name": "data"},
						"spec": map[string]interface{}{
							"resources": map[string]interface{}{
								"requests": map[string]interface{}{
									"storage": lvm.LVSize.String(),
								},
							},
							"storageClassName": osdStorageClassName,
							"volumeMode":       "Block",
							"accessModes":      []interface{}{"ReadWriteOnce"},
						},
					},
				},
			},
		}
	}
	return storage
}

// isDevicePathRegex returns true if the user-supplied DeviceFilter looks
// like a regex over absolute device paths (anything containing "/dev/"
// or starting with "/") and therefore must be passed to Rook as
// devicePathFilter rather than deviceFilter.
func isDevicePathRegex(filter string) bool {
	// Trim the standard regex anchor so "^/dev/sdc$" is recognised as a
	// path filter even when authored with a leading "^".
	probe := strings.TrimPrefix(filter, "^")
	return strings.HasPrefix(probe, "/") || strings.Contains(filter, "/dev/")
}

func defaultIfEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func daemonOrDefault(n, def int32) int32 {
	if n == 0 {
		return def
	}
	return n
}

// CephBlockPool builds an unstructured CephBlockPool CR.
func CephBlockPool(cluster *v1alpha1.SdsElasticCluster, namespace string, pool *v1alpha1.BlockPoolSpec) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"failureDomain": defaultIfEmpty(pool.FailureDomain, "host"),
	}
	if pool.Replicated != nil {
		repl := map[string]interface{}{
			"size": int64(pool.Replicated.Size),
		}
		if pool.Replicated.RequireSafeReplicaSize != nil {
			repl["requireSafeReplicaSize"] = *pool.Replicated.RequireSafeReplicaSize
		}
		spec["replicated"] = repl
	}
	if pool.ErasureCoded != nil {
		spec["erasureCoded"] = map[string]interface{}{
			"dataChunks":   int64(pool.ErasureCoded.DataChunks),
			"codingChunks": int64(pool.ErasureCoded.CodingChunks),
		}
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephBlockPoolGVK)
	obj.SetName(pool.Name)
	obj.SetNamespace(namespace)
	obj.SetLabels(ManagedLabels(cluster.Name))
	obj.Object["spec"] = spec
	return obj
}

func buildPoolConfig(pc *v1alpha1.PoolConfig) map[string]interface{} {
	m := map[string]interface{}{
		"failureDomain": defaultIfEmpty(pc.FailureDomain, "host"),
	}
	if pc.Replicated != nil {
		repl := map[string]interface{}{
			"size": int64(pc.Replicated.Size),
		}
		if pc.Replicated.RequireSafeReplicaSize != nil {
			repl["requireSafeReplicaSize"] = *pc.Replicated.RequireSafeReplicaSize
		}
		m["replicated"] = repl
	}
	if pc.ErasureCoded != nil {
		m["erasureCoded"] = map[string]interface{}{
			"dataChunks":   int64(pc.ErasureCoded.DataChunks),
			"codingChunks": int64(pc.ErasureCoded.CodingChunks),
		}
	}
	return m
}

func buildNamedPoolConfig(np *v1alpha1.NamedPoolConfig) map[string]interface{} {
	m := buildPoolConfig(&np.PoolConfig)
	m["name"] = np.Name
	return m
}

// CephFilesystem builds an unstructured CephFilesystem CR.
func CephFilesystem(cluster *v1alpha1.SdsElasticCluster, namespace string, fs *v1alpha1.FilesystemSpec) *unstructured.Unstructured {
	dataPools := make([]interface{}, 0, len(fs.DataPools))
	for i := range fs.DataPools {
		dataPools = append(dataPools, buildNamedPoolConfig(&fs.DataPools[i]))
	}

	ms := map[string]interface{}{
		"activeCount":   int64(1),
		"activeStandby": true,
	}
	if fs.MetadataServer != nil {
		if fs.MetadataServer.ActiveCount > 0 {
			ms["activeCount"] = int64(fs.MetadataServer.ActiveCount)
		}
		ms["activeStandby"] = fs.MetadataServer.ActiveStandby
	}

	spec := map[string]interface{}{
		"metadataPool":               buildPoolConfig(&fs.MetadataPool),
		"dataPools":                  dataPools,
		"preserveFilesystemOnDelete": fs.PreserveFilesystemOnDelete,
		"metadataServer":             ms,
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephFilesystemGVK)
	obj.SetName(fs.Name)
	obj.SetNamespace(namespace)
	obj.SetLabels(ManagedLabels(cluster.Name))
	obj.Object["spec"] = spec
	return obj
}

// CephObjectStore builds an unstructured CephObjectStore CR.
func CephObjectStore(cluster *v1alpha1.SdsElasticCluster, namespace string, os *v1alpha1.ObjectStoreSpec) *unstructured.Unstructured {
	gateway := map[string]interface{}{
		"port":      int64(80),
		"instances": int64(1),
	}
	if os.Gateway.Port > 0 {
		gateway["port"] = int64(os.Gateway.Port)
	}
	if os.Gateway.SecurePort > 0 {
		gateway["securePort"] = int64(os.Gateway.SecurePort)
	}
	if os.Gateway.Instances > 0 {
		gateway["instances"] = int64(os.Gateway.Instances)
	}

	spec := map[string]interface{}{
		"metadataPool":          buildPoolConfig(&os.MetadataPool),
		"dataPool":              buildPoolConfig(&os.DataPool),
		"preservePoolsOnDelete": os.PreservePoolsOnDelete,
		"gateway":               gateway,
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephObjectStoreGVK)
	obj.SetName(os.Name)
	obj.SetNamespace(namespace)
	obj.SetLabels(ManagedLabels(cluster.Name))
	obj.Object["spec"] = spec
	return obj
}
