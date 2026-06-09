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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// rookDeletionBlockedConditionType is the condition Rook publishes on a
// CephCluster (and CephFilesystem) when deletion is blocked by remaining
// dependents — for a CephCluster that means PersistentVolumes still
// reference it (allowUninstallWithVolumes=false). Mirrors
// cephv1.ConditionDeletionIsBlocked; duplicated as a literal so the
// controller does not take a build dependency on the Rook Go module.
const rookDeletionBlockedConditionType = "DeletionIsBlocked"

// ensureECFinalizer adds ECFinalizer to the ElasticCluster via a
// merge-patch if it is not already present. The patch touches only
// metadata.finalizers, so it does not bump generation and cannot race
// with a concurrent status update.
func (r *ElasticClusterReconciler) ensureECFinalizer(ctx context.Context, ec *v1alpha1.ElasticCluster) error {
	if controllerutil.ContainsFinalizer(ec, external.ECFinalizer) {
		return nil
	}
	patch := client.MergeFrom(ec.DeepCopy())
	controllerutil.AddFinalizer(ec, external.ECFinalizer)
	return r.Client.Patch(ctx, ec, patch)
}

// removeECFinalizer drops ECFinalizer so the API server can finish
// deleting the CR. Idempotent: a no-op when the finalizer is already
// gone.
func (r *ElasticClusterReconciler) removeECFinalizer(ctx context.Context, ec *v1alpha1.ElasticCluster) error {
	if !controllerutil.ContainsFinalizer(ec, external.ECFinalizer) {
		return nil
	}
	patch := client.MergeFrom(ec.DeepCopy())
	controllerutil.RemoveFinalizer(ec, external.ECFinalizer)
	return r.Client.Patch(ctx, ec, patch)
}

// reconcileDelete runs the ordered ElasticCluster teardown behind a
// non-bypassable dependency guard:
//
//  1. Refuse to proceed while any ElasticStorageClass still references
//     this cluster — the operator must delete those first (their own
//     teardown owns the destructive pool/filesystem removal). This guard
//     has no force override: EC deletion without remaining ESCs is
//     reversible (OSD disks and the mon store are preserved).
//  2. Delete the CephCluster and CephClusterConnection — the two
//     resources the operator cannot delete by hand because the
//     vendor-cr-validation webhook rejects manual deletes.
//  3. While the backend is still terminating, translate the cause into a
//     domain-level Ready condition (VolumesExist when Rook reports
//     DeletionIsBlocked, otherwise Terminating) without leaking any Rook
//     resource names.
//  4. Once both are gone, drop the finalizer.
//
// PV / LLV / LVG and the BlockDevice cluster label are intentionally left
// in place for the operator to clean up by label (documented procedure).
func (r *ElasticClusterReconciler) reconcileDelete(ctx context.Context, ec *v1alpha1.ElasticCluster) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ec, external.ECFinalizer) {
		return ctrl.Result{}, nil
	}

	dependents, err := r.dependentESCNames(ctx, ec)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(dependents) > 0 {
		// This message intentionally surfaces the operator's own ESC
		// names (not vendor entities), so the no-vendor-leak screen does
		// not apply to it.
		msg := fmt.Sprintf("delete dependent ElasticStorageClasses first: %s", strings.Join(dependents, ", "))
		if err := r.patchECDeleteCondition(ctx, ec, v1alpha1.ECReasonStorageClassesExist, msg); err != nil {
			return ctrl.Result{}, err
		}
		// Re-enqueued by the ElasticStorageClass watch on deletion;
		// RequeueAfter is only the safety net.
		return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
	}

	ccNN := types.NamespacedName{Namespace: r.Cfg.ControllerNamespace, Name: builder.ECCephClusterName(ec)}
	cccNN := types.NamespacedName{Name: builder.ECCephClusterConnectionName(ec)}

	if err := r.deleteExternalIfExists(ctx, external.CephClusterGVK, ccNN); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteExternalIfExists(ctx, external.CephClusterConnectionGVK, cccNN); err != nil {
		return ctrl.Result{}, err
	}

	cc, ccFound, err := r.getExternalIfExists(ctx, external.CephClusterGVK, ccNN)
	if err != nil {
		return ctrl.Result{}, err
	}
	_, cccFound, err := r.getExternalIfExists(ctx, external.CephClusterConnectionGVK, cccNN)
	if err != nil {
		return ctrl.Result{}, err
	}

	if ccFound || cccFound {
		reason := v1alpha1.ECReasonTerminating
		msg := "removing storage backend resources"
		if ccFound && externalDeletionBlocked(cc) {
			reason = v1alpha1.ECReasonVolumesExist
			msg = "PersistentVolumes still reference this cluster; delete the remaining PersistentVolumes first"
		}
		if err := r.patchECDeleteCondition(ctx, ec, reason, msg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
	}

	if err := r.removeECFinalizer(ctx, ec); err != nil {
		return ctrl.Result{}, err
	}
	r.Log.Info(fmt.Sprintf("[reconcileDelete] ElasticCluster %q teardown complete; finalizer removed", ec.Name))
	return ctrl.Result{}, nil
}

// dependentESCNames returns the sorted names of every ElasticStorageClass
// whose spec.clusterRef points at this ElasticCluster.
func (r *ElasticClusterReconciler) dependentESCNames(ctx context.Context, ec *v1alpha1.ElasticCluster) ([]string, error) {
	list := &v1alpha1.ElasticStorageClassList{}
	if err := r.Client.List(ctx, list); err != nil {
		return nil, err
	}
	var names []string
	for i := range list.Items {
		if list.Items[i].Spec.ClusterRef == ec.Name {
			names = append(names, list.Items[i].Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// deleteExternalIfExists issues a Delete for an unstructured vendor
// resource. A missing object (NotFound) or a not-yet-installed CRD
// (NoMatch) both count as "already gone" and return nil.
func (r *ElasticClusterReconciler) deleteExternalIfExists(ctx context.Context, gvk schema.GroupVersionKind, nn types.NamespacedName) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(nn.Name)
	u.SetNamespace(nn.Namespace)
	err := r.Client.Delete(ctx, u)
	if apierrors.IsNotFound(err) || isNoMatchErr(err) {
		return nil
	}
	return err
}

// getExternalIfExists fetches an unstructured vendor resource. Returns
// found=false for both NotFound and NoMatch (CRD absent).
//
// Treating NoMatch as "gone" in the teardown confirmation step is
// deliberate: a missing CRD means the resource cannot exist (the API
// server garbage-collects all CRs when their CRD is removed), so the
// finalizer can be released safely. The alternative — treating NoMatch as
// "still present" — would wedge the ElasticCluster in Terminating forever
// whenever the dependency CRD has genuinely been uninstalled.
func (r *ElasticClusterReconciler) getExternalIfExists(ctx context.Context, gvk schema.GroupVersionKind, nn types.NamespacedName) (*unstructured.Unstructured, bool, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := r.Client.Get(ctx, nn, u)
	if apierrors.IsNotFound(err) || isNoMatchErr(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return u, true, nil
}

// externalDeletionBlocked reports whether a Rook resource carries the
// DeletionIsBlocked=True condition (CephCluster: bound PVs remain).
func externalDeletionBlocked(u *unstructured.Unstructured) bool {
	return unstructuredConditionTrue(u, rookDeletionBlockedConditionType)
}

// patchECDeleteCondition publishes the aggregate Ready=False teardown
// condition (with a domain-level reason/message) and pins Phase to
// InProgress. Uses RetryOnConflict and skips the write when nothing
// changed so a blocked teardown does not churn the status. A CR that has
// already been removed (NotFound) is treated as success.
func (r *ElasticClusterReconciler) patchECDeleteCondition(ctx context.Context, ec *v1alpha1.ElasticCluster, reason, msg string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.ElasticCluster{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: ec.Name}, latest); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if latest.Status == nil {
			latest.Status = &v1alpha1.ElasticClusterStatus{}
		}
		before := latest.Status.DeepCopy()
		apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ECConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: latest.Generation,
		})
		// Phase reuses InProgress during teardown (the Phase enum has no
		// dedicated Terminating value); the precise teardown state lives
		// in the Ready condition reason. deriveECPhase is not involved on
		// the deletion path, so there is no competing writer.
		latest.Status.Phase = v1alpha1.PhaseInProgress
		if reflect.DeepEqual(before, latest.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, latest)
	})
}
