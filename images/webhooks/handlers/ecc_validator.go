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

// ElasticClusterCredentialValidate enforces the FSID-immutability
// invariant on the cluster-scoped ECC.
//
// The controller back-syncs spec.fsid from the rook-ceph-mon Secret on
// the first successful read; once populated, the field encodes the
// identity of the Ceph cluster. Mutating a populated FSID would
// silently repoint csi-ceph clients at a different Ceph cluster and is
// treated as an unrecoverable error rather than a recoverable drift.
//
// Allowed transitions:
//
//   - "" -> any value:                initial population by the controller.
//   - <fsid> -> <fsid> (unchanged):   no-op (other fields may change).
//   - <fsid> -> "" or <other-fsid>:   rejected.
//
// Like the ESC validator, this rule duplicates the FSID-immutability
// guarantee that should ideally live as an `x-kubernetes-validations`
// block on the ECC CRD (`crds/internal/elasticclustercredential.yaml`);
// the CRD-level CEL is currently omitted because the controller
// back-syncs fields incrementally and the resource transiently exists
// with an empty FSID. Doing the check at admission time is intentional
// defense-in-depth.
//
// MonSecret / AdminSecret are intentionally NOT immutable here — Rook
// rotates the cephx secrets and the BACK-SYNC reconciler must be free
// to update the mirror. RESTORE-direction validation (ECC.spec → live
// Secret) is part of B20 and out of scope for the MVP; "fully
// populated on Phase=Populated" is also out of scope (B22) — the
// controller-side comment in
// `images/controller/internal/controller/elastic_cluster_credential_controller.go`
// has been brought in line with this.
func ElasticClusterCredentialValidate(_ context.Context, ar *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	if ar.Operation != model.OperationUpdate {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	newObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		// Fail-closed: see the equivalent comment in
		// ElasticStorageClassValidate.
		klog.Errorf("[ecc-validate] unexpected object type %T (expected *unstructured.Unstructured)", obj)
		return nil, fmt.Errorf("unexpected admission object type %T", obj)
	}

	oldObj, err := decodeUnstructured(ar.OldObjectRaw)
	if err != nil {
		return nil, fmt.Errorf("decode oldObject: %w", err)
	}
	if oldObj == nil {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	oldFSID, _, _ := unstructured.NestedString(oldObj.Object, "spec", "fsid")
	if oldFSID == "" {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	newFSID, _, _ := unstructured.NestedString(newObj.Object, "spec", "fsid")
	if newFSID != oldFSID {
		return reject(fmt.Sprintf(
			"ElasticClusterCredential.spec.fsid is immutable after first population (was %q, attempted %q)",
			oldFSID, newFSID,
		)), nil
	}

	return &kwhvalidating.ValidatorResult{Valid: true}, nil
}
