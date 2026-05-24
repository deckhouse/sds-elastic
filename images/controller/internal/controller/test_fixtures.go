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

func newRookMonSecret(fsid, adminSecret, monSecret string) *corev1.Secret {
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

func newCephClusterConnectionUnstructured(name, phase string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephClusterConnectionGVK)
	obj.SetName(name)
	obj.Object["status"] = map[string]interface{}{
		"phase": phase,
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

func newTestElasticStorageClass(name, clusterRef string, scType v1alpha1.StorageClassType) *v1alpha1.ElasticStorageClass {
	return &v1alpha1.ElasticStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ElasticStorageClassSpec{
			ClusterRef:  clusterRef,
			Type:        scType,
			Replication: v1alpha1.ReplicationConsistencyAndAvailability,
		},
	}
}
