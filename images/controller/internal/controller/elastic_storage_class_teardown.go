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
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/deckhouse/sds-common-lib/conditions"
	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// rookPoolDeletionBlockedConditionType is the condition Rook publishes on
// a CephBlockPool when deletion is blocked because the pool still holds
// data (and no rook.io/force-deletion annotation is present). Mirrors
// cephv1.ConditionPoolDeletionIsBlocked; duplicated as a literal so the
// controller does not take a build dependency on the Rook Go module.
const rookPoolDeletionBlockedConditionType = "PoolDeletionIsBlocked"

// ensureESCFinalizer adds ESCFinalizer via a merge-patch if absent. The
// patch only touches metadata.finalizers, so it does not bump generation.
func (r *ElasticStorageClassReconciler) ensureESCFinalizer(ctx context.Context, esc *v1alpha1.ElasticStorageClass) error {
	if controllerutil.ContainsFinalizer(esc, external.ESCFinalizer) {
		return nil
	}
	patch := client.MergeFrom(esc.DeepCopy())
	controllerutil.AddFinalizer(esc, external.ESCFinalizer)
	return r.Client.Patch(ctx, esc, patch)
}

// removeESCFinalizer drops ESCFinalizer so the API server can finish
// deleting the CR. Idempotent: a no-op when the finalizer is already gone.
func (r *ElasticStorageClassReconciler) removeESCFinalizer(ctx context.Context, esc *v1alpha1.ElasticStorageClass) error {
	if !controllerutil.ContainsFinalizer(esc, external.ESCFinalizer) {
		return nil
	}
	patch := client.MergeFrom(esc.DeepCopy())
	controllerutil.RemoveFinalizer(esc, external.ESCFinalizer)
	return r.Client.Patch(ctx, esc, patch)
}

// reconcileDeleteESC runs the destructive ElasticStorageClass teardown:
//
//  1. A non-bypassable bound-PV guard: while any PersistentVolume
//     provisioned from this StorageClass is still Bound, refuse to delete
//     anything (the operator must delete the PVCs first). The
//     force-deletion annotation never lifts this guard.
//  2. Delete the csi-ceph CephStorageClass.
//  3. Tear down the backing pool/filesystem:
//     - RBD: a non-empty pool is preserved unless the operator sets the
//     force-deletion annotation, which the controller propagates to the
//     CephBlockPool as rook.io/force-deletion (destructive purge).
//     - CephFS: there is no force path; the filesystem is reconfigured to
//     destroy-on-delete and only torn down once it is empty (the
//     operator must delete remaining PersistentVolumes first).
//  4. While anything is still terminating, translate the cause into a
//     domain-level Ready condition without leaking Rook resource names.
//  5. Once the pool/filesystem and CephStorageClass are gone, drop the
//     finalizer.
func (r *ElasticStorageClassReconciler) reconcileDeleteESC(ctx context.Context, esc *v1alpha1.ElasticStorageClass) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(esc, external.ESCFinalizer) {
		return ctrl.Result{}, nil
	}

	bound, err := r.boundPVCount(ctx, esc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if bound > 0 {
		msg := fmt.Sprintf("%d volume(s) still bound to StorageClass %q; delete the PersistentVolumeClaims first", bound, esc.Name)
		if err := r.patchESCDeleteCondition(ctx, esc, v1alpha1.ESCReasonBoundVolumesExist, msg); err != nil {
			return ctrl.Result{}, err
		}
		// Re-enqueued by the PersistentVolume watch; RequeueAfter is the
		// safety net.
		return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
	}

	cscNN := types.NamespacedName{Name: builder.ESCCephStorageClassName(esc)}
	if err := r.deleteExternalIfExistsESC(ctx, external.CephStorageClassGVK, cscNN); err != nil {
		return ctrl.Result{}, err
	}

	var (
		present bool
		reason  string
		msg     string
	)
	switch esc.Spec.Type {
	case v1alpha1.StorageClassTypeRBD:
		present, reason, msg, err = r.teardownRBD(ctx, esc)
	case v1alpha1.StorageClassTypeCephFS:
		present, reason, msg, err = r.teardownCephFS(ctx, esc)
	default:
		// Unsupported type: nothing pool-side to tear down beyond the
		// CephStorageClass deleted above.
		r.Log.Warning(fmt.Sprintf("[reconcileDeleteESC] ElasticStorageClass %q has unsupported type %q; skipping pool teardown", esc.Name, esc.Spec.Type))
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if present {
		if err := r.patchESCDeleteCondition(ctx, esc, reason, msg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
	}

	if _, cscFound, err := r.getExternalIfExistsESC(ctx, external.CephStorageClassGVK, cscNN); err != nil {
		return ctrl.Result{}, err
	} else if cscFound {
		if err := r.patchESCDeleteCondition(ctx, esc, v1alpha1.ESCReasonTerminating, "removing storage class"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
	}

	if err := r.removeESCFinalizer(ctx, esc); err != nil {
		return ctrl.Result{}, err
	}
	r.Log.Info(fmt.Sprintf("[reconcileDeleteESC] ElasticStorageClass %q teardown complete; finalizer removed", esc.Name))
	return ctrl.Result{}, nil
}

// teardownRBD purges the CephBlockPool backing an RBD ElasticStorageClass.
// A non-empty pool is preserved unless the operator authorises the
// destructive purge via ESCForceDeleteAnnotation, which the controller
// propagates to the pool as rook.io/force-deletion. Returns present=true
// (with a domain-level reason/message) while the pool CR still exists.
func (r *ElasticStorageClassReconciler) teardownRBD(ctx context.Context, esc *v1alpha1.ElasticStorageClass) (bool, string, string, error) {
	poolNN := types.NamespacedName{Namespace: r.Cfg.ControllerNamespace, Name: builder.ESCRBDPoolName(esc)}
	force := strings.EqualFold(esc.Annotations[external.ESCForceDeleteAnnotation], "true")

	if force {
		if err := r.patchExternalAnnotation(ctx, external.CephBlockPoolGVK, poolNN, external.RookForceDeletionAnnotation, "true"); err != nil {
			return false, "", "", err
		}
	}
	if err := r.deleteExternalIfExistsESC(ctx, external.CephBlockPoolGVK, poolNN); err != nil {
		return false, "", "", err
	}

	pool, found, err := r.getExternalIfExistsESC(ctx, external.CephBlockPoolGVK, poolNN)
	if err != nil {
		return false, "", "", err
	}
	if !found {
		return false, "", "", nil
	}
	if !force && unstructuredConditionTrue(pool, rookPoolDeletionBlockedConditionType) {
		msg := fmt.Sprintf(
			"storage pool still contains data; to permanently delete it (all data in the pool will be lost) run: kubectl annotate elasticstorageclass %s %s=true",
			esc.Name, external.ESCForceDeleteAnnotation,
		)
		return true, v1alpha1.ESCReasonDataPresentInPool, msg, nil
	}
	return true, v1alpha1.ESCReasonTerminating, "removing storage pool", nil
}

// teardownCephFS tears down the CephFilesystem backing a CephFS
// ElasticStorageClass. There is no force path for CephFS: the filesystem
// is reconfigured to destroy-on-delete (so an empty filesystem is actually
// removed) and only disappears once all its subvolumes are gone, which the
// operator achieves by deleting the remaining PersistentVolumes. Returns
// present=true (with a domain-level reason/message) while the CR exists.
func (r *ElasticStorageClassReconciler) teardownCephFS(ctx context.Context, esc *v1alpha1.ElasticStorageClass) (bool, string, string, error) {
	if strings.EqualFold(esc.Annotations[external.ESCForceDeleteAnnotation], "true") {
		r.Log.Warning(fmt.Sprintf("[teardownCephFS] force-deletion annotation on ElasticStorageClass %q has no effect for CephFS; delete the remaining PersistentVolumes instead", esc.Name))
	}
	fsNN := types.NamespacedName{Namespace: r.Cfg.ControllerNamespace, Name: builder.ESCCephFSName(esc)}

	if err := r.patchCephFSDestroyOnDelete(ctx, fsNN); err != nil {
		return false, "", "", err
	}
	if err := r.deleteExternalIfExistsESC(ctx, external.CephFilesystemGVK, fsNN); err != nil {
		return false, "", "", err
	}

	fs, found, err := r.getExternalIfExistsESC(ctx, external.CephFilesystemGVK, fsNN)
	if err != nil {
		return false, "", "", err
	}
	if !found {
		return false, "", "", nil
	}
	if externalDeletionBlocked(fs) {
		msg := fmt.Sprintf("filesystem still has volumes; delete the remaining PersistentVolumes for StorageClass %q first", esc.Name)
		return true, v1alpha1.ESCReasonFilesystemNotEmpty, msg, nil
	}
	return true, v1alpha1.ESCReasonTerminating, "removing storage filesystem", nil
}

// boundPVCount counts PersistentVolumes provisioned from this ESC's
// StorageClass that are still Bound. Released/Available/Failed PVs do not
// block teardown (no application still references the data).
func (r *ElasticStorageClassReconciler) boundPVCount(ctx context.Context, esc *v1alpha1.ElasticStorageClass) (int, error) {
	pvs := &corev1.PersistentVolumeList{}
	if err := r.Client.List(ctx, pvs); err != nil {
		return 0, err
	}
	scName := builder.ESCCephStorageClassName(esc)
	count := 0
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Spec.StorageClassName == scName && pv.Status.Phase == corev1.VolumeBound {
			count++
		}
	}
	return count, nil
}

// patchCephFSDestroyOnDelete flips the CephFilesystem to destroy-on-delete
// (preserveFilesystemOnDelete=false, preservePoolsOnDelete=false) so an
// empty filesystem is actually removed rather than orphaned. No-op when
// the CR is gone or already configured to destroy.
func (r *ElasticStorageClassReconciler) patchCephFSDestroyOnDelete(ctx context.Context, nn types.NamespacedName) error {
	fs, found, err := r.getExternalIfExistsESC(ctx, external.CephFilesystemGVK, nn)
	if err != nil || !found {
		return err
	}
	preserveFS, _, _ := unstructured.NestedBool(fs.Object, "spec", "preserveFilesystemOnDelete")
	preservePools, _, _ := unstructured.NestedBool(fs.Object, "spec", "preservePoolsOnDelete")
	if !preserveFS && !preservePools {
		return nil
	}
	patch := client.MergeFrom(fs.DeepCopy())
	if err := unstructured.SetNestedField(fs.Object, false, "spec", "preserveFilesystemOnDelete"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(fs.Object, false, "spec", "preservePoolsOnDelete"); err != nil {
		return err
	}
	return r.Client.Patch(ctx, fs, patch)
}

// patchExternalAnnotation sets a single annotation on an unstructured
// vendor resource via a merge-patch. No-op when the CR is gone or already
// carries the desired value.
func (r *ElasticStorageClassReconciler) patchExternalAnnotation(ctx context.Context, gvk schema.GroupVersionKind, nn types.NamespacedName, key, value string) error {
	u, found, err := r.getExternalIfExistsESC(ctx, gvk, nn)
	if err != nil || !found {
		return err
	}
	if u.GetAnnotations()[key] == value {
		return nil
	}
	patch := client.MergeFrom(u.DeepCopy())
	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[key] = value
	u.SetAnnotations(ann)
	return r.Client.Patch(ctx, u, patch)
}

// deleteExternalIfExistsESC issues a Delete for an unstructured vendor
// resource. NotFound and NoMatch (CRD absent) both count as "already
// gone" and return nil.
func (r *ElasticStorageClassReconciler) deleteExternalIfExistsESC(ctx context.Context, gvk schema.GroupVersionKind, nn types.NamespacedName) error {
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

// getExternalIfExistsESC fetches an unstructured vendor resource. Returns
// found=false for both NotFound and NoMatch (CRD absent) — see the EC
// reconciler's getExternalIfExists for the rationale on treating a missing
// CRD as "gone" so the finalizer can be released safely.
func (r *ElasticStorageClassReconciler) getExternalIfExistsESC(ctx context.Context, gvk schema.GroupVersionKind, nn types.NamespacedName) (*unstructured.Unstructured, bool, error) {
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

// patchESCDeleteCondition publishes the aggregate Ready=False teardown
// condition (with a domain-level reason/message) and pins Phase to
// InProgress. Uses RetryOnConflict and skips the write when nothing
// changed so a blocked teardown does not churn the status. A CR that has
// already been removed (NotFound) is treated as success.
func (r *ElasticStorageClassReconciler) patchESCDeleteCondition(ctx context.Context, esc *v1alpha1.ElasticStorageClass, reason, msg string) error {
	err := conditions.UpdateStatus(ctx, r.Client, esc, func(latest *v1alpha1.ElasticStorageClass) {
		if latest.Status == nil {
			latest.Status = &v1alpha1.ElasticStorageClassStatus{}
		}
		conditions.Set(&latest.Status.Conditions, metav1.Condition{
			Type:               v1alpha1.ESCConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            conditions.TruncateMessage(msg),
			ObservedGeneration: latest.Generation,
		})
		// Phase reuses InProgress during teardown (the Phase enum has no
		// dedicated Terminating value); the precise teardown state lives in
		// the Ready condition reason. deriveESCPhase is not involved on the
		// deletion path, so there is no competing writer.
		latest.Status.Phase = v1alpha1.PhaseInProgress
	})
	// A CR that has already been removed is not a failure to report on.
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
