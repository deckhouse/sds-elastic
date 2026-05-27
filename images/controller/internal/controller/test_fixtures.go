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

package controller

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

const (
	testECName      = "demo"
	testNamespace   = "d8-sds-elastic"
	testStorageRole = "storage"
	testBDLabel     = "elastic-osd"
)

func newTestElasticCluster() *v1alpha1.ElasticCluster {
	return &v1alpha1.ElasticCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testECName},
		Spec: v1alpha1.ElasticClusterSpec{
			Storage: v1alpha1.ElasticClusterStorageSpec{
				NodeSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"node-role": testStorageRole},
				},
				BlockDeviceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"role": testBDLabel},
				},
			},
		},
	}
}

func newTestNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"node-role": testStorageRole},
		},
	}
}

func newBlockDevice(name, nodeName, size string, consumable bool, extraLabels map[string]string) *unstructured.Unstructured {
	labels := map[string]string{"role": testBDLabel}
	for k, v := range extraLabels {
		labels[k] = v
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.BlockDeviceGVK)
	obj.SetName(name)
	obj.SetLabels(labels)
	obj.Object["status"] = map[string]interface{}{
		"nodeName":   nodeName,
		"size":       size,
		"consumable": consumable,
	}
	return obj
}

// newRookMonSecret produces a rook-ceph-mon Secret carrying both the
// modern "admin-secret" key and the legacy "ceph-secret" key with the
// same admin key, mirroring how recent Rook releases populate both. Use
// newRookMonSecretLegacy / newRookMonSecretAdminOnly when a test wants
// to exercise a single key being present.
func newRookMonSecret(fsid, adminSecret, monSecret string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      external.RookCephMonSecretName,
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			external.RookCephMonSecretFSIDKey:        []byte(fsid),
			external.RookCephMonSecretAdminSecretKey: []byte(adminSecret),
			external.RookCephMonSecretCephSecretKey:  []byte(adminSecret),
			external.RookCephMonSecretMonSecretKey:   []byte(monSecret),
		},
	}
}

// newRookMonSecretLegacy produces the rook-ceph-mon Secret as written by
// older Rook releases (and the Deckhouse-vendored Rook on this branch):
// admin key under "ceph-secret" only, no "admin-secret" key. The ECC
// reconciler's fallback path must accept this layout.
func newRookMonSecretLegacy(fsid, cephSecret, monSecret string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      external.RookCephMonSecretName,
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			external.RookCephMonSecretFSIDKey:       []byte(fsid),
			external.RookCephMonSecretCephSecretKey: []byte(cephSecret),
			external.RookCephMonSecretMonSecretKey:  []byte(monSecret),
		},
	}
}

// newRookMonSecretAdminOnly produces the rook-ceph-mon Secret with only
// the modern "admin-secret" key set (no legacy "ceph-secret"), exercising
// the preferred branch of the ECC fallback chain.
func newRookMonSecretAdminOnly(fsid, adminSecret, monSecret string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      external.RookCephMonSecretName,
			Namespace: testNamespace,
		},
		Data: map[string][]byte{
			external.RookCephMonSecretFSIDKey:        []byte(fsid),
			external.RookCephMonSecretAdminSecretKey: []byte(adminSecret),
			external.RookCephMonSecretMonSecretKey:   []byte(monSecret),
		},
	}
}

func newRookMonEndpointsCM(data, maxMonID string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      external.RookCephMonEndpointsConfigMap,
			Namespace: testNamespace,
		},
		Data: map[string]string{
			external.RookCephMonEndpointsDataKey:     data,
			external.RookCephMonEndpointsMaxMonIDKey: maxMonID,
		},
	}
}

func newCephClusterUnstructured(ec *v1alpha1.ElasticCluster, phase, runningVersion, cephImage string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephClusterGVK)
	obj.SetName(builder.ECCephClusterName(ec))
	obj.SetNamespace(testNamespace)
	obj.Object["spec"] = map[string]interface{}{
		"cephVersion": map[string]interface{}{
			"image": cephImage,
		},
	}
	status := map[string]interface{}{
		"phase": phase,
		"version": map[string]interface{}{
			"version": runningVersion,
		},
		"ceph": map[string]interface{}{
			"health": "HEALTH_OK",
		},
	}
	obj.Object["status"] = status
	return obj
}

// withCephClusterCephStatus mutates a CephCluster fixture's
// status.ceph subtree (health, message, lastChecked, capacity, details,
// versions) in-place. Each block is opt-in: the capacity block is filled
// only when bytesTotal > 0, the versions block only when versionsByKind
// is non-empty, etc. The function returns no value — every caller passes
// the original `cc` to the fake client and never reads the return.
//
// versionsByKind maps kind ("osd"/"mon"/"mgr") to the version-string →
// daemon-count map Rook publishes under
// `CephCluster.status.ceph.versions.<kind>`.
func withCephClusterCephStatus(
	cc *unstructured.Unstructured,
	health, message, lastChecked string,
	bytesTotal, bytesUsed, bytesAvailable int64,
	lastUpdated string,
	checks map[string]map[string]string,
	versionsByKind map[string]map[string]int32,
) {
	status, _ := cc.Object["status"].(map[string]interface{})
	if status == nil {
		status = map[string]interface{}{}
		cc.Object["status"] = status
	}
	cephObj, _ := status["ceph"].(map[string]interface{})
	if cephObj == nil {
		cephObj = map[string]interface{}{}
		status["ceph"] = cephObj
	}
	if health != "" {
		cephObj["health"] = health
	}
	if message != "" {
		cephObj["message"] = message
	}
	if lastChecked != "" {
		cephObj["lastChecked"] = lastChecked
	}
	if bytesTotal > 0 || bytesUsed > 0 || bytesAvailable > 0 {
		capObj := map[string]interface{}{
			"bytesTotal":     bytesTotal,
			"bytesUsed":      bytesUsed,
			"bytesAvailable": bytesAvailable,
		}
		if lastUpdated != "" {
			capObj["lastUpdated"] = lastUpdated
		}
		cephObj["capacity"] = capObj
	}
	if len(checks) > 0 {
		details := map[string]interface{}{}
		for name, props := range checks {
			detail := map[string]interface{}{}
			for k, v := range props {
				detail[k] = v
			}
			details[name] = detail
		}
		cephObj["details"] = details
	}
	if len(versionsByKind) > 0 {
		versions := map[string]interface{}{}
		for kind, hist := range versionsByKind {
			if len(hist) == 0 {
				continue
			}
			kindMap := map[string]interface{}{}
			for ver, count := range hist {
				// Mimic Rook's wire format: Rook publishes counts as
				// JSON numbers, which round-trip to float64 through
				// unstructured.Unstructured. Use float64 here so the
				// fixture matches the cache contents in production.
				kindMap[ver] = float64(count)
			}
			versions[kind] = kindMap
		}
		if len(versions) > 0 {
			cephObj["versions"] = versions
		}
	}
}

// newCephClusterConnectionUnstructured builds a CephClusterConnection
// fixture in the "Created" phase, named after the shared `testECName`
// EC. Every test uses this 1:1 mapping; if a future test needs a
// different phase or name, take a parameter back at that point.
func newCephClusterConnectionUnstructured() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephClusterConnectionGVK)
	obj.SetName(testECName)
	obj.Object["status"] = map[string]interface{}{
		"phase": "Created",
	}
	return obj
}

func newCephBlockPoolUnstructured(name, namespace, phase string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephBlockPoolGVK)
	obj.SetName(name)
	obj.SetNamespace(namespace)
	obj.Object["status"] = map[string]interface{}{
		"phase": phase,
	}
	return obj
}

func newCephStorageClassUnstructured(name, phase string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephStorageClassGVK)
	obj.SetName(name)
	obj.Object["status"] = map[string]interface{}{
		"phase": phase,
	}
	return obj
}

func ecWithCephClusterReady(ec *v1alpha1.ElasticCluster) *v1alpha1.ElasticCluster {
	out := ec.DeepCopy()
	if out.Status == nil {
		out.Status = &v1alpha1.ElasticClusterStatus{}
	}
	out.Status.Conditions = append(out.Status.Conditions, metav1.Condition{
		Type:   v1alpha1.ECConditionCephClusterReady,
		Status: metav1.ConditionTrue,
	})
	return out
}

// newTestElasticStorageClass builds an ESC fixture for the shared
// testECName ElasticCluster. The clusterRef parameter is implicit in the
// test suite because all reconciler tests target the single test EC.
func newTestElasticStorageClass(name string, scType v1alpha1.StorageClassType) *v1alpha1.ElasticStorageClass {
	return &v1alpha1.ElasticStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ElasticStorageClassSpec{
			ClusterRef:  testECName,
			Type:        scType,
			Replication: v1alpha1.ReplicationConsistencyAndAvailability,
		},
	}
}
