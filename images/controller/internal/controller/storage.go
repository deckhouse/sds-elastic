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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
)

// ensureStorage materialises the OSD storage backing:
//   - in LVM mode it creates one LVMLogicalVolume + local PV per node;
//   - in Devices mode there is nothing to do at the K8s API layer.
//
// Returns (done, message, error). done=false means the stage made progress
// but is not finished yet (e.g. waiting for sds-node-configurator to bring
// LLVs Ready). The function never blocks on Ready: it relies on
// reconcile-on-event from controller-runtime watches.
func (r *SdsElasticClusterReconciler) ensureStorage(ctx context.Context, cluster *v1alpha1.SdsElasticCluster) (bool, string, error) {
	if cluster.Spec.Storage.LVM == nil {
		return true, "raw devices mode, Rook consumes /dev/* directly", nil
	}

	lvm := cluster.Spec.Storage.LVM
	for i := int32(0); i < lvm.NodeCount; i++ {
		llv := builder.LVMLogicalVolume(cluster, i)
		if err := r.upsertUnstructured(ctx, llv); err != nil {
			return false, "", fmt.Errorf("upsert LVMLogicalVolume %s: %w", llv.GetName(), err)
		}

		pv := builder.OSDPersistentVolume(cluster, i, r.Cfg.OSDStorageClassName)
		if err := r.upsertPV(ctx, pv); err != nil {
			return false, "", fmt.Errorf("upsert PV %s: %w", pv.Name, err)
		}
	}

	return true, "LLV and PVs are provisioned", nil
}

// upsertPV creates or updates a PersistentVolume. We compare only the
// stable parts of the spec (capacity, accessModes, reclaimPolicy, volume
// source, node affinity) — once a PV is Bound, K8s will reject most spec
// changes anyway.
func (r *SdsElasticClusterReconciler) upsertPV(ctx context.Context, desired *corev1.PersistentVolume) error {
	existing := &corev1.PersistentVolume{}
	err := r.Client.Get(ctx, types.NamespacedName{Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	patched := false
	if !equality.Semantic.DeepEqual(existing.Spec.NodeAffinity, desired.Spec.NodeAffinity) {
		existing.Spec.NodeAffinity = desired.Spec.NodeAffinity
		patched = true
	}
	mergedLabels := mergeLabels(existing.Labels, desired.Labels)
	if !equality.Semantic.DeepEqual(existing.Labels, mergedLabels) {
		existing.Labels = mergedLabels
		patched = true
	}

	if !patched {
		return nil
	}
	return r.Client.Update(ctx, existing)
}

// listOwnedPVs returns every PV labelled as owned by this CR.
func (r *SdsElasticClusterReconciler) listOwnedPVs(ctx context.Context, clusterName string) ([]corev1.PersistentVolume, error) {
	list := &corev1.PersistentVolumeList{}
	if err := r.Client.List(ctx, list, client.MatchingLabels{
		"storage.deckhouse.io/sds-elastic-cluster": clusterName,
	}); err != nil {
		return nil, err
	}
	return list.Items, nil
}
