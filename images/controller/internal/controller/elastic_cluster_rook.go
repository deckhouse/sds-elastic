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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
// Returns the effective CephTopologyStatus alongside the FSM result so
// the caller can persist the (mon, mgr) high-water-mark on EC.status.
// Topology is computed before the CephCluster upsert and applied
// regardless of whether the upsert succeeds — recording an intent the
// next reconcile can pick up is preferable to silently dropping it.
//
// Also returns a *cephUpgradeProbe whenever the CephCluster object has
// been fetched. The probe drives the UpgradeInProgress signal, which
// must stay accurate even when status.phase=Progressing causes the FSM
// to gate the EC reconciler upstream of UpgradeReady (a Rook rolling
// upgrade flips status.phase=Progressing for tens of minutes — exactly
// the period during which UPGRADING=True is interesting). Returning the
// probe alongside the FSM result lets reconcileNormal publish it before
// gateAfter clobbers the condition.
//
// pvcStorageRequest must equal min(BD.size) over the BDs that ensureStorage
// adopted for this EC; it is propagated into the CephCluster's
// volumeClaimTemplates[0].spec.resources.requests.storage so that every
// per-OSD PVC binds to one of the local-PVs produced by ensureStorage.
func (r *ElasticClusterReconciler) ensureCephCluster(ctx context.Context, ec *v1alpha1.ElasticCluster, osdCount int32, pvcStorageRequest resource.Quantity) (bool, string, *v1alpha1.CephTopologyStatus, *cephUpgradeProbe, error) {
	monCount, mgrCount, reason, err := r.computeCephTopology(ctx, ec)
	if err != nil {
		return false, "", nil, nil, fmt.Errorf("compute Ceph topology: %w", err)
	}
	topology := buildCephTopologyStatus(ec, monCount, mgrCount, reason)

	desiredVersion := v1alpha1.DefaultCephVersion
	cephImage, err := builder.CephImage(r.Cfg.CephImages, desiredVersion)
	if err != nil {
		return false, "", topology, nil, err
	}

	placementAll := builder.ECPlacementAll(ec, nil /* tolerations TBD via module config */)
	desired := builder.ECCephCluster(ec, r.Cfg.ControllerNamespace, cephImage, osdCount, pvcStorageRequest, placementAll, monCount, mgrCount)
	if err := r.upsertECUnstructured(ctx, desired); err != nil {
		if isNoMatchErr(err) {
			return false, "waiting for storage backend CRD", topology, nil, nil
		}
		// Keep the Rook resource detail in the logs only; the EC
		// condition message stays domain-level (no Rook/Ceph CR names).
		r.Log.Error(err, "[ensureCephCluster] failed to upsert CephCluster")
		return false, "", topology, nil, errProvisionStorageBackend
	}

	cc := &unstructured.Unstructured{}
	cc.SetGroupVersionKind(external.CephClusterGVK)
	err = r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      builder.ECCephClusterName(ec),
	}, cc)
	if apierrors.IsNotFound(err) {
		return false, "storage backend not yet visible", topology, nil, nil
	}
	if err != nil {
		r.Log.Error(err, "[ensureCephCluster] failed to get CephCluster")
		return false, "", topology, nil, errProvisionStorageBackend
	}

	probe := probeCephUpgradeState(cc, cephImage, desiredVersion)

	phase, _, _ := unstructured.NestedString(cc.Object, "status", "phase")
	if phase != "Ready" {
		return false, fmt.Sprintf("storage backend not ready (phase=%q)", phase), topology, &probe, nil
	}
	return true, "storage backend is ready", topology, &probe, nil
}

// buildCephTopologyStatus assembles the CephTopologyStatus value the
// controller will publish on EC.status. The LastPromotedAt timestamp
// follows two simple rules:
//
//   - Stamp a fresh `now` when at least one count is being raised
//     compared to the previously-recorded values AND the new value
//     crosses above the standard-profile baseline (defaultMon/MgrCount).
//   - Otherwise preserve the previously-recorded timestamp (which is
//     nil if the cluster has never left the standard profile).
//
// As a result, a non-nil LastPromotedAt is a binary "the cluster is at
// or above the high-availability profile" marker for the UI, with the
// timestamp pointing at when that state was first reached. Standard-
// profile reconciles always produce a nil LastPromotedAt.
//
// The function does NOT lower counts back — that contract belongs to
// computeCephTopology.
func buildCephTopologyStatus(ec *v1alpha1.ElasticCluster, monCount, mgrCount int32, reason string) *v1alpha1.CephTopologyStatus {
	out := &v1alpha1.CephTopologyStatus{
		MonCount: monCount,
		MgrCount: mgrCount,
		Reason:   reason,
	}
	var (
		prevMon, prevMgr int32
		prevPromotedAt   *metav1.Time
	)
	if ec.Status != nil && ec.Status.CephTopology != nil {
		prevMon = ec.Status.CephTopology.MonCount
		prevMgr = ec.Status.CephTopology.MgrCount
		prevPromotedAt = ec.Status.CephTopology.LastPromotedAt
	}
	out.LastPromotedAt = prevPromotedAt

	if (monCount > prevMon || mgrCount > prevMgr) &&
		(monCount > defaultMonCount || mgrCount > defaultMgrCount) {
		now := metav1.Now()
		out.LastPromotedAt = &now
	}
	return out
}
