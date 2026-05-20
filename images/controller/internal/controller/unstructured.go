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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// upsertUnstructured creates the desired object if it is missing or patches
// .spec / .metadata.labels when they differ from the existing object. The
// existing .status is never touched (Rook owns it).
func (r *SdsElasticClusterReconciler) upsertUnstructured(ctx context.Context, desired *unstructured.Unstructured) error {
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

// deleteUnstructuredIfExists removes a cluster-scoped or namespaced resource
// by GVK + name (+ namespace). Returns:
//   - (true, nil)  — the object did not exist, treat as fully deleted.
//   - (false, nil) — Delete has been issued, retry later to confirm removal.
//   - (false, err) — error.
func (r *SdsElasticClusterReconciler) deleteUnstructuredIfExists(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)
	if namespace != "" {
		obj.SetNamespace(namespace)
	}

	err := r.Client.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	// If the CRD itself is not registered, there is nothing to delete and
	// nothing depends on this kind being present (csi-ceph / Rook CRDs are
	// optional from the SdsElasticCluster point of view). Treat as deleted.
	if apimeta.IsNoMatchError(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if obj.GetDeletionTimestamp() != nil {
		return false, nil
	}
	if err := r.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	return false, nil
}

// forceDeleteUnstructured issues a Delete and, if the object is stuck on
// foreign finalizers (typical Rook / csi-ceph behaviour when their owner
// controllers cannot perform graceful cleanup — e.g. MONs never reached
// quorum), strips those finalizers via merge-patch so the object is
// actually removed by the API server.
//
// This is intentionally aggressive: by the time reconcileDelete reaches a
// Rook resource the user has already requested SdsElasticCluster removal
// and accepts that downstream graceful cleanup may be skipped. Mirrors
// the OnAfterDeleteHelm hook logic for individual CR deletes.
//
// Returns the same tri-state as deleteUnstructuredIfExists.
func (r *SdsElasticClusterReconciler) forceDeleteUnstructured(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(name)
	if namespace != "" {
		obj.SetNamespace(namespace)
	}

	err := r.Client.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}

	if obj.GetDeletionTimestamp() == nil {
		if err := r.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
	}

	if len(obj.GetFinalizers()) > 0 {
		patch := client.MergeFrom(obj.DeepCopy())
		obj.SetFinalizers(nil)
		if err := r.Client.Patch(ctx, obj, patch); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("strip finalizers from %s %s/%s: %w", gvk.Kind, namespace, name, err)
		}
	}
	return false, nil
}

// listOwnedUnstructured fetches all objects of the given GVK that carry our
// ClusterOwnerLabel = cluster.Name.
func (r *SdsElasticClusterReconciler) listOwnedUnstructured(ctx context.Context, gvk schema.GroupVersionKind, namespace, clusterName string) (*unstructured.UnstructuredList, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   gvk.Group,
		Version: gvk.Version,
		Kind:    gvk.Kind + "List",
	})
	opts := []client.ListOption{
		client.MatchingLabels{external.ClusterOwnerLabel: clusterName},
	}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := r.Client.List(ctx, list, opts...); err != nil {
		// The third-party CRD (csi-ceph: CephStorageClass /
		// CephClusterConnection, Rook: CephBlockPool / CephFilesystem /
		// CephObjectStore, sds-node-configurator: LVMLogicalVolume) might
		// not be installed in the cluster — for example during teardown
		// after csi-ceph or Rook has already been removed. In that case
		// there are no objects to enumerate, so return an empty list
		// instead of failing the whole reconcile/teardown.
		if apimeta.IsNoMatchError(err) {
			return list, nil
		}
		return nil, err
	}
	return list, nil
}

func mergeLabels(existing, desired map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range desired {
		out[k] = v
	}
	return out
}
