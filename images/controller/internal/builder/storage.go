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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// LLVName returns the deterministic name of the i-th LVMLogicalVolume.
func LLVName(_ *v1alpha1.SdsElasticCluster, i int32) string {
	return fmt.Sprintf("ceph-osd-%d", i)
}

// OSDPVName returns the deterministic name of the i-th PV backed by the LLV.
func OSDPVName(_ *v1alpha1.SdsElasticCluster, i int32) string {
	return fmt.Sprintf("ceph-osd-%d", i)
}

// NodeName returns the hostname for the i-th node, following
// instruction.md: "<NodeNamePrefix>-<i>".
func NodeName(lvm *v1alpha1.LVMStorageSpec, i int32) string {
	return fmt.Sprintf("%s-%d", lvm.NodeNamePrefix, i)
}

// LVGName returns the LVMVolumeGroup name for the i-th node.
func LVGName(lvm *v1alpha1.LVMStorageSpec, i int32) string {
	return fmt.Sprintf("%s-%d", lvm.LVGNamePrefix, i)
}

// LVMLogicalVolume produces an unstructured LLV CR (sds-node-configurator),
// equivalent to the YAML from instruction.md "Создать LLV для OSD".
func LVMLogicalVolume(cluster *v1alpha1.SdsElasticCluster, i int32) *unstructured.Unstructured {
	lvm := cluster.Spec.Storage.LVM
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.LVMLogicalVolumeGVK)
	obj.SetName(LLVName(cluster, i))
	obj.SetLabels(ManagedLabels(cluster.Name))
	obj.SetFinalizers([]string{external.LVMLogicalVolumeManualFinalizer})
	obj.Object["spec"] = map[string]interface{}{
		"actualLVNameOnTheNode": lvm.ActualLVName,
		"lvmVolumeGroupName":    LVGName(lvm, i),
		"size":                  lvm.LVSize.String(),
		"type":                  "Thick",
	}
	return obj
}

// OSDPersistentVolume builds the local-path PV described in instruction.md
// "Временное решение с ручным созданием PV из LLV".
func OSDPersistentVolume(cluster *v1alpha1.SdsElasticCluster, i int32, storageClassName string) *corev1.PersistentVolume {
	lvm := cluster.Spec.Storage.LVM
	volumeMode := corev1.PersistentVolumeBlock
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   OSDPVName(cluster, i),
			Labels: ManagedLabels(cluster.Name),
		},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: storageClassName,
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: lvm.LVSize,
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			VolumeMode:                    &volumeMode,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				Local: &corev1.LocalVolumeSource{
					Path: fmt.Sprintf("/dev/%s/%s", lvm.ActualVGName, lvm.ActualLVName),
				},
			},
			NodeAffinity: &corev1.VolumeNodeAffinity{
				Required: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      corev1.LabelHostname,
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{NodeName(lvm, i)},
								},
							},
						},
					},
				},
			},
		},
	}
	return pv
}

// LVSizeQuantity validates the configured size and returns it as a Quantity.
// (Helper kept here so business code does not import apimachinery resource pkg.)
func LVSizeQuantity(lvm *v1alpha1.LVMStorageSpec) resource.Quantity {
	return lvm.LVSize
}
