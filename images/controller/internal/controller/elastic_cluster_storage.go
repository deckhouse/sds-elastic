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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// Status.phase target values for the LVG/LLV chain managed by
// sds-node-configurator. Hard-coded on purpose: importing the SNC api
// just for two strings would pull a sister-module dependency before B19
// (typed clients) lands. The values come from
// sds-node-configurator/api/v1alpha1/const.go.
const (
	lvgPhaseReady   = "Ready"
	llvPhaseCreated = "Created"
)

// Well-known reasons published on ECConditionStorageReady when the FSM
// is gated waiting for a downstream resource to converge. Stable strings
// — UI/dashboards dispatch on them.
const (
	storageReasonNoBlockDevices         = "NoBlockDevices"
	storageReasonWaitingForBlockDevices = "WaitingForBlockDevices"
	storageReasonWaitingForBDCRD        = "WaitingForBlockDeviceCRD"
	storageReasonWaitingForLVGCRD       = "WaitingForLVMVolumeGroupCRD"
	storageReasonWaitingForLLVCRD       = "WaitingForLVMLogicalVolumeCRD"
	storageReasonWaitingForLVG          = "WaitingForLVMVolumeGroup"
	storageReasonWaitingForLLV          = "WaitingForLVMLogicalVolume"
	storageReasonWaitingForPV           = "WaitingForPersistentVolume"
)

// ensureStorage reconciles the LVM-backed storage chain BlockDevice → LVG →
// LLV → local PV for every BlockDevice that matches
// ec.spec.storage.{nodeSelector,blockDeviceSelector}.
//
// Returns:
//   - done       — true once every selected BD has a matching LVG/LLV/PV
//     AND the underlying CRs report a target status.phase: LVG=Ready,
//     LLV=Created, PV in {Available, Bound}. Without these checks the
//     reconciler raced ahead of sds-node-configurator's host-side
//     provisioning, leaving kubelet to FailedMapVolume on rook-ceph-osd-
//     prepare pods because /dev/<vg>/<lv> did not yet exist.
//   - osdCount   — the total number of OSDs (== matched BlockDevices). Passed
//     to the CephCluster builder so storageClassDeviceSets[0]
//     asks for exactly the right number of PVCs.
//   - pvcRequest — min(BD.size) across all selected BDs. Equal to the
//     capacity of the smallest local-PV produced by this stage and used
//     by ensureCephCluster to populate volumeClaimTemplates[0].spec.
//     resources.requests.storage. The K8s PV binder requires
//     PV.capacity >= PVC.requests.storage, so taking the minimum of the
//     PV capacities guarantees every set1-data-* PVC can bind to one of
//     the local-PVs. Zero-value Quantity when no BD was selected.
//   - reason     — well-known machine-readable cause for !done (or "" on
//     done). Mirrored verbatim into the StorageReady condition's Reason.
//   - msg        — human-readable status surfaced on the StorageReady cond.
//
// Adoption: every selected BD gets the ECClusterLabel=<ec.Name> label
// patched in as the first per-BD step. sds-node-configurator flips
// status.consumable=false the moment it puts a VG on top of the device,
// so a label-free filter (consumable-only) would drop the BD on the next
// reconcile and trigger a destructive osdCount shrink. The label keeps
// adopted BDs included in `listMatchingBlockDevices` regardless of the
// downstream consumable flag.
func (r *ElasticClusterReconciler) ensureStorage(
	ctx context.Context,
	ec *v1alpha1.ElasticCluster,
) (done bool, osdCount int32, pvcRequest resource.Quantity, reason, msg string, err error) {
	matchedBDs, listErr := r.listMatchingBlockDevices(ctx, ec)
	if listErr != nil {
		if isNoMatchErr(listErr) {
			return false, 0, resource.Quantity{}, storageReasonWaitingForBDCRD,
				"waiting for BlockDevice CRD (sds-node-configurator)", nil
		}
		return false, 0, resource.Quantity{}, "", "", fmt.Errorf("list BlockDevices: %w", listErr)
	}
	if len(matchedBDs) == 0 {
		return false, 0, resource.Quantity{}, storageReasonNoBlockDevices,
			"no BlockDevices match storage.{nodeSelector,blockDeviceSelector}", nil
	}

	var (
		selected     int32
		skipped      []string
		minSize      resource.Quantity
		managedNames []string
	)
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
			return false, 0, resource.Quantity{}, "", "", fmt.Errorf("adopt BlockDevice %q: %w", bdName, err)
		}

		lvg := builder.ECLVMVolumeGroup(ec, bdName, nodeName)
		if err := r.upsertECUnstructured(ctx, lvg); err != nil {
			if isNoMatchErr(err) {
				return false, 0, resource.Quantity{}, storageReasonWaitingForLVGCRD,
					"waiting for LVMVolumeGroup CRD (sds-node-configurator)", nil
			}
			return false, 0, resource.Quantity{}, "", "", fmt.Errorf("upsert LVMVolumeGroup %s: %w", lvg.GetName(), err)
		}

		llv := builder.ECLVMLogicalVolume(ec, bdName)
		if err := r.upsertECUnstructured(ctx, llv); err != nil {
			if isNoMatchErr(err) {
				return false, 0, resource.Quantity{}, storageReasonWaitingForLLVCRD,
					"waiting for LVMLogicalVolume CRD (sds-node-configurator)", nil
			}
			return false, 0, resource.Quantity{}, "", "", fmt.Errorf("upsert LVMLogicalVolume %s: %w", llv.GetName(), err)
		}

		pv := builder.ECOSDPersistentVolume(ec, bdName, nodeName, capacity)
		if err := r.upsertECPersistentVolume(ctx, pv); err != nil {
			return false, 0, resource.Quantity{}, "", "", fmt.Errorf("upsert PersistentVolume %s: %w", pv.Name, err)
		}
		if selected == 0 || capacity.Cmp(minSize) < 0 {
			minSize = capacity
		}
		selected++
		managedNames = append(managedNames, builder.ECOSDResourceName(ec, bdName))
	}

	if len(skipped) > 0 {
		return false, selected, minSize, storageReasonWaitingForBlockDevices,
			fmt.Sprintf("selected %d BlockDevices for LVG/LLV/PV provisioning; skipped %d unusable: %v",
				selected, len(skipped), skipped),
			nil
	}
	if selected == 0 {
		// listMatchingBlockDevices returned items but every one failed
		// validation above. Treat as transient: surface "no usable BDs"
		// rather than declaring storage Ready on an empty set.
		return false, 0, resource.Quantity{}, storageReasonNoBlockDevices,
			"no usable BlockDevices after validation", nil
	}

	sort.Strings(managedNames)

	// Wait for sds-node-configurator to actually flip phase=Ready on the
	// LVG before declaring storage Ready: otherwise the LVG is just a CR
	// and there is no /dev/<vg> on the host yet.
	lvgPhases, err := r.fetchUnstructuredPhasesByLabel(ctx, ec, external.LVMVolumeGroupGVK)
	if err != nil {
		return false, selected, minSize, "", "", fmt.Errorf("list LVMVolumeGroups for status: %w", err)
	}
	if ready, pending := assessPhases(managedNames, lvgPhases, lvgPhaseReady); ready < int(selected) {
		return false, selected, minSize, storageReasonWaitingForLVG,
			fmt.Sprintf("%d/%d LVMVolumeGroups Ready; pending: %s",
				ready, selected, formatPending(pending)),
			nil
	}

	// Wait for LLV to reach phase=Created (sds-node-configurator has
	// allocated the LV inside the VG). Until then /dev/<vg>/<lv> does
	// not exist and kubelet's FailedMapVolume retry loop is the only
	// signal Rook gets.
	llvPhases, err := r.fetchUnstructuredPhasesByLabel(ctx, ec, external.LVMLogicalVolumeGVK)
	if err != nil {
		return false, selected, minSize, "", "", fmt.Errorf("list LVMLogicalVolumes for status: %w", err)
	}
	if ready, pending := assessPhases(managedNames, llvPhases, llvPhaseCreated); ready < int(selected) {
		return false, selected, minSize, storageReasonWaitingForLLV,
			fmt.Sprintf("%d/%d LVMLogicalVolumes Created; pending: %s",
				ready, selected, formatPending(pending)),
			nil
	}

	// Wait for the local PV to reach a phase that the K8s scheduler can
	// bind to (Available or Bound). A freshly-Create()d PV starts in the
	// empty / Pending phase; the volume binder transitions it once the
	// LLV-backed device is observable.
	pvPhases, err := r.fetchPVPhasesByLabel(ctx, ec)
	if err != nil {
		return false, selected, minSize, "", "", fmt.Errorf("list PersistentVolumes for status: %w", err)
	}
	if ready, pending := assessPhases(managedNames, pvPhases,
		string(corev1.VolumeAvailable), string(corev1.VolumeBound)); ready < int(selected) {
		return false, selected, minSize, storageReasonWaitingForPV,
			fmt.Sprintf("%d/%d PersistentVolumes Available|Bound; pending: %s",
				ready, selected, formatPending(pending)),
			nil
	}

	return true, selected, minSize, "",
		fmt.Sprintf("%d OSD volumes ready (LVG/LLV/PV)", selected), nil
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
// backlog item B20 alongside the rest of the OwnerReferences/finalizers
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

// fetchUnstructuredPhasesByLabel returns name → status.phase for every
// CR of the given GVK labelled with ECClusterLabel=<ec.Name>. Missing
// CRs are reported via the absence of the key in the map (callers
// observe NotFound).
func (r *ElasticClusterReconciler) fetchUnstructuredPhasesByLabel(
	ctx context.Context, ec *v1alpha1.ElasticCluster, gvk schema.GroupVersionKind,
) (map[string]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schemaListKind(gvk))
	if err := r.Client.List(ctx, list, client.MatchingLabels{external.ECClusterLabel: ec.Name}); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list.Items))
	for i := range list.Items {
		phase, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "phase")
		out[list.Items[i].GetName()] = phase
	}
	return out, nil
}

// fetchPVPhasesByLabel returns name → PV.status.phase for every PV
// labelled with ECClusterLabel=<ec.Name>. Stringly-typed return so the
// generic assessPhases() helper handles both unstructured CRs and PVs.
func (r *ElasticClusterReconciler) fetchPVPhasesByLabel(
	ctx context.Context, ec *v1alpha1.ElasticCluster,
) (map[string]string, error) {
	list := &corev1.PersistentVolumeList{}
	if err := r.Client.List(ctx, list, client.MatchingLabels{external.ECClusterLabel: ec.Name}); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list.Items))
	for i := range list.Items {
		out[list.Items[i].Name] = string(list.Items[i].Status.Phase)
	}
	return out, nil
}

// assessPhases walks every name in expected and counts how many have an
// observed phase that matches one of targets. Names that do not match
// (missing, blank or in another phase) are returned in `pending` as
// "name(observed)" tokens, sorted, ready for human-readable formatting.
func assessPhases(expected []string, observed map[string]string, targets ...string) (ready int, pending []string) {
	targetSet := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		targetSet[t] = struct{}{}
	}
	for _, name := range expected {
		phase, present := observed[name]
		switch {
		case !present:
			pending = append(pending, fmt.Sprintf("%s(NotFound)", name))
		case phase == "":
			pending = append(pending, fmt.Sprintf("%s(NoStatus)", name))
		default:
			if _, ok := targetSet[phase]; ok {
				ready++
				continue
			}
			pending = append(pending, fmt.Sprintf("%s(%s)", name, phase))
		}
	}
	sort.Strings(pending)
	return ready, pending
}

// formatPending caps the pending list to keep the condition message
// useful in `kubectl describe` output. The full list is still observable
// via the per-resource CR phase if the operator wants the long form.
func formatPending(pending []string) string {
	const cap = 5
	if len(pending) <= cap {
		return fmt.Sprintf("%v", pending)
	}
	return fmt.Sprintf("%v (+%d more)", pending[:cap], len(pending)-cap)
}
