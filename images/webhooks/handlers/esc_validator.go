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
	"sort"

	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
)

// reservedOSDStorageClassName mirrors api/v1alpha1.ReservedOSDStorageClassName.
//
// Duplicated as a plain string here because the webhook is published as
// a separate go module (`images/webhooks/go.mod`) and pulling the typed
// API package would require adding a `require` + `replace` directive to
// that module — a cross-module dependency we deferred to backlog item
// B19 (which collapses both reconcilers and the webhook onto a shared
// helper layer).
const reservedOSDStorageClassName = "sds-elastic-osd"

const (
	storageClassTypeRBD            = "RBD"
	replicationErasureCodedCompact = "ErasureCodedCompact"
	replicationHighRedundancy      = "HighRedundancy"
)

// HighRedundancy fault-tolerance thresholds. Sized to match the
// HighRedundancy data plane (size=4, min_size=2, failureDomain=host)
// and the auto-promoted control plane (mon.count=5, mgr.count=3): five
// nodes are needed to host a 5-mon quorum that tolerates two
// simultaneous host failures, four are the CRUSH-placement floor for a
// 4-replica pool with failureDomain=host. The webhook gates ESC
// creation on these numbers so that an HR ESC cannot trigger the
// sticky-promotion machinery on an undersized cluster (mon.count=5 on
// 3 nodes would lose quorum at the very first host failure).
const (
	hrMinNodesMatched = 5
	hrMinOSDNodes     = 4
)

// elasticClusterGVR is the dynamic-client GroupVersionResource for the
// ElasticCluster CR. The plural form is `elasticclusters` (lowercase,
// no hyphen) per the CRD's spec.names.plural.
var elasticClusterGVR = schema.GroupVersionResource{
	Group:    "storage.deckhouse.io",
	Version:  "v1alpha1",
	Resource: "elasticclusters",
}

// NewElasticStorageClassValidator builds a kwhvalidating.ValidatorFunc
// that enforces the ESC invariants the CRD's x-kubernetes-validations
// already encode plus a topology preflight for HighRedundancy:
//
//   - metadata.name MUST NOT collide with the helm-managed reserved OSD
//     StorageClass name.
//   - type=RBD with replication=ErasureCodedCompact is rejected
//     (csi-ceph does not yet provision RBD on erasure-coded pools —
//     backlog B4).
//   - On CREATE with replication=HighRedundancy the parent
//     ElasticCluster MUST exist and have at least
//     hrMinNodesMatched nodes matching its spec.storage.nodeSelector
//     and at least hrMinOSDNodes distinct nodes hosting an adopted
//     BlockDevice (label sds-elastic.deckhouse.io/cluster=<ec>).
//     A missing parent EC is rejected outright — HR is a sticky
//     promotion that cannot be safely deferred to "create the EC
//     later".
//   - spec.clusterRef, spec.type, and spec.replication are immutable
//     after creation; mutating any of them would orphan the underlying
//     Ceph pool.
//
// The duplication between this webhook and the CRD's CEL rules is
// intentional defense-in-depth: should the CRD ever be applied without
// `x-kubernetes-validations` (older Kubernetes versions, regenerated
// CRDs missing the validations stanza, dev clusters running with
// CRDValidationRatcheting disabled), admission still rejects bad
// requests. Both layers must stay in sync — the matching CEL rules live
// in `crds/elasticstorageclass.yaml`.
//
// The dynamic.Interface is injected so tests can substitute
// dynamicfake.NewSimpleDynamicClient. In production it is built from
// rest.InClusterConfig() in cmd/main.go and shared with the EC
// validator (same ServiceAccount, same RBAC).
func NewElasticStorageClassValidator(
	dyn dynamic.Interface,
) func(context.Context, *model.AdmissionReview, metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	return func(ctx context.Context, ar *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
		newObj, ok := obj.(*unstructured.Unstructured)
		if !ok {
			// Fail-closed: with failurePolicy=Fail, returning an error
			// causes the API server to deny the request rather than
			// silently accept an object the webhook could not inspect.
			klog.Errorf("[esc-validate] unexpected object type %T (expected *unstructured.Unstructured)", obj)
			return nil, fmt.Errorf("unexpected admission object type %T", obj)
		}

		if name := newObj.GetName(); name == reservedOSDStorageClassName {
			return reject(fmt.Sprintf(
				"ElasticStorageClass.metadata.name %q collides with the reserved internal OSD StorageClass; pick a different name",
				name,
			)), nil
		}

		scType, _, _ := unstructured.NestedString(newObj.Object, "spec", "type")
		replication, _, _ := unstructured.NestedString(newObj.Object, "spec", "replication")
		if scType == storageClassTypeRBD && replication == replicationErasureCodedCompact {
			return reject(
				"ElasticStorageClass with type=RBD does not support replication=ErasureCodedCompact " +
					"(csi-ceph does not yet provision RBD on erasure-coded pools)",
			), nil
		}

		// HighRedundancy preflight runs only on CREATE: spec.replication
		// is immutable (enforced below on UPDATE), so an UPDATE can never
		// introduce a HighRedundancy value that did not pass admission
		// the first time. Re-running the check on every UPDATE would
		// turn a legitimate apply (for example an annotation tweak) into
		// a rejection on a cluster that has temporarily lost a node.
		if ar.Operation == model.OperationCreate && replication == replicationHighRedundancy {
			if v := validateHighRedundancyTopology(ctx, dyn, newObj); v != nil {
				return v, nil
			}
		}

		if ar.Operation != model.OperationUpdate {
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		oldObj, err := decodeUnstructured(ar.OldObjectRaw)
		if err != nil {
			return nil, fmt.Errorf("decode oldObject: %w", err)
		}
		if oldObj == nil {
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		// CRD makes clusterRef, type, and replication required (with
		// minLength=1 for clusterRef and enum-validated values for type and
		// replication); old values are guaranteed non-empty on UPDATE.
		if v := mustImmutable(oldObj, newObj, "clusterRef", "spec.clusterRef"); v != nil {
			return v, nil
		}
		if v := mustImmutable(oldObj, newObj, "type", "spec.type"); v != nil {
			return v, nil
		}
		if v := mustImmutable(oldObj, newObj, "replication", "spec.replication"); v != nil {
			return v, nil
		}

		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}
}

// validateHighRedundancyTopology rejects CREATE of an ESC with
// replication=HighRedundancy when the parent ElasticCluster is absent
// or undersized.
//
// Three distinct rejection paths, each surfaced with an actionable
// message:
//
//   - spec.clusterRef is empty: malformed object (CRD requires it
//     non-empty, but defense-in-depth).
//   - parent EC is absent: HR is a sticky control-plane promotion;
//     creating the ESC against a not-yet-existing EC would either
//     no-op until the EC arrives (silent) or — worse — promote
//     mon.count=5 immediately upon EC creation regardless of how many
//     nodes the operator actually attached. Reject so the ordering is
//     enforced: EC first, observe Ready, then HR ESC.
//   - parent EC has too few nodeSelector-matching nodes (< 5) or too
//     few distinct nodes hosting adopted BlockDevices (< 4): the
//     fault-tolerance contract HR promises (two simultaneous host
//     failures with continued I/O) cannot be honoured by Ceph, and
//     mon.count=5 would lose quorum at the first failure.
func validateHighRedundancyTopology(
	ctx context.Context,
	dyn dynamic.Interface,
	escObj *unstructured.Unstructured,
) *kwhvalidating.ValidatorResult {
	clusterRef, _, _ := unstructured.NestedString(escObj.Object, "spec", "clusterRef")
	if clusterRef == "" {
		return reject(
			"ElasticStorageClass.spec.clusterRef is required for replication=HighRedundancy " +
				"(the parent ElasticCluster must exist and be sized for double-fault tolerance)",
		)
	}

	ecObj, err := dyn.Resource(elasticClusterGVR).Get(ctx, clusterRef, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return reject(fmt.Sprintf(
			"ElasticCluster %q referenced by ElasticStorageClass %q does not exist; HighRedundancy ESC requires an existing parent cluster sized for double-fault tolerance "+
				"(>= %d nodes matching spec.storage.nodeSelector, >= %d distinct nodes hosting adopted BlockDevices). "+
				"Apply the ElasticCluster first, wait for status.phase=Ready, then create the HighRedundancy ElasticStorageClass.",
			clusterRef, escObj.GetName(), hrMinNodesMatched, hrMinOSDNodes,
		))
	}
	if err != nil {
		klog.Errorf("[esc-validate] get ElasticCluster %q: %v", clusterRef, err)
		return reject(fmt.Sprintf("failed to load parent ElasticCluster %q: %v", clusterRef, err))
	}

	_, nodeSel, vr := buildSelectors(ecObj)
	if vr != nil {
		return vr
	}

	matchedNames, vr := allowedNodeNames(ctx, dyn, nodeSel)
	if vr != nil {
		return vr
	}
	nodesMatched := len(matchedNames)

	osdNodes, vr := countDistinctOSDNodes(ctx, dyn, clusterRef)
	if vr != nil {
		return vr
	}

	if nodesMatched < hrMinNodesMatched || osdNodes < hrMinOSDNodes {
		return reject(fmt.Sprintf(
			"ElasticStorageClass %q with replication=HighRedundancy requires at least %d nodes matching ElasticCluster %q spec.storage.nodeSelector (have %d) "+
				"and at least %d distinct nodes hosting adopted BlockDevices (have %d). "+
				"Wait for the cluster to scale out before creating the HighRedundancy ElasticStorageClass; see docs/USAGE.md.",
			escObj.GetName(),
			hrMinNodesMatched, clusterRef, nodesMatched,
			hrMinOSDNodes, osdNodes,
		))
	}
	return nil
}

// countDistinctOSDNodes returns the number of distinct
// BlockDevice.status.nodeName values for BDs already adopted by the
// referenced EC (label sds-elastic.deckhouse.io/cluster=<ec>).
//
// "Adopted" rather than "selectable" by design: HighRedundancy
// implies a sticky control-plane promotion, so the threshold must be
// satisfied by BDs the controller has already taken responsibility
// for, not BDs that merely match the selectors. This avoids racing
// admission against an in-flight scale-out.
//
// BDs without a populated status.nodeName (the BD CR exists but
// sds-node-configurator has not yet observed it on a host) are
// counted as "no node" and ignored — the count is monotonic in the
// number of fully-adopted BDs.
func countDistinctOSDNodes(
	ctx context.Context,
	dyn dynamic.Interface,
	ecName string,
) (int, *kwhvalidating.ValidatorResult) {
	owned, err := dyn.Resource(blockDeviceGVR).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", ecClusterLabel, ecName),
	})
	if err != nil {
		klog.Errorf("[esc-validate] list owned BlockDevices for %s: %v", ecName, err)
		return 0, reject(fmt.Sprintf("failed to enumerate adopted BlockDevices for ElasticCluster %q: %v", ecName, err))
	}
	seen := make(map[string]struct{}, len(owned.Items))
	for i := range owned.Items {
		nodeName, _, _ := unstructured.NestedString(owned.Items[i].Object, "status", "nodeName")
		if nodeName == "" {
			continue
		}
		seen[nodeName] = struct{}{}
	}

	if klog.V(4).Enabled() {
		names := make([]string, 0, len(seen))
		for n := range seen {
			names = append(names, n)
		}
		sort.Strings(names)
		klog.V(4).Infof("[esc-validate] EC %q adopted-BD distinct nodes: %v", ecName, names)
	}

	return len(seen), nil
}

// mustImmutable returns a reject result iff `spec.<field>` differs
// between oldObj and newObj. The fieldPath argument is used only for
// the human-readable rejection message.
func mustImmutable(oldObj, newObj *unstructured.Unstructured, field, fieldPath string) *kwhvalidating.ValidatorResult {
	oldVal, _, _ := unstructured.NestedString(oldObj.Object, "spec", field)
	newVal, _, _ := unstructured.NestedString(newObj.Object, "spec", field)
	if oldVal != newVal {
		return reject(fmt.Sprintf(
			"ElasticStorageClass.%s is immutable after creation (was %q, attempted %q)",
			fieldPath, oldVal, newVal,
		))
	}
	return nil
}
