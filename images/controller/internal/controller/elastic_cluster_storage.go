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
	"context"
	"fmt"
	"reflect"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// ensureStorage reconciles the LVM-backed storage chain BlockDevice → LVG →
// LLV → local PV for every BlockDevice that matches
// ec.spec.storage.{nodeSelector,blockDeviceSelector}.
//
// Returns:
//   - done       — true once every selected BD has a matching LVG/LLV/PV
//     and at least one PV is ready to be consumed.
//   - osdCount   — the total number of OSDs (== matched BlockDevices). Passed
//     to the CephCluster builder so storageClassDeviceSets[0]
//     asks for exactly the right number of PVCs.
//   - msg        — human-readable status surfaced on the StorageReady cond.
//
// Adoption: every selected BD gets the ECClusterLabel=<ec.Name> label
// patched in as the first per-BD step. sds-node-configurator flips
// status.consumable=false the moment it puts a VG on top of the device,
// so a label-free filter (consumable-only) would drop the BD on the next
// reconcile and trigger a destructive osdCount shrink. The label keeps
// adopted BDs included in `listMatchingBlockDevices` regardless of the
// downstream consumable flag.
func (r *ElasticClusterReconciler) ensureStorage(ctx context.Context, ec *v1alpha1.ElasticCluster) (bool, int32, string, error) {
	matchedBDs, err := r.listMatchingBlockDevices(ctx, ec)
	if err != nil {
		if isNoMatchErr(err) {
			return false, 0, "waiting for BlockDevice CRD (sds-node-configurator)", nil
		}
		return false, 0, "", fmt.Errorf("list BlockDevices: %w", err)
	}
	if len(matchedBDs) == 0 {
		return false, 0, "no BlockDevices match storage.{nodeSelector,blockDeviceSelector}", nil
	}

	selected := int32(0)
	skipped := []string{}
	for i := range matchedBDs {
		bd := matchedBDs[i]
		bdName := bd.GetName()
		nodeName, _, _ := unstructured.NestedString(bd.Object, "status", "nodeName")
		if nodeName == "" {
			skipped = append(skipped, fmt.Sprintf("%s(empty nodeName)", bdName))
			continue
		}
		sizeStr, _, _ := unstructured.NestedString(bd.Object, "status", "size")
		capacity, capErr := resource.ParseQuantity(sizeStr)
		if capErr != nil {
			skipped = append(skipped, fmt.Sprintf("%s(bad size %q)", bdName, sizeStr))
			continue
		}

		if err := r.adoptBlockDevice(ctx, &bd, ec); err != nil {
			return false, 0, "", fmt.Errorf("adopt BlockDevice %q: %w", bdName, err)
		}

		lvg := builder.ECLVMVolumeGroup(ec, bdName, nodeName)
		if err := r.upsertECUnstructured(ctx, lvg); err != nil {
			if isNoMatchErr(err) {
				return false, 0, "waiting for LVMVolumeGroup CRD (sds-node-configurator)", nil
			}
			return false, 0, "", fmt.Errorf("upsert LVMVolumeGroup %s: %w", lvg.GetName(), err)
		}

		llv := builder.ECLVMLogicalVolume(ec, bdName)
		if err := r.upsertECUnstructured(ctx, llv); err != nil {
			if isNoMatchErr(err) {
				return false, 0, "waiting for LVMLogicalVolume CRD (sds-node-configurator)", nil
			}
			return false, 0, "", fmt.Errorf("upsert LVMLogicalVolume %s: %w", llv.GetName(), err)
		}

		pv := builder.ECOSDPersistentVolume(ec, bdName, nodeName, capacity)
		if err := r.upsertECPersistentVolume(ctx, pv); err != nil {
			return false, 0, "", fmt.Errorf("upsert PersistentVolume %s: %w", pv.Name, err)
		}
		selected++
	}

	if len(skipped) > 0 {
		return false, selected,
			fmt.Sprintf("selected %d BlockDevices for LVG/LLV/PV provisioning; skipped %d unusable: %v", selected, len(skipped), skipped),
			nil
	}
	return true, selected,
		fmt.Sprintf("selected %d BlockDevices for LVG/LLV/PV provisioning", selected), nil
}

// adoptBlockDevice patches the BD with the ECClusterLabel pointing at the
// owning ElasticCluster. The label is the only durable signal the storage
// stage uses to keep selecting the BD after sds-node-configurator flips
// status.consumable=false (which it does as soon as a VG appears on the
// device). The patch is idempotent — already-labelled BDs short-circuit.
func (r *ElasticClusterReconciler) adoptBlockDevice(ctx context.Context, bd *unstructured.Unstructured, ec *v1alpha1.ElasticCluster) error {
	if cur, ok := bd.GetLabels()[external.ECClusterLabel]; ok && cur == ec.Name {
		return nil
	}
	patch := client.MergeFrom(bd.DeepCopy())
	labels := bd.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[external.ECClusterLabel] = ec.Name
	bd.SetLabels(labels)
	return r.Client.Patch(ctx, bd, patch)
}

// listMatchingBlockDevices returns BlockDevices that:
//  1. match ec.spec.storage.blockDeviceSelector via labels;
//  2. live on a node matched by ec.spec.storage.nodeSelector;
//  3. are consumable (status.consumable == true).
func (r *ElasticClusterReconciler) listMatchingBlockDevices(ctx context.Context, ec *v1alpha1.ElasticCluster) ([]unstructured.Unstructured, error) {
	bdSelector, err := metav1.LabelSelectorAsSelector(ec.Spec.Storage.BlockDeviceSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid blockDeviceSelector: %w", err)
	}
	nodeSelector, err := metav1.LabelSelectorAsSelector(ec.Spec.Storage.NodeSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid nodeSelector: %w", err)
	}

	matchingNodes, err := r.matchingNodeNames(ctx, nodeSelector)
	if err != nil {
		return nil, fmt.Errorf("list matching Nodes: %w", err)
	}

	bdList := &unstructured.UnstructuredList{}
	bdList.SetGroupVersionKind(schemaListKind(external.BlockDeviceGVK))
	if err := r.Client.List(ctx, bdList, &client.ListOptions{LabelSelector: bdSelector}); err != nil {
		return nil, err
	}

	out := make([]unstructured.Unstructured, 0, len(bdList.Items))
	for _, bd := range bdList.Items {
		nodeName, _, _ := unstructured.NestedString(bd.Object, "status", "nodeName")
		consumable, _, _ := unstructured.NestedBool(bd.Object, "status", "consumable")
		_, alreadyOurs := bd.GetLabels()[external.ECClusterLabel]
		if !alreadyOurs && !consumable {
			continue
		}
		if !matchingNodes[nodeName] {
			continue
		}
		out = append(out, bd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out, nil
}

// matchingNodeNames returns the set of node names that match `sel`. An
// empty selector matches every node.
func (r *ElasticClusterReconciler) matchingNodeNames(ctx context.Context, sel labels.Selector) (map[string]bool, error) {
	nodes := &corev1.NodeList{}
	opts := []client.ListOption{}
	if sel != nil && !sel.Empty() {
		opts = append(opts, client.MatchingLabelsSelector{Selector: sel})
	}
	if err := r.Client.List(ctx, nodes, opts...); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(nodes.Items))
	for _, n := range nodes.Items {
		out[n.Name] = true
	}
	return out, nil
}

// upsertECPersistentVolume creates or patches a PV labelled with
// ECClusterLabel=<ec.Name>. Only labels are reconciled; PV is otherwise
// immutable after Bound. NodeAffinity drift on host rename is tracked in
// backlog item B-N1 alongside the rest of the OwnerReferences/finalizers
// work — a host rename is rare and out of MVP scope.
func (r *ElasticClusterReconciler) upsertECPersistentVolume(ctx context.Context, desired *corev1.PersistentVolume) error {
	existing := &corev1.PersistentVolume{}
	err := r.Client.Get(ctx, types.NamespacedName{Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	merged := mergeLabels(existing.Labels, desired.Labels)
	if reflect.DeepEqual(existing.Labels, merged) {
		return nil
	}
	existing.Labels = merged
	return r.Client.Update(ctx, existing)
}
