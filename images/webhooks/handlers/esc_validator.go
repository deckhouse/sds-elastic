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

	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
)

// reservedOSDStorageClassName mirrors api/v1alpha1.ReservedOSDStorageClassName.
//
// Duplicated as a plain string here because the webhook is published as
// a separate go module (`images/webhooks/go.mod`) and pulling the typed
// API package would require adding a `require` + `replace` directive to
// that module — a cross-module dependency we deferred to backlog item
// B-N3 (which collapses both reconcilers and the webhook onto a shared
// helper layer).
const reservedOSDStorageClassName = "sds-elastic-osd"

const (
	storageClassTypeRBD            = "RBD"
	replicationErasureCodedCompact = "ErasureCodedCompact"
)

// ElasticStorageClassValidate enforces the same set of ESC invariants
// that the CRD already encodes via x-kubernetes-validations (CEL):
//
//   - metadata.name MUST NOT collide with the helm-managed reserved OSD
//     StorageClass name.
//   - type=RBD with replication=ErasureCodedCompact is rejected (csi-ceph
//     does not yet provision RBD on erasure-coded pools — backlog B4).
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
func ElasticStorageClassValidate(_ context.Context, ar *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
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
