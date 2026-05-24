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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// OSDResourceShortHash hashes "<ec-name>:<bd-name>" into 8 lowercase hex
// characters. Used to derive deterministic per-BlockDevice resource names
// without exceeding Kubernetes 63-char label/name limits.
func OSDResourceShortHash(ecName, bdName string) string {
	sum := sha256.Sum256([]byte(ecName + ":" + bdName))
	return hex.EncodeToString(sum[:])[:8]
}

// ECOSDResourceName returns the deterministic name shared by the
// LVMVolumeGroup, LVMLogicalVolume, and local PersistentVolume that back
// a single OSD on the BlockDevice identified by `bdName`.
//
// Pattern: "sds-elastic-<ec>-osd-<sha8>". The 8-char hash keeps the name
// stable even when ec-name + bd-name approach the 63-char DNS-1123 ceiling.
func ECOSDResourceName(ec *v1alpha1.ElasticCluster, bdName string) string {
	return fmt.Sprintf("sds-elastic-%s-osd-%s", ec.Name, OSDResourceShortHash(ec.Name, bdName))
}

// ECManagedLabels is the common label set placed on every resource owned by
// an ElasticCluster CR (LVG/LLV/PV and downstream Rook CRs).
func ECManagedLabels(ec *v1alpha1.ElasticCluster) map[string]string {
	return map[string]string{
		external.ManagedByLabelKey: external.ManagedByLabelValue,
		external.ECClusterLabel:    ec.Name,
	}
}

// ECLVMVolumeGroup produces an unstructured LVMVolumeGroup CR
// (sds-node-configurator) that wraps a single BlockDevice. One LVG per
// BlockDevice — LVMVolumeGroupSet is intentionally not used (B14 backlog
// follow-up: stabilise the BlockDevice consumable label).
//
// Spec layout follows crds/lvmvolumegroup.yaml from sds-node-configurator:
//   - type=Local
//   - local.nodeName=<node hosting the BlockDevice>
//   - blockDeviceSelector matches the single BD by metadata.name.
//   - actualVGNameOnTheNode follows the OSD resource name.
func ECLVMVolumeGroup(ec *v1alpha1.ElasticCluster, bdName, nodeName string) *unstructured.Unstructured {
	name := ECOSDResourceName(ec, bdName)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.LVMVolumeGroupGVK)
	obj.SetName(name)
	obj.SetLabels(ECManagedLabels(ec))
	obj.Object["spec"] = map[string]interface{}{
		"type": "Local",
		"local": map[string]interface{}{
			"nodeName": nodeName,
		},
		"actualVGNameOnTheNode": name,
		"blockDeviceSelector": map[string]interface{}{
			"matchExpressions": []interface{}{
				map[string]interface{}{
					"key":      "kubernetes.io/metadata.name",
					"operator": "In",
					"values":   []interface{}{bdName},
				},
			},
		},
	}
	return obj
}

// ECLVMLogicalVolume produces an unstructured LVMLogicalVolume CR that
// occupies 100% of the LVG. Type=Thick (no thin pool) because the
// volumeMode=Block PV that Rook consumes does not need overprovisioning.
func ECLVMLogicalVolume(ec *v1alpha1.ElasticCluster, bdName string) *unstructured.Unstructured {
	name := ECOSDResourceName(ec, bdName)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.LVMLogicalVolumeGVK)
	obj.SetName(name)
	obj.SetLabels(ECManagedLabels(ec))
	obj.Object["spec"] = map[string]interface{}{
		"actualLVNameOnTheNode": name,
		"lvmVolumeGroupName":    name,
		"size":                  "100%FREE",
		"type":                  "Thick",
	}
	return obj
}

// ECOSDPersistentVolume builds the local-path PV that hides the LLV behind a
// Block-mode PV. Capacity is supplied by the controller (read from the
// resolved LLV size in bytes) — the BlockDevice + LVG layout determines the
// real space, the PV value is purely informational for the scheduler.
func ECOSDPersistentVolume(ec *v1alpha1.ElasticCluster, bdName, nodeName string, capacityBytes int64) *corev1.PersistentVolume {
	name := ECOSDResourceName(ec, bdName)
	volumeMode := corev1.PersistentVolumeBlock
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: ECManagedLabels(ec),
		},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: external.ReservedOSDStorageClassName,
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: *resource.NewQuantity(capacityBytes, resource.BinarySI),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			VolumeMode:                    &volumeMode,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				Local: &corev1.LocalVolumeSource{
					Path: fmt.Sprintf("/dev/%s/%s", name, name),
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
									Values:   []string{nodeName},
								},
							},
						},
					},
				},
			},
		},
	}
}
