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
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

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

// ErrOwnershipConflict is returned by adoptBlockDevice when the BD already
// carries `sds-elastic.deckhouse.io/cluster=<otherEC>`. ensureStorage maps
// it (via listMatchingBlockDevices.conflicts) onto a StorageReady=False /
// reason=OwnershipConflict condition rather than a hard reconcile error.
var ErrOwnershipConflict = errors.New("blockdevice claimed by another ElasticCluster")

// bdOwnershipConflict is a (BD name, current owner EC name) pair surfaced
// by listMatchingBlockDevices when a BD matching this EC's selector is
// already owned by a different EC. Aggregated into the StorageReady
// condition's message so the operator sees what to clear.
type bdOwnershipConflict struct {
	bdName  string
	ownerEC string
}

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

// selectedBD captures the per-BlockDevice values the sequential storage
// stages need to consume after Phase 0 has validated and adopted the BD.
// Stored in a slice so subsequent stages can iterate without re-listing
// BDs and without re-running validation.
type selectedBD struct {
	bdName   string
	nodeName string
	capacity resource.Quantity
}

// ensureStorage reconciles the LVM-backed storage chain BlockDevice → LVG →
// LLV → local PV for every BlockDevice that matches
// ec.spec.storage.{nodeSelector,blockDeviceSelector}.
//
// The stage is split into three sequential sub-phases that strictly
// honour the LVG → LLV → PV dependency on the host:
//
//  1. Adopt every selected BD and upsert its LVG. Wait for
//     LVG.status.phase == Ready before progressing.
//  2. Upsert every LLV. Wait for LLV.status.phase == Created.
//  3. Upsert every local PV. Wait for PV.status.phase ∈ {Available, Bound}.
//
// LLV CRs are NOT created until every LVG is Ready; PV CRs are NOT
// created until every LLV is Created. The original implementation
// upserted the whole chain in one pass and relied on sds-node-configurator
// (SNC) to retry LLV reconciliation once its parent LVG flipped to Ready.
// SNC currently has a watch race in this path — an LLV created before
// its LVG sometimes never gets re-reconciled — so we sequence the
// upserts ourselves to keep the contract independent of upstream bugs.
//
// Returns:
//   - done       — true once every selected BD has a matching LVG/LLV/PV
//     AND the underlying CRs report a target status.phase: LVG=Ready,
//     LLV=Created, PV in {Available, Bound}.
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
	matchedBDs, conflicts, listErr := r.listMatchingBlockDevices(ctx, ec)
	if listErr != nil {
		if isNoMatchErr(listErr) {
			return false, 0, resource.Quantity{}, storageReasonWaitingForBDCRD,
				"waiting for BlockDevice CRD (sds-node-configurator)", nil
		}
		return false, 0, resource.Quantity{}, "", "", fmt.Errorf("list BlockDevices: %w", listErr)
	}
	if len(conflicts) > 0 {
		// Surface conflicts before everything else: even one foreign-owned
		// BD halts the stage so we never silently transfer ownership or
		// half-provision the cluster against a contested device set.
		parts := make([]string, 0, len(conflicts))
		for _, c := range conflicts {
			parts = append(parts, fmt.Sprintf("%s: claimed by %s", c.bdName, c.ownerEC))
		}
		return false, 0, resource.Quantity{}, v1alpha1.ECReasonOwnershipConflict,
			fmt.Sprintf("ownership conflicts: %s", strings.Join(parts, "; ")), nil
	}
	if len(matchedBDs) == 0 {
		return false, 0, resource.Quantity{}, storageReasonNoBlockDevices,
			"no BlockDevices match storage.{nodeSelector,blockDeviceSelector}", nil
	}

	// Phase 0: validate every BD, adopt it (idempotent label patch), and
	// upsert its LVG. LLV/PV CRs intentionally are NOT touched yet.
	selected := make([]selectedBD, 0, len(matchedBDs))
	skipped := []string{}
	var minSize resource.Quantity
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
			if errors.Is(err, ErrOwnershipConflict) {
				// listMatchingBlockDevices already filters foreign-owned
				// BDs into the conflicts slice; reaching here means a
				// concurrent relabel slipped in between List and Patch
				// (caught by MergeFromWithOptimisticLock). Surface as the
				// same reason, not a hard error — the next reconcile
				// re-Lists and the BD lands in conflicts properly.
				return false, 0, resource.Quantity{}, v1alpha1.ECReasonOwnershipConflict, err.Error(), nil
			}
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

		if len(selected) == 0 || capacity.Cmp(minSize) < 0 {
			minSize = capacity
		}
		selected = append(selected, selectedBD{bdName: bdName, nodeName: nodeName, capacity: capacity})
	}

	if len(skipped) > 0 {
		return false, int32(len(selected)), minSize, storageReasonWaitingForBlockDevices,
			fmt.Sprintf("selected %d BlockDevices for LVG/LLV/PV provisioning; skipped %d unusable: %v",
				len(selected), len(skipped), skipped),
			nil
	}
	if len(selected) == 0 {
		// listMatchingBlockDevices returned items but every one failed
		// validation above. Treat as transient: surface "no usable BDs"
		// rather than declaring storage Ready on an empty set.
		return false, 0, resource.Quantity{}, storageReasonNoBlockDevices,
			"no usable BlockDevices after validation", nil
	}
	osdCount = int32(len(selected))
	pvcRequest = minSize

	managedNames := make([]string, 0, len(selected))
	for _, s := range selected {
		managedNames = append(managedNames, builder.ECOSDResourceName(ec, s.bdName))
	}
	sort.Strings(managedNames)

	// Phase 1 gate: every LVG must be Ready before LLVs are created.
	// Not gating here means SNC sees a fresh LLV pointing at a not-yet-
	// provisioned VG and (today) hangs in NotReady because the LVG ready
	// event does not re-enqueue the LLV. Sequencing the create on our
	// side makes the contract independent of that watch race.
	lvgPhases, err := r.fetchUnstructuredPhasesByLabel(ctx, ec, external.LVMVolumeGroupGVK)
	if err != nil {
		return false, osdCount, pvcRequest, "", "", fmt.Errorf("list LVMVolumeGroups for status: %w", err)
	}
	if ready, pending := assessPhases(managedNames, lvgPhases, lvgPhaseReady); ready < int(osdCount) {
		return false, osdCount, pvcRequest, storageReasonWaitingForLVG,
			fmt.Sprintf("%d/%d LVMVolumeGroups Ready; pending: %s",
				ready, osdCount, formatPending(pending)),
			nil
	}

	// Phase 2: every LVG is Ready — upsert LLVs.
	for _, s := range selected {
		llv := builder.ECLVMLogicalVolume(ec, s.bdName)
		if err := r.upsertECUnstructured(ctx, llv); err != nil {
			if isNoMatchErr(err) {
				return false, osdCount, pvcRequest, storageReasonWaitingForLLVCRD,
					"waiting for LVMLogicalVolume CRD (sds-node-configurator)", nil
			}
			return false, osdCount, pvcRequest, "", "", fmt.Errorf("upsert LVMLogicalVolume %s: %w", llv.GetName(), err)
		}
	}

	// Phase 2 gate: every LLV must be Created (LV provisioned on the
	// host) before we expose the local PV. Otherwise kubelet would race
	// FailedMapVolume / EvalHostSymlinks against the missing /dev/<vg>/<lv>.
	llvPhases, err := r.fetchUnstructuredPhasesByLabel(ctx, ec, external.LVMLogicalVolumeGVK)
	if err != nil {
		return false, osdCount, pvcRequest, "", "", fmt.Errorf("list LVMLogicalVolumes for status: %w", err)
	}
	if ready, pending := assessPhases(managedNames, llvPhases, llvPhaseCreated); ready < int(osdCount) {
		return false, osdCount, pvcRequest, storageReasonWaitingForLLV,
			fmt.Sprintf("%d/%d LVMLogicalVolumes Created; pending: %s",
				ready, osdCount, formatPending(pending)),
			nil
	}

	// Phase 3: every LLV is Created — upsert local PVs.
	for _, s := range selected {
		pv := builder.ECOSDPersistentVolume(ec, s.bdName, s.nodeName, s.capacity)
		if err := r.upsertECPersistentVolume(ctx, pv); err != nil {
			return false, osdCount, pvcRequest, "", "", fmt.Errorf("upsert PersistentVolume %s: %w", pv.Name, err)
		}
	}

	// Phase 3 gate: every PV must be Available or Bound. A freshly
	// Create()d PV starts in the empty / Pending phase; the volume
	// binder transitions it once the LLV-backed device is observable.
	pvPhases, err := r.fetchPVPhasesByLabel(ctx, ec)
	if err != nil {
		return false, osdCount, pvcRequest, "", "", fmt.Errorf("list PersistentVolumes for status: %w", err)
	}
	if ready, pending := assessPhases(managedNames, pvPhases,
		string(corev1.VolumeAvailable), string(corev1.VolumeBound)); ready < int(osdCount) {
		return false, osdCount, pvcRequest, storageReasonWaitingForPV,
			fmt.Sprintf("%d/%d PersistentVolumes Available|Bound; pending: %s",
				ready, osdCount, formatPending(pending)),
			nil
	}

	return true, osdCount, pvcRequest, "",
		fmt.Sprintf("%d OSD volumes ready (LVG/LLV/PV)", osdCount), nil
}

// adoptBlockDevice patches ECClusterLabel onto a BlockDevice that is
// either unowned (no label, or label value == "") or already owned by
// this EC (idempotent short-circuit). Refuses to overwrite a non-empty
// ECClusterLabel pointing at a different EC: returns ErrOwnershipConflict
// so the storage stage surfaces the conflict via StorageReady=False /
// reason=OwnershipConflict instead of silently transferring ownership.
//
// Orphan recovery (the named owner no longer exists in the cluster) is
// intentionally NOT auto-handled here — the operator must clear the
// stale label manually. See B20 for OwnerReferences-based teardown that
// would obviate manual cleanup.
//
// MergeFromWithOptimisticLock anchors the patch to the BD's
// resourceVersion so a concurrent reconcile (or a manual relabel landing
// between our List and Patch) cannot race past the trichotomy: a stale
// pre-image yields HTTP 409, controller-runtime requeues, and the next
// pass sees the freshly written foreign label and refuses.
func (r *ElasticClusterReconciler) adoptBlockDevice(ctx context.Context, bd *unstructured.Unstructured, ec *v1alpha1.ElasticCluster) error {
	cur, hasLabel := bd.GetLabels()[external.ECClusterLabel]
	switch {
	case hasLabel && cur == ec.Name:
		return nil
	case hasLabel && cur != "":
		return fmt.Errorf("%w: %q claimed by %q", ErrOwnershipConflict, bd.GetName(), cur)
	}
	patch := client.MergeFromWithOptions(bd.DeepCopy(), client.MergeFromWithOptimisticLock{})
	labels := bd.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[external.ECClusterLabel] = ec.Name
	bd.SetLabels(labels)
	return r.Client.Patch(ctx, bd, patch)
}

// listMatchingBlockDevices returns the union of two BlockDevice
// populations as `selected`, plus any foreign-owned conflicts:
//
//  1. Sticky-owned (always included): BlockDevices labelled with
//     `sds-elastic.deckhouse.io/cluster=<ec.Name>`. These are BDs we
//     have previously adopted; they bypass both
//     `spec.storage.blockDeviceSelector` AND `spec.storage.nodeSelector`.
//     The CephCluster.spec.storageClassDeviceSets[0].count is derived
//     from len(selected); shrinking it does not, by itself, remove
//     OSDs (Rook only does so with `removeOSDsIfOutAndSafeToRemove:
//     true`, which we leave at the default `false`), but it does
//     immediately stop new pods being scheduled and produces churn in
//     the spec. Sticky inclusion makes the count monotonic for the
//     lifetime of an EC: once a BD is adopted, the only way to
//     decrement count is to manually clear the label after deleting
//     the LVG/LLV/PV plumbing.
//
//  2. Selector-matching (included when consumable + node match):
//     BDs that match `blockDeviceSelector` and live on a node matching
//     `nodeSelector`. Foreign-owned BDs (label points at a different
//     EC) are excluded and surfaced via `conflicts` so ensureStorage
//     can short-circuit with reason=OwnershipConflict.
//
// Dedup is by BD name; the sticky list takes precedence so the
// returned object is the freshest copy of the BD CR available.
func (r *ElasticClusterReconciler) listMatchingBlockDevices(
	ctx context.Context, ec *v1alpha1.ElasticCluster,
) (selected []unstructured.Unstructured, conflicts []bdOwnershipConflict, err error) {
	bdSelector, err := metav1.LabelSelectorAsSelector(ec.Spec.Storage.BlockDeviceSelector)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid blockDeviceSelector: %w", err)
	}
	nodeSelector, err := metav1.LabelSelectorAsSelector(ec.Spec.Storage.NodeSelector)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid nodeSelector: %w", err)
	}

	matchingNodes, err := r.matchingNodeNames(ctx, nodeSelector)
	if err != nil {
		return nil, nil, fmt.Errorf("list matching Nodes: %w", err)
	}

	// List 1: sticky — every BD already labelled as ours.
	owned := &unstructured.UnstructuredList{}
	owned.SetGroupVersionKind(schemaListKind(external.BlockDeviceGVK))
	if err := r.Client.List(ctx, owned, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{external.ECClusterLabel: ec.Name}),
	}); err != nil {
		return nil, nil, err
	}

	// List 2: BDs matching the user-defined blockDeviceSelector.
	matched := &unstructured.UnstructuredList{}
	matched.SetGroupVersionKind(schemaListKind(external.BlockDeviceGVK))
	if err := r.Client.List(ctx, matched, &client.ListOptions{LabelSelector: bdSelector}); err != nil {
		return nil, nil, err
	}

	seen := make(map[string]struct{}, len(owned.Items)+len(matched.Items))
	out := make([]unstructured.Unstructured, 0, len(owned.Items)+len(matched.Items))

	// Sticky pass: include unconditionally. We cannot rescind ownership
	// without data-loss risk, so selector drift on a previously-adopted
	// BD must NOT shrink the working set.
	for _, bd := range owned.Items {
		seen[bd.GetName()] = struct{}{}
		out = append(out, bd)
	}

	// Selector pass: fresh adoption candidates plus conflict surfacing.
	for _, bd := range matched.Items {
		if _, dup := seen[bd.GetName()]; dup {
			continue
		}
		cur, hasLabel := bd.GetLabels()[external.ECClusterLabel]
		if hasLabel && cur != "" && cur != ec.Name {
			conflicts = append(conflicts, bdOwnershipConflict{bdName: bd.GetName(), ownerEC: cur})
			continue
		}
		// Reaching here: BD is not foreign-owned and not yet adopted by us
		// (sticky path would have caught alreadyOurs). To enrol it for
		// fresh adoption it must be consumable AND on a matching node.
		nodeName, _, _ := unstructured.NestedString(bd.Object, "status", "nodeName")
		consumable, _, _ := unstructured.NestedBool(bd.Object, "status", "consumable")
		if !consumable {
			continue
		}
		if !matchingNodes[nodeName] {
			continue
		}
		seen[bd.GetName()] = struct{}{}
		out = append(out, bd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].bdName < conflicts[j].bdName })
	return out, conflicts, nil
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
	const maxItems = 5
	if len(pending) <= maxItems {
		return fmt.Sprintf("%v", pending)
	}
	return fmt.Sprintf("%v (+%d more)", pending[:maxItems], len(pending)-maxItems)
}
