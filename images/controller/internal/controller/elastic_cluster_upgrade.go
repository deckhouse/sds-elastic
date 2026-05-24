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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// ensureUpgrade implements the rolling-upgrade FSM stage for the demo:
//
//  1. Compute desired Ceph version from the controller config (driven by the
//     module image in production). For MVP this is v1alpha1.DefaultCephVersion.
//  2. Compare the value Rook reports under CephCluster.status.version.version
//     against the desired version. If they match, UpgradeReady=True.
//  3. If the version differs, UpgradeInProgress=True is set and Rook is
//     allowed to continue rolling. The next reconcile re-checks.
//  4. Pre-upgrade health gate: if a switch is requested and the cluster is
//     not HEALTH_OK / HEALTH_WARN, surface the gate via UpgradeReady=False
//     reason="PreUpgradeGate" so an operator notices before Rook is bumped.
//     The actual bump happens in ensureCephCluster on the next reconcile,
//     which is intentionally separated to keep the stages single-purpose.
//
// Returns (done, inProgress, msg, err).
func (r *ElasticClusterReconciler) ensureUpgrade(ctx context.Context, ec *v1alpha1.ElasticCluster, status *ecStatusBuilder) (bool, bool, string, error) {
	desiredVersion := v1alpha1.DefaultCephVersion
	desiredImage, err := builder.CephImage(r.Cfg.CephImages, desiredVersion)
	if err != nil {
		return false, false, "", err
	}

	cc := &unstructured.Unstructured{}
	cc.SetGroupVersionKind(external.CephClusterGVK)
	err = r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      builder.ECCephClusterName(ec),
	}, cc)
	if apierrors.IsNotFound(err) {
		return false, false, "CephCluster not yet visible", nil
	}
	if err != nil {
		return false, false, "", err
	}

	currentImage, _, _ := unstructured.NestedString(cc.Object, "spec", "cephVersion", "image")
	// Rook publishes the running ceph version under status.version.version
	// (status.version.image carries the resolved image). status.ceph.* only
	// contains health/lastChecked/capacity/...; reading runningVersion from
	// status.ceph.version always yielded "" and pinned UpgradeReady=False.
	runningVersion, _, _ := unstructured.NestedString(cc.Object, "status", "version", "version")
	cephHealth, _, _ := unstructured.NestedString(cc.Object, "status", "ceph", "health")

	if status.cephVersion == nil {
		status.cephVersion = &v1alpha1.CephVersionStatus{}
	}
	status.cephVersion.Requested = desiredVersion
	status.cephVersion.Running = runningVersion

	if currentImage != desiredImage {
		if !cephHealthOK(cephHealth) {
			return false, false,
				fmt.Sprintf("pre-upgrade gate: cluster health=%q (need HEALTH_OK before bumping to %s)", cephHealth, desiredVersion),
				nil
		}
		return false, true,
			fmt.Sprintf("upgrade to %s queued; Rook will roll pods", desiredVersion),
			nil
	}

	if runningVersion == "" {
		return false, true, "Rook reports no running ceph version yet", nil
	}
	if !versionMatches(runningVersion, desiredVersion) {
		return false, true,
			fmt.Sprintf("Rook rolling pods: running=%s desired=%s", runningVersion, desiredVersion),
			nil
	}
	return true, false, fmt.Sprintf("running ceph version %s", runningVersion), nil
}

func cephHealthOK(h string) bool {
	switch strings.ToUpper(strings.TrimSpace(h)) {
	case "HEALTH_OK", "HEALTH_WARN":
		return true
	default:
		return false
	}
}

// versionMatches accepts either a bare "vX.Y.Z" string or Rook's
// "X.Y.Z-N" / "ceph version X.Y.Z (...)" formatting and returns true
// when both refer to the same release.
//
// strings.Contains alone is not safe for a pure substring match: it
// would falsely accept "19.2.30" when desired is "v19.2.3". The check
// below requires that the desired version is followed in the running
// string either by end-of-string or by a non-digit character (".",
// "-", " ", "(", etc.), which Rook always emits.
func versionMatches(running, desired string) bool {
	r := strings.TrimSpace(running)
	d := strings.TrimSpace(desired)
	if r == d {
		return true
	}
	d = strings.TrimPrefix(d, "v")
	idx := strings.Index(r, d)
	if idx < 0 {
		return false
	}
	tail := r[idx+len(d):]
	if tail == "" {
		return true
	}
	switch tail[0] {
	case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return false
	}
	return true
}
