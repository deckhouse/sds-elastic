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
	"strings"

	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
)

// moduleConfigName is the ModuleConfig object name that toggles the
// sds-elastic module on and off (it equals the module's plural name).
//
// Duplicated as a plain string here — instead of importing
// hooks/go/consts.ModulePluralName — because the webhook is published as
// a separate go module (`images/webhooks/go.mod`) that intentionally
// carries no cross-module dependency on the hooks/controller code
// (backlog item B19 collapses them onto a shared helper layer).
//
// The mc-validation VWC already scopes the webhook to this name via
// matchConditions (request.name == "sds-elastic"); the handler re-checks
// it as defense-in-depth so a mis-scoped or unit-test invocation against
// a different ModuleConfig is a silent no-op rather than a spurious
// denial.
const moduleConfigName = "sds-elastic"

// ForceDisableAnnotation, when set to "true" on the sds-elastic
// ModuleConfig, authorises disabling the module (spec.enabled=false)
// even while ElasticCluster CRs still exist.
//
// This is a deliberate escape hatch for disaster-recovery / advanced
// operators: disabling the module stops the controller and the Rook
// operator, so the leftover ElasticCluster CRs stop being reconciled and
// the module-delete hook strips their finalizers to let the API server
// GC them. The OSD data on the host disks and dataDirHostPath is left
// untouched — it is simply no longer managed. Without the annotation the
// webhook hard-blocks the disable to prevent an accidental orphaning of
// a live Ceph cluster.
const ForceDisableAnnotation = "sds-elastic.deckhouse.io/force-disable"

// NewModuleConfigValidator builds a kwhvalidating.ValidatorFunc that
// blocks disabling the sds-elastic module while one or more
// ElasticCluster CRs still exist.
//
// The guard fires on CREATE/UPDATE of the sds-elastic ModuleConfig when
// the incoming object has spec.enabled=false. It is bypassed only when
// the ModuleConfig carries the ForceDisableAnnotation set to "true".
//
// Rationale: disabling the module tears down the controller and the
// vendored Rook operator. Any surviving ElasticCluster would then be
// left with controller finalizers that nothing can clear through the
// normal teardown path, and — worse — the backing Ceph cluster (OSD data
// on host disks) would be orphaned with no operator to manage it. By
// denying the disable while ECs exist, the operator is forced through
// the documented ordered teardown (delete ESC -> delete EC -> disable
// module). The force annotation exists for the rare case where the
// operator deliberately wants to keep the EC CRs / on-disk data and stop
// managing them.
//
// failurePolicy=Fail on the VWC is intentional and safe here: when the
// webhook is unreachable the API server denies the ModuleConfig change,
// so the guard cannot be bypassed by scaling the webhook down. There is
// no re-enable deadlock because the VWC is part of the module's Helm
// release and is removed when the module uninstalls, so toggling
// spec.enabled back to true is never intercepted.
//
// The dynamic.Interface is injected so tests can substitute
// dynamicfake.NewSimpleDynamicClient. In production it is built from
// rest.InClusterConfig() in cmd/main.go and shared with the EC/ESC
// validators (same ServiceAccount, same RBAC).
func NewModuleConfigValidator(
	dyn dynamic.Interface,
) func(context.Context, *model.AdmissionReview, metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	return func(ctx context.Context, ar *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
		newObj, ok := obj.(*unstructured.Unstructured)
		if !ok {
			// Fail-closed: with failurePolicy=Fail, returning an error
			// makes the API server deny the request rather than silently
			// accept a ModuleConfig the webhook could not inspect.
			klog.Errorf("[mc-validate] unexpected object type %T (expected *unstructured.Unstructured)", obj)
			return nil, fmt.Errorf("unexpected admission object type %T", obj)
		}

		// Defense-in-depth scope guard (see moduleConfigName doc): only
		// the sds-elastic ModuleConfig is our concern.
		if newObj.GetName() != moduleConfigName {
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		// Only CREATE/UPDATE can introduce spec.enabled=false. DELETE of
		// the ModuleConfig is handled by Deckhouse module lifecycle (and
		// by the module-delete finalizer hook) and is never blocked here.
		if ar.Operation != model.OperationCreate && ar.Operation != model.OperationUpdate {
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		// Never block updates to a ModuleConfig that is already being
		// deleted. Once a deletionTimestamp is set the module is on its way
		// out through its own lifecycle; the only remaining UPDATEs are
		// finalizer removals. Denying those would wedge the object in
		// Terminating with no way to make progress. (Same short-circuit the
		// csi-nfs mc-validate webhook performs.)
		if newObj.GetDeletionTimestamp() != nil {
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		enabled, found, err := unstructured.NestedBool(newObj.Object, "spec", "enabled")
		if err != nil {
			klog.Errorf("[mc-validate] read spec.enabled on ModuleConfig %q: %v", newObj.GetName(), err)
			return reject(fmt.Sprintf("failed to read spec.enabled on ModuleConfig %q: %v", newObj.GetName(), err)), nil
		}
		// Absent or true => the module stays enabled => nothing to guard.
		if !found || enabled {
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		// spec.enabled == false: the operator is disabling the module.
		if newObj.GetAnnotations()[ForceDisableAnnotation] == "true" {
			klog.Infof(
				"[mc-validate] %s=true on ModuleConfig %q: allowing disable despite any existing ElasticClusters",
				ForceDisableAnnotation, newObj.GetName(),
			)
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		ecList, err := dyn.Resource(elasticClusterGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			klog.Errorf("[mc-validate] list ElasticClusters: %v", err)
			// Fail-closed: do not allow a disable we could not verify is
			// safe.
			return reject(fmt.Sprintf(
				"failed to verify that no ElasticClusters exist before disabling the module: %v", err,
			)), nil
		}
		if len(ecList.Items) == 0 {
			return &kwhvalidating.ValidatorResult{Valid: true}, nil
		}

		names := make([]string, 0, len(ecList.Items))
		for i := range ecList.Items {
			names = append(names, ecList.Items[i].GetName())
		}
		sort.Strings(names)
		return reject(fmt.Sprintf(
			"cannot disable the sds-elastic module while %d ElasticCluster(s) still exist [%s]: "+
				"disabling stops the controller and the Rook operator, which would orphan the backing Ceph cluster "+
				"(OSD data on host disks) and leave the ElasticCluster CRs unmanaged. "+
				"Delete the ElasticStorageClasses and ElasticClusters first (see docs/USAGE.md), then disable the module. "+
				"To disable anyway and keep the ElasticCluster CRs and on-disk data unmanaged, set the annotation %s=\"true\" on the ModuleConfig.",
			len(names), strings.Join(names, ", "), ForceDisableAnnotation,
		)), nil
	}
}
