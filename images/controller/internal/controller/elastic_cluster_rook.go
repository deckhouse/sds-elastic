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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// ensureCephCluster materialises the Rook CephCluster CR for an
// ElasticCluster. Returns true when CephCluster.status.phase==Ready (i.e.
// MONs reached quorum, OSDs are up). Until then the controller keeps
// requeuing — Rook owns the convergence loop, sds-elastic only translates
// the CR.
//
// pvcStorageRequest must equal min(BD.size) over the BDs that ensureStorage
// adopted for this EC; it is propagated into the CephCluster's
// volumeClaimTemplates[0].spec.resources.requests.storage so that every
// per-OSD PVC binds to one of the local-PVs produced by ensureStorage.
func (r *ElasticClusterReconciler) ensureCephCluster(ctx context.Context, ec *v1alpha1.ElasticCluster, osdCount int32, pvcStorageRequest resource.Quantity) (bool, string, error) {
	cephImage, err := builder.CephImage(r.Cfg.CephImages, v1alpha1.DefaultCephVersion)
	if err != nil {
		return false, "", err
	}

	placementAll := builder.ECPlacementAll(ec, nil /* tolerations TBD via module config */)
	desired := builder.ECCephCluster(ec, r.Cfg.ControllerNamespace, cephImage, osdCount, pvcStorageRequest, placementAll)
	if err := r.upsertECUnstructured(ctx, desired); err != nil {
		if isNoMatchErr(err) {
			return false, "waiting for CephCluster CRD (Rook)", nil
		}
		return false, "", fmt.Errorf("upsert CephCluster: %w", err)
	}

	cc := &unstructured.Unstructured{}
	cc.SetGroupVersionKind(external.CephClusterGVK)
	err = r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      builder.ECCephClusterName(ec),
	}, cc)
	if apierrors.IsNotFound(err) {
		return false, "CephCluster CR not yet visible", nil
	}
	if err != nil {
		return false, "", err
	}

	phase, _, _ := unstructured.NestedString(cc.Object, "status", "phase")
	if phase != "Ready" {
		return false, fmt.Sprintf("CephCluster phase=%q (waiting for Ready)", phase), nil
	}
	return true, fmt.Sprintf("CephCluster %s is Ready", cc.GetName()), nil
}
