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

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

// Default Ceph daemon counts when the standard fault-tolerance profile
// is sufficient: 3 mons (quorum 2, survives 1 host failure) and 2 mgrs
// (active + standby). Values match the Rook-recommended baseline for a
// 3-node Ceph cluster, which is also the minimum useful sds-elastic
// deployment.
const (
	defaultMonCount int32 = 3
	defaultMgrCount int32 = 2

	// High-availability profile counts. Selected so the mon plane
	// survives the same two simultaneous host failures the data plane
	// is sized for once a HighRedundancy ESC enters the picture: 5
	// mons give quorum 3, tolerating 2 losses, and 3 mgrs give one
	// active + two standby (no critical Ceph subsystem stalls when
	// the active mgr is one of the failed hosts).
	highMonCount int32 = 5
	highMgrCount int32 = 3
)

// computeCephTopology returns the effective (mon, mgr) counts the
// controller should ask Rook to apply for `ec`'s CephCluster, plus a
// machine-readable `reason` summarising why those numbers were chosen.
// The caller is expected to pass the values through builder.ECCephCluster
// and to persist them on EC.status.cephTopology so the next reconcile
// can preserve the high-water-mark.
//
// Decision tree:
//
//  1. Defaults: (3, 2), reason="Standard".
//  2. If at least one ElasticStorageClass references this EC by
//     spec.clusterRef AND has spec.replication == HighRedundancy, the
//     desired counts are raised to (5, 3) with
//     reason="HighRedundancyESCPresent".
//  3. The previously-recorded values on ec.Status.CephTopology act as a
//     monotonic floor: if either is higher than what step 2 produced,
//     the recorded value wins and reason becomes "StickyHighWaterMark".
//
// The trichotomy means promotion happens automatically the moment an
// operator creates a HighRedundancy ESC, but a subsequent deletion of
// that ESC does NOT silently demote the mon plane — the only way back
// to (3, 2) is for the operator to explicitly clear EC.status.cephTopology
// (a status-subresource UPDATE), which is documented in USAGE.md as an
// expert-only operation.
func (r *ElasticClusterReconciler) computeCephTopology(
	ctx context.Context, ec *v1alpha1.ElasticCluster,
) (monCount, mgrCount int32, reason string, err error) {
	monCount, mgrCount = defaultMonCount, defaultMgrCount
	reason = v1alpha1.CephTopologyReasonStandard

	list := &v1alpha1.ElasticStorageClassList{}
	if err := r.Client.List(ctx, list); err != nil {
		return 0, 0, "", fmt.Errorf("list ElasticStorageClasses: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.ClusterRef != ec.Name {
			continue
		}
		if list.Items[i].Spec.Replication == v1alpha1.ReplicationHighRedundancy {
			monCount, mgrCount = highMonCount, highMgrCount
			reason = v1alpha1.CephTopologyReasonHighRedundancyESCPresent
			break
		}
	}

	// Sticky high-water-mark: never lower the counts below what was
	// already recorded. Using element-wise max so a future profile
	// can independently raise mon or mgr without losing the other.
	if ec.Status != nil && ec.Status.CephTopology != nil {
		recorded := ec.Status.CephTopology
		stickier := false
		if recorded.MonCount > monCount {
			monCount = recorded.MonCount
			stickier = true
		}
		if recorded.MgrCount > mgrCount {
			mgrCount = recorded.MgrCount
			stickier = true
		}
		if stickier {
			reason = v1alpha1.CephTopologyReasonStickyHighWaterMark
		}
	}

	return monCount, mgrCount, reason, nil
}
