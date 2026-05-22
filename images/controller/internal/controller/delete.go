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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// reconcileDelete performs reverse teardown in strict order:
//
//  1. CephStorageClass-es (csi-ceph)
//  2. CephClusterConnection (csi-ceph)
//  3. CephObjectStore (Rook)
//  4. CephFilesystem (Rook)
//  5. CephBlockPool (Rook)
//  6. rook-ceph-tools Deployment + CephCluster (Rook)
//  7. local PVs labelled by us
//  8. LVMLogicalVolume CRs labelled by us
//
// The function does *not* wait for downstream objects to disappear in one
// pass; it requeues until every step reports "fully deleted" so that no
// upper layer is removed before the lower one is gone.
//
// Each downstream resource is normally driven through a graceful Delete
// and the controller waits for Rook / csi-ceph to do their own cleanup
// (drain pools, wipe OSD devices, release cephx auth, ...). Foreign
// finalizers are only stripped when the operator opts in by setting
// v1alpha1.ForceDeleteAnnotation=true on the CR *and* at least
// Cfg.ForceDeleteGracePeriod has elapsed since DeletionTimestamp — see
// shouldForceDelete.
func (r *SdsElasticClusterReconciler) reconcileDelete(ctx context.Context, cluster *v1alpha1.SdsElasticCluster) (ctrl.Result, error) {
	force := r.shouldForceDelete(cluster)
	r.Log.Info(fmt.Sprintf("[reconcileDelete] tearing down SdsElasticCluster %q (force=%t)", cluster.Name, force))

	steps := []func(context.Context, *v1alpha1.SdsElasticCluster, bool) (bool, error){
		r.teardownCephStorageClasses,
		r.teardownCephClusterConnection,
		r.teardownOwned(external.CephObjectStoreGVK, r.Cfg.ControllerNamespace),
		r.teardownOwned(external.CephFilesystemGVK, r.Cfg.ControllerNamespace),
		r.teardownOwned(external.CephBlockPoolGVK, r.Cfg.ControllerNamespace),
		r.teardownCephCluster,
		r.teardownPVs,
		r.teardownLLVs,
	}

	for i, step := range steps {
		done, err := step(ctx, cluster, force)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("teardown step %d: %w", i, err)
		}
		if !done {
			return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
		}
	}

	if err := r.removeFinalizer(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// shouldForceDelete returns true only when both the operator opt-in
// (v1alpha1.ForceDeleteAnnotation="true" on the SdsElasticCluster CR)
// is present and the configured grace window since the CR's
// DeletionTimestamp has elapsed.
//
// The grace window prevents a misconfigured annotation from skipping
// Rook's graceful cleanup on a healthy cluster. The annotation prevents
// the controller from ever silently breaking the Kubernetes finalizer
// contract on the happy path — the documented recovery procedure for
// the "MONs never reached quorum / Rook cannot drain the cluster" case
// is to set the annotation and wait.
func (r *SdsElasticClusterReconciler) shouldForceDelete(cluster *v1alpha1.SdsElasticCluster) bool {
	if cluster.DeletionTimestamp == nil {
		return false
	}
	if cluster.GetAnnotations()[v1alpha1.ForceDeleteAnnotation] != "true" {
		return false
	}
	return time.Since(cluster.DeletionTimestamp.Time) >= r.Cfg.ForceDeleteGracePeriod
}

// teardownOwned returns a step that deletes every cluster- or namespace-
// scoped resource of the given GVK that we previously owned.
//
// Rook (CephCluster/Pool/Filesystem/ObjectStore) and csi-ceph
// (CephClusterConnection/CephStorageClass) put their own finalizers on
// these CRs to gate destruction on graceful, in-Ceph cleanup
// (`ceph osd pool delete`, ObjectBucket / OBC drain, OSD device wipe,
// cephx auth eviction, ...). On the happy path we wait for those
// finalizers to drop on their own; only the operator-confirmed
// force path (see shouldForceDelete) strips them.
func (r *SdsElasticClusterReconciler) teardownOwned(gvk schema.GroupVersionKind, namespace string) func(context.Context, *v1alpha1.SdsElasticCluster, bool) (bool, error) {
	return func(ctx context.Context, cluster *v1alpha1.SdsElasticCluster, force bool) (bool, error) {
		list, err := r.listOwnedUnstructured(ctx, gvk, namespace, cluster.Name)
		if err != nil {
			return false, err
		}
		if len(list.Items) == 0 {
			return true, nil
		}
		for i := range list.Items {
			item := &list.Items[i]
			if _, err := r.deleteUnstructured(ctx, gvk, item.GetNamespace(), item.GetName(), force); err != nil {
				return false, err
			}
		}
		return false, nil
	}
}

// teardownCephStorageClasses removes every CephStorageClass owned by this CR
// (cluster-scoped, namespace "").
func (r *SdsElasticClusterReconciler) teardownCephStorageClasses(ctx context.Context, cluster *v1alpha1.SdsElasticCluster, force bool) (bool, error) {
	return r.teardownOwned(external.CephStorageClassGVK, "")(ctx, cluster, force)
}

// teardownCephClusterConnection removes the CephClusterConnection CR
// configured by the user (or the default one). We do not look it up by
// label because the user may have created it by hand earlier; matching by
// name + ManagedBy label keeps that safe.
func (r *SdsElasticClusterReconciler) teardownCephClusterConnection(ctx context.Context, cluster *v1alpha1.SdsElasticCluster, force bool) (bool, error) {
	if cluster.Spec.CsiCephIntegration == nil || !cluster.Spec.CsiCephIntegration.Enabled {
		return true, nil
	}
	return r.teardownOwned(external.CephClusterConnectionGVK, "")(ctx, cluster, force)
}

// teardownCephCluster removes the rook-ceph-tools Deployment and the
// CephCluster itself. The CephCluster must always go last among Rook
// resources. The cluster parameter is unused (the CephCluster name and
// namespace are derived from controller config), but the signature is
// fixed by the teardownFunc slice in reconcileDelete.
//
// The CephCluster `cephcluster.ceph.rook.io` finalizer gates deletion
// on Rook flushing dataDirHostPath, releasing OSD devices, running the
// configured cleanupPolicy, and removing the cluster from cephx auth.
// Stripping it skips all of that, which is only acceptable when Rook
// itself cannot make progress (e.g. MONs never reached quorum) — see
// shouldForceDelete for the operator opt-in.
func (r *SdsElasticClusterReconciler) teardownCephCluster(ctx context.Context, _ *v1alpha1.SdsElasticCluster, force bool) (bool, error) {
	if err := r.deleteCephToolsDeployment(ctx); err != nil {
		return false, err
	}

	cephClusterKey := types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      builder.CephClusterName,
	}
	gone, err := r.deleteUnstructured(ctx, external.CephClusterGVK, cephClusterKey.Namespace, cephClusterKey.Name, force)
	if err != nil {
		return false, err
	}
	return gone, nil
}

func (r *SdsElasticClusterReconciler) deleteCephToolsDeployment(ctx context.Context) error {
	dep := &appsv1.Deployment{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      builder.CephToolsDeploymentName,
	}, dep)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if dep.DeletionTimestamp != nil {
		return nil
	}
	if err := r.Client.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// teardownPVs deletes every PV labelled by this controller. PVs we own
// carry no foreign finalizers, so the operator force opt-in is ignored.
func (r *SdsElasticClusterReconciler) teardownPVs(ctx context.Context, cluster *v1alpha1.SdsElasticCluster, _ bool) (bool, error) {
	pvs, err := r.listOwnedPVs(ctx, cluster.Name)
	if err != nil {
		return false, err
	}
	if len(pvs) == 0 {
		return true, nil
	}
	for i := range pvs {
		pv := &pvs[i]
		if pv.DeletionTimestamp != nil {
			continue
		}
		if err := r.Client.Delete(ctx, pv); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}

// teardownLLVs deletes every LVMLogicalVolume labelled by this controller.
// The manual-creation finalizer placed at build time is *our own* marker
// (it tells sds-node-configurator not to auto-delete the LLV) and is
// always safe to strip during teardown — that's the documented contract
// of the manual-creation flow, not a foreign finalizer like Rook's. The
// operator force opt-in is therefore not consulted here.
func (r *SdsElasticClusterReconciler) teardownLLVs(ctx context.Context, cluster *v1alpha1.SdsElasticCluster, _ bool) (bool, error) {
	list, err := r.listOwnedUnstructured(ctx, external.LVMLogicalVolumeGVK, "", cluster.Name)
	if err != nil {
		return false, err
	}
	if len(list.Items) == 0 {
		return true, nil
	}
	for i := range list.Items {
		item := &list.Items[i]
		if removed := stripFinalizer(item, external.LVMLogicalVolumeManualFinalizer); removed {
			if err := r.Client.Update(ctx, item); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
		if err := r.deleteUnstructuredIfExists(ctx, external.LVMLogicalVolumeGVK, item.GetNamespace(), item.GetName()); err != nil {
			return false, err
		}
	}
	return false, nil
}

func stripFinalizer(obj client.Object, finalizer string) bool {
	finalizers := obj.GetFinalizers()
	out := finalizers[:0]
	changed := false
	for _, f := range finalizers {
		if f == finalizer {
			changed = true
			continue
		}
		out = append(out, f)
	}
	if changed {
		obj.SetFinalizers(out)
	}
	return changed
}
