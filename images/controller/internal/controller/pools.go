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

	"k8s.io/apimachinery/pkg/runtime/schema"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// ensureBlockPools applies every CephBlockPool from spec.blockPools.
// Pools that were previously owned by this CR but are no longer in the spec
// are deleted (drift correction).
func (r *SdsElasticClusterReconciler) ensureBlockPools(ctx context.Context, cluster *v1alpha1.SdsElasticCluster) (bool, string, error) {
	desiredNames := map[string]struct{}{}
	for i := range cluster.Spec.BlockPools {
		pool := &cluster.Spec.BlockPools[i]
		obj := builder.CephBlockPool(cluster, r.Cfg.ControllerNamespace, pool)
		if err := r.upsertUnstructured(ctx, obj); err != nil {
			return false, "", fmt.Errorf("upsert CephBlockPool %s: %w", pool.Name, err)
		}
		desiredNames[pool.Name] = struct{}{}
	}

	if err := r.pruneOwnedByGVK(ctx, external.CephBlockPoolGVK, cluster.Name, r.Cfg.ControllerNamespace, desiredNames); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// ensureFilesystems applies every CephFilesystem from spec.filesystems and
// prunes obsolete ones.
func (r *SdsElasticClusterReconciler) ensureFilesystems(ctx context.Context, cluster *v1alpha1.SdsElasticCluster) (bool, string, error) {
	desiredNames := map[string]struct{}{}
	for i := range cluster.Spec.Filesystems {
		fs := &cluster.Spec.Filesystems[i]
		obj := builder.CephFilesystem(cluster, r.Cfg.ControllerNamespace, fs)
		if err := r.upsertUnstructured(ctx, obj); err != nil {
			return false, "", fmt.Errorf("upsert CephFilesystem %s: %w", fs.Name, err)
		}
		desiredNames[fs.Name] = struct{}{}
	}

	if err := r.pruneOwnedByGVK(ctx, external.CephFilesystemGVK, cluster.Name, r.Cfg.ControllerNamespace, desiredNames); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// ensureObjectStores applies every CephObjectStore from spec.objectStores
// and prunes obsolete ones.
func (r *SdsElasticClusterReconciler) ensureObjectStores(ctx context.Context, cluster *v1alpha1.SdsElasticCluster) (bool, string, error) {
	desiredNames := map[string]struct{}{}
	for i := range cluster.Spec.ObjectStores {
		os := &cluster.Spec.ObjectStores[i]
		obj := builder.CephObjectStore(cluster, r.Cfg.ControllerNamespace, os)
		if err := r.upsertUnstructured(ctx, obj); err != nil {
			return false, "", fmt.Errorf("upsert CephObjectStore %s: %w", os.Name, err)
		}
		desiredNames[os.Name] = struct{}{}
	}

	if err := r.pruneOwnedByGVK(ctx, external.CephObjectStoreGVK, cluster.Name, r.Cfg.ControllerNamespace, desiredNames); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// pruneOwnedByGVK deletes objects previously owned by clusterName whose
// names are not in the desired set. Used by ensure*-methods to remove
// resources that were dropped from spec.
func (r *SdsElasticClusterReconciler) pruneOwnedByGVK(ctx context.Context, gvk schema.GroupVersionKind, clusterName, namespace string, desired map[string]struct{}) error {
	list, err := r.listOwnedUnstructured(ctx, gvk, namespace, clusterName)
	if err != nil {
		return err
	}
	for i := range list.Items {
		item := &list.Items[i]
		if _, keep := desired[item.GetName()]; keep {
			continue
		}
		if _, err := r.deleteUnstructuredIfExists(ctx, gvk, item.GetNamespace(), item.GetName()); err != nil {
			return fmt.Errorf("prune %s %s: %w", gvk.Kind, item.GetName(), err)
		}
	}
	return nil
}
