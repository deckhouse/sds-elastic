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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// ensurePool is the PoolReady stage. It first gates on the parent EC
// reaching CephClusterReady=True (otherwise creating a CephBlockPool /
// CephFilesystem would fail to schedule against a non-existent
// CephCluster), then upserts the Rook pool CR and waits for
// status.phase==Ready.
func (r *ElasticStorageClassReconciler) ensurePool(ctx context.Context, esc *v1alpha1.ElasticStorageClass) (bool, string, error) {
	_, ready, msg, err := r.getReadyEC(ctx, esc)
	if err != nil {
		return false, "", err
	}
	if !ready {
		return false, msg, nil
	}

	switch esc.Spec.Type {
	case v1alpha1.StorageClassTypeRBD:
		return r.ensureRBDPool(ctx, esc)
	case v1alpha1.StorageClassTypeCephFS:
		return r.ensureCephFS(ctx, esc)
	default:
		return false, "", fmt.Errorf("unsupported ElasticStorageClass type %q", esc.Spec.Type)
	}
}

// getReadyEC fetches the parent EC and returns whether it has reached
// CephClusterReady=True. Missing EC is surfaced as an InProgress
// condition rather than a hard error so the operator sees "waiting for
// EC <name>" instead of a noisy reconcile failure.
func (r *ElasticStorageClassReconciler) getReadyEC(ctx context.Context, esc *v1alpha1.ElasticStorageClass) (*v1alpha1.ElasticCluster, bool, string, error) {
	ec := &v1alpha1.ElasticCluster{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: esc.Spec.ClusterRef}, ec)
	if apierrors.IsNotFound(err) {
		return nil, false, fmt.Sprintf("waiting for ElasticCluster %q", esc.Spec.ClusterRef), nil
	}
	if err != nil {
		return nil, false, "", err
	}
	if ec.Status == nil {
		return ec, false, fmt.Sprintf("ElasticCluster %q has no status yet", ec.Name), nil
	}
	cond := apimeta.FindStatusCondition(ec.Status.Conditions, v1alpha1.ECConditionCephClusterReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return ec, false, fmt.Sprintf("waiting for ElasticCluster %q to reach CephClusterReady=True", ec.Name), nil
	}
	return ec, true, "", nil
}

func (r *ElasticStorageClassReconciler) ensureRBDPool(ctx context.Context, esc *v1alpha1.ElasticStorageClass) (bool, string, error) {
	desired, err := builder.ESCCephBlockPool(esc, r.Cfg.ControllerNamespace)
	if err != nil {
		return false, "", err
	}
	if err := r.upsertESCUnstructured(ctx, desired); err != nil {
		if isNoMatchErr(err) {
			return false, "waiting for CephBlockPool CRD (Rook)", nil
		}
		return false, "", fmt.Errorf("upsert CephBlockPool: %w", err)
	}

	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(external.CephBlockPoolGVK)
	err = r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      builder.ESCRBDPoolName(esc),
	}, pool)
	if apierrors.IsNotFound(err) {
		return false, "CephBlockPool not yet visible", nil
	}
	if err != nil {
		return false, "", err
	}
	phase, _, _ := unstructured.NestedString(pool.Object, "status", "phase")
	if phase != "Ready" {
		return false, fmt.Sprintf("CephBlockPool phase=%q (waiting for Ready)", phase), nil
	}
	return true, fmt.Sprintf("CephBlockPool %s is Ready", pool.GetName()), nil
}

func (r *ElasticStorageClassReconciler) ensureCephFS(ctx context.Context, esc *v1alpha1.ElasticStorageClass) (bool, string, error) {
	desired, err := builder.ESCCephFilesystem(esc, r.Cfg.ControllerNamespace)
	if err != nil {
		return false, "", err
	}
	if err := r.upsertESCUnstructured(ctx, desired); err != nil {
		if isNoMatchErr(err) {
			return false, "waiting for CephFilesystem CRD (Rook)", nil
		}
		return false, "", fmt.Errorf("upsert CephFilesystem: %w", err)
	}

	fs := &unstructured.Unstructured{}
	fs.SetGroupVersionKind(external.CephFilesystemGVK)
	err = r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      builder.ESCCephFSName(esc),
	}, fs)
	if apierrors.IsNotFound(err) {
		return false, "CephFilesystem not yet visible", nil
	}
	if err != nil {
		return false, "", err
	}
	phase, _, _ := unstructured.NestedString(fs.Object, "status", "phase")
	if phase != "Ready" {
		return false, fmt.Sprintf("CephFilesystem phase=%q (waiting for Ready)", phase), nil
	}
	return true, fmt.Sprintf("CephFilesystem %s is Ready", fs.GetName()), nil
}

// ensureCsiStorageClass is the CsiStorageClassReady stage. Connection
// name is the EC name (1:1 with the CephClusterConnection produced by
// the EC reconciler).
func (r *ElasticStorageClassReconciler) ensureCsiStorageClass(ctx context.Context, esc *v1alpha1.ElasticStorageClass) (bool, string, error) {
	desired, err := builder.ESCCephStorageClass(esc, esc.Spec.ClusterRef)
	if err != nil {
		return false, "", err
	}
	if err := r.upsertESCUnstructured(ctx, desired); err != nil {
		if isNoMatchErr(err) {
			return false, "waiting for CephStorageClass CRD (csi-ceph)", nil
		}
		return false, "", fmt.Errorf("upsert CephStorageClass: %w", err)
	}

	sc := &unstructured.Unstructured{}
	sc.SetGroupVersionKind(external.CephStorageClassGVK)
	err = r.Client.Get(ctx, types.NamespacedName{Name: desired.GetName()}, sc)
	if apierrors.IsNotFound(err) {
		return false, "CephStorageClass not yet visible", nil
	}
	if err != nil {
		return false, "", err
	}
	phase, _, _ := unstructured.NestedString(sc.Object, "status", "phase")
	if phase != "Created" {
		return false, fmt.Sprintf("CephStorageClass phase=%q (waiting for Created)", phase), nil
	}
	return true, fmt.Sprintf("CephStorageClass %s is Created", sc.GetName()), nil
}

// upsertESCUnstructured is a near-duplicate of upsertECUnstructured; the
// duplication is intentional so commit B22 can drop the legacy
// SdsElasticCluster controller without leaving dead receivers behind.
// B-N3 tracks the upserter-interface refactor that collapses both into a
// shared helper.
func (r *ElasticStorageClassReconciler) upsertESCUnstructured(ctx context.Context, desired *unstructured.Unstructured) error {
	gvk := desired.GroupVersionKind()
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)

	key := types.NamespacedName{Name: desired.GetName(), Namespace: desired.GetNamespace()}
	if err := r.Client.Get(ctx, key, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return r.Client.Create(ctx, desired)
		}
		return fmt.Errorf("get %s %s/%s: %w", gvk.Kind, desired.GetNamespace(), desired.GetName(), err)
	}

	patched := false
	desiredSpec, _, _ := unstructured.NestedFieldCopy(desired.Object, "spec")
	existingSpec, _, _ := unstructured.NestedFieldCopy(existing.Object, "spec")
	if !reflect.DeepEqual(desiredSpec, existingSpec) {
		if desiredSpec == nil {
			unstructured.RemoveNestedField(existing.Object, "spec")
		} else {
			existing.Object["spec"] = desiredSpec
		}
		patched = true
	}

	desiredLabels := desired.GetLabels()
	if desiredLabels != nil {
		merged := mergeLabels(existing.GetLabels(), desiredLabels)
		if !reflect.DeepEqual(existing.GetLabels(), merged) {
			existing.SetLabels(merged)
			patched = true
		}
	}

	if patched {
		return r.Client.Update(ctx, existing)
	}
	return nil
}
