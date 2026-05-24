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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// upsertECUnstructured creates the desired object if it is missing or
// patches .spec / .metadata.labels when they differ from the existing
// object. The existing .status is never touched (Rook/csi-ceph/SNC own it).
//
// B-N3 in the backlog tracks the upserter-interface refactor that will
// collapse this method and ElasticStorageClassReconciler.upsertESCUnstructured
// into a shared helper.
func (r *ElasticClusterReconciler) upsertECUnstructured(ctx context.Context, desired *unstructured.Unstructured) error {
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

// schemaListKind returns the matching List GVK for an item GVK
// (e.g. BlockDevice -> BlockDeviceList) so unstructured.UnstructuredList
// can be filled by client.List.
func schemaListKind(gvk schema.GroupVersionKind) schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   gvk.Group,
		Version: gvk.Version,
		Kind:    gvk.Kind + "List",
	}
}
