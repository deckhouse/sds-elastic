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

package handlers

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
)

// ecClusterLabel mirrors external.ECClusterLabel from the controller
// module. Duplicated as a plain string because the webhook lives in a
// separate go module and pulling the controller's `external` package
// would require a cross-module replace directive (backlog item B19
// folds the controller, webhooks, and a shared helper layer onto a
// single module).
const ecClusterLabel = "sds-elastic.deckhouse.io/cluster"

// blockDeviceGVR is the dynamic-client GroupVersionResource for the
// sds-node-configurator BlockDevice CR. The plural form is
// `blockdevices` (lowercase, no hyphen) per the CRD's
// `spec.names.plural`.
var blockDeviceGVR = schema.GroupVersionResource{
	Group:    "storage.deckhouse.io",
	Version:  "v1alpha1",
	Resource: "blockdevices",
}

// nodeGVR is the dynamic-client GVR for core/v1 Nodes. Cluster-scoped.
var nodeGVR = schema.GroupVersionResource{
	Group:    "",
	Version:  "v1",
	Resource: "nodes",
}

// NewElasticClusterValidator builds a kwhvalidating.ValidatorFunc that
// guards UPDATE-operations on ElasticCluster.
//
// CREATE is passed through unconditionally (CRD schema validation is
// authoritative for shape; the controller short-circuits to a sane
// status if the cluster ends up in a degenerate state on first
// reconcile).
//
// On UPDATE the webhook enforces, in order:
//
//  1. spec.network immutability. The CRD already encodes the same rule
//     via x-kubernetes-validations; this is defense-in-depth for
//     clusters running without CEL ratcheting (older Kubernetes
//     versions, regenerated CRDs missing the validations stanza, dev
//     clusters with the CRDValidationRatcheting feature gate off).
//
//  2. Orphan-guard. spec.storage.{blockDeviceSelector,nodeSelector}
//     are EDITABLE (the CEL freeze on spec.storage was removed in the
//     same change that introduced this webhook). However, narrowing
//     the selectors after BlockDevice adoption would orphan resources
//     the controller cannot safely release without manual cleanup of
//     the LVG/LLV/PV plumbing. The webhook lists every BD already
//     labelled `sds-elastic.deckhouse.io/cluster=<ec.Name>` and
//     rejects the update if any of them no longer matches the new
//     selector pair.
//
//  3. Pre-flight ownership conflict. The controller's reconciliation
//     loop already refuses to overwrite a foreign-owned BD (surfacing
//     reason=OwnershipConflict on EC.status.conditions). Detecting
//     the same condition at admission time gives the operator a
//     synchronous, actionable error instead of a stalled reconcile.
//
// The dynamic.Interface is injected so tests can substitute
// `dynamicfake.NewSimpleDynamicClient`. In production it is built from
// rest.InClusterConfig() in cmd/main.go.
func NewElasticClusterValidator(
	dyn dynamic.Interface,
) func(context.Context, *model.AdmissionReview, metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	return func(ctx context.Context, ar *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
		if ar.Operation != model.OperationUpdate {
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		newObj, ok := obj.(*unstructured.Unstructured)
		if !ok {
			klog.Errorf("[ec-validate] unexpected object type %T (expected *unstructured.Unstructured)", obj)
			return nil, fmt.Errorf("unexpected admission object type %T", obj)
		}

		oldObj, err := decodeUnstructured(ar.OldObjectRaw)
		if err != nil {
			return nil, fmt.Errorf("decode oldObject: %w", err)
		}
		if oldObj == nil {
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		if v := validateNetworkImmutability(oldObj, newObj); v != nil {
			return v, nil
		}

		newBdSel, newNodeSel, v := buildSelectors(newObj)
		if v != nil {
			return v, nil
		}

		if v := validateNoOrphans(ctx, dyn, newObj.GetName(), newBdSel, newNodeSel); v != nil {
			return v, nil
		}

		if v := validateNoConflicts(ctx, dyn, newObj.GetName(), newBdSel); v != nil {
			return v, nil
		}

		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}
}

// validateNetworkImmutability returns a reject result when the
// ElasticCluster.spec.network sub-object is mutated on UPDATE.
//
// `unstructured.NestedMap` returns nil for an absent key; that is the
// shape we want to compare with reflect.DeepEqual rather than coercing
// to a typed object — Network is optional and the CRD already
// guarantees the schema of its leaves.
func validateNetworkImmutability(oldObj, newObj *unstructured.Unstructured) *kwhvalidating.ValidatorResult {
	oldNet, _, _ := unstructured.NestedMap(oldObj.Object, "spec", "network")
	newNet, _, _ := unstructured.NestedMap(newObj.Object, "spec", "network")
	if !reflect.DeepEqual(oldNet, newNet) {
		return reject("ElasticCluster.spec.network is immutable after creation (cannot be added, removed, or changed)")
	}
	return nil
}

// buildSelectors converts the new ElasticCluster's spec.storage
// selectors into labels.Selectors usable for List requests.
//
// Both selectors are required by the CRD schema, so a nil/empty value
// here means the operator submitted a malformed object. Failing fast
// at admission with a descriptive error is friendlier than letting
// the controller transiently report `StorageReady=False` on next
// reconcile.
func buildSelectors(newObj *unstructured.Unstructured) (labels.Selector, labels.Selector, *kwhvalidating.ValidatorResult) {
	bdSelRaw, _, err := unstructured.NestedMap(newObj.Object, "spec", "storage", "blockDeviceSelector")
	if err != nil {
		return nil, nil, reject(fmt.Sprintf("ElasticCluster.spec.storage.blockDeviceSelector is malformed: %v", err))
	}
	bdSel, vr := labelSelectorFromMap(bdSelRaw, "blockDeviceSelector")
	if vr != nil {
		return nil, nil, vr
	}

	nodeSelRaw, _, err := unstructured.NestedMap(newObj.Object, "spec", "storage", "nodeSelector")
	if err != nil {
		return nil, nil, reject(fmt.Sprintf("ElasticCluster.spec.storage.nodeSelector is malformed: %v", err))
	}
	nodeSel, vr := labelSelectorFromMap(nodeSelRaw, "nodeSelector")
	if vr != nil {
		return nil, nil, vr
	}

	return bdSel, nodeSel, nil
}

// labelSelectorFromMap deserialises an unstructured selector map (as
// stored under spec.storage) into a labels.Selector via the canonical
// metav1.LabelSelector round-trip. fieldName is used only for error
// messages.
func labelSelectorFromMap(raw map[string]interface{}, fieldName string) (labels.Selector, *kwhvalidating.ValidatorResult) {
	ls := &metav1.LabelSelector{}
	if raw != nil {
		if err := unstructuredToLabelSelector(raw, ls); err != nil {
			return nil, reject(fmt.Sprintf("ElasticCluster.spec.storage.%s is malformed: %v", fieldName, err))
		}
	}
	sel, err := metav1.LabelSelectorAsSelector(ls)
	if err != nil {
		return nil, reject(fmt.Sprintf("ElasticCluster.spec.storage.%s is invalid: %v", fieldName, err))
	}
	return sel, nil
}

// unstructuredToLabelSelector copies matchLabels / matchExpressions
// out of the unstructured map without going through JSON. We avoid
// pulling sigs.k8s.io/runtime/serializer here to keep the webhook's
// dependency surface small.
func unstructuredToLabelSelector(in map[string]interface{}, out *metav1.LabelSelector) error {
	if rawML, ok := in["matchLabels"]; ok && rawML != nil {
		ml, ok := rawML.(map[string]interface{})
		if !ok {
			return fmt.Errorf("matchLabels: expected map, got %T", rawML)
		}
		out.MatchLabels = make(map[string]string, len(ml))
		for k, v := range ml {
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("matchLabels[%q]: expected string, got %T", k, v)
			}
			out.MatchLabels[k] = s
		}
	}
	if rawME, ok := in["matchExpressions"]; ok && rawME != nil {
		me, ok := rawME.([]interface{})
		if !ok {
			return fmt.Errorf("matchExpressions: expected list, got %T", rawME)
		}
		out.MatchExpressions = make([]metav1.LabelSelectorRequirement, 0, len(me))
		for i, item := range me {
			m, ok := item.(map[string]interface{})
			if !ok {
				return fmt.Errorf("matchExpressions[%d]: expected map, got %T", i, item)
			}
			req := metav1.LabelSelectorRequirement{}
			if v, ok := m["key"]; ok {
				if s, ok := v.(string); ok {
					req.Key = s
				} else {
					return fmt.Errorf("matchExpressions[%d].key: expected string, got %T", i, v)
				}
			}
			if v, ok := m["operator"]; ok {
				if s, ok := v.(string); ok {
					req.Operator = metav1.LabelSelectorOperator(s)
				} else {
					return fmt.Errorf("matchExpressions[%d].operator: expected string, got %T", i, v)
				}
			}
			if v, ok := m["values"]; ok && v != nil {
				vals, ok := v.([]interface{})
				if !ok {
					return fmt.Errorf("matchExpressions[%d].values: expected list, got %T", i, v)
				}
				req.Values = make([]string, 0, len(vals))
				for j, vv := range vals {
					s, ok := vv.(string)
					if !ok {
						return fmt.Errorf("matchExpressions[%d].values[%d]: expected string, got %T", i, j, vv)
					}
					req.Values = append(req.Values, s)
				}
			}
			out.MatchExpressions = append(out.MatchExpressions, req)
		}
	}
	return nil
}

// validateNoOrphans rejects the update when one or more BlockDevices
// already adopted by `ecName` would no longer match the new
// blockDeviceSelector / nodeSelector pair, since the controller has
// no safe way to release them automatically — the operator must
// follow the manual release procedure documented in USAGE.md.
func validateNoOrphans(
	ctx context.Context,
	dyn dynamic.Interface,
	ecName string,
	newBdSel, newNodeSel labels.Selector,
) *kwhvalidating.ValidatorResult {
	owned, err := dyn.Resource(blockDeviceGVR).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", ecClusterLabel, ecName),
	})
	if err != nil {
		klog.Errorf("[ec-validate] list owned BlockDevices for %s: %v", ecName, err)
		return reject(fmt.Sprintf("failed to verify ownership invariants: %v", err))
	}
	if len(owned.Items) == 0 {
		return nil
	}

	allowedNodes, vr := allowedNodeNames(ctx, dyn, newNodeSel)
	if vr != nil {
		return vr
	}

	var orphans []string
	for i := range owned.Items {
		bd := owned.Items[i]
		bdLabels := labels.Set(bd.GetLabels())
		if !newBdSel.Matches(bdLabels) {
			orphans = append(orphans, bd.GetName())
			continue
		}
		nodeName, _, _ := unstructured.NestedString(bd.Object, "status", "nodeName")
		if !allowedNodes[nodeName] {
			orphans = append(orphans, bd.GetName())
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	sort.Strings(orphans)
	return reject(fmt.Sprintf(
		"selector update would orphan adopted BlockDevices [%s]; release them manually first (see docs/USAGE.md)",
		strings.Join(orphans, ", "),
	))
}

// allowedNodeNames builds the set of node names matching the new
// nodeSelector. An empty selector matches every node, which means
// every adopted BD passes the node-membership half of the orphan
// check and only the bdSelector part can produce orphans.
func allowedNodeNames(ctx context.Context, dyn dynamic.Interface, sel labels.Selector) (map[string]bool, *kwhvalidating.ValidatorResult) {
	opts := metav1.ListOptions{}
	if sel != nil && !sel.Empty() {
		opts.LabelSelector = sel.String()
	}
	nodes, err := dyn.Resource(nodeGVR).List(ctx, opts)
	if err != nil {
		klog.Errorf("[ec-validate] list nodes for selector %q: %v", sel.String(), err)
		return nil, reject(fmt.Sprintf("failed to verify nodeSelector against cluster Nodes: %v", err))
	}
	out := make(map[string]bool, len(nodes.Items))
	for i := range nodes.Items {
		out[nodes.Items[i].GetName()] = true
	}
	return out, nil
}

// validateNoConflicts rejects the update when the new
// blockDeviceSelector matches a BD already labelled as owned by a
// different ElasticCluster. The controller would otherwise short-
// circuit on next reconcile with reason=OwnershipConflict; doing the
// check here yields a synchronous error to the operator.
func validateNoConflicts(
	ctx context.Context,
	dyn dynamic.Interface,
	ecName string,
	newBdSel labels.Selector,
) *kwhvalidating.ValidatorResult {
	matched, err := dyn.Resource(blockDeviceGVR).List(ctx, metav1.ListOptions{
		LabelSelector: newBdSel.String(),
	})
	if err != nil {
		klog.Errorf("[ec-validate] list BDs by selector %q: %v", newBdSel.String(), err)
		return reject(fmt.Sprintf("failed to pre-validate ownership conflicts: %v", err))
	}
	var conflicts []string
	for i := range matched.Items {
		bd := matched.Items[i]
		owner := bd.GetLabels()[ecClusterLabel]
		if owner == "" || owner == ecName {
			continue
		}
		conflicts = append(conflicts, fmt.Sprintf("%s (claimed by %s)", bd.GetName(), owner))
	}
	if len(conflicts) == 0 {
		return nil
	}
	sort.Strings(conflicts)
	return reject(fmt.Sprintf(
		"selector update would adopt BlockDevices already owned by another ElasticCluster: [%s]",
		strings.Join(conflicts, ", "),
	))
}
