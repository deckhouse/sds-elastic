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

// cephUpgradeProbe is the result of inspecting a CephCluster and
// determining where it is in a rolling upgrade.
//
// Done and InProgress are not strict negations: while the pre-upgrade
// health gate is blocking the bump (cluster is not HEALTH_OK / WARN),
// both are False — Rook will not start rolling pods until the operator
// fixes the gate, but we are also not "running the desired version" yet.
type cephUpgradeProbe struct {
	// Running is the value to publish on EC.status.cephVersion.running.
	// While daemons disagree it is the LAGGING (oldest) version present
	// in `status.ceph.versions.overall`, so the printcolumn faithfully
	// shows what callers will still hit if they connect to a random
	// daemon. Falls back to `status.version.version` (Rook's target
	// marker) when the per-kind histogram is empty.
	Running string

	// Done is true when the desired image is applied AND every daemon
	// has converged on the desired version (single key in
	// versions.overall matching desired).
	Done bool

	// InProgress is true while Rook is rolling pods OR while a queued
	// upgrade is waiting for the next reconcile to apply the image
	// bump. Stays True throughout the mon → mgr → osd → mds rolling
	// window even when CephCluster.status.phase=Progressing causes the
	// FSM to gate the EC reconciler upstream of UpgradeReady.
	InProgress bool

	// Msg is the human-readable explanation surfaced on the
	// UpgradeReady / UpgradeInProgress conditions.
	Msg string
}

// ensureUpgrade implements the rolling-upgrade FSM stage.
//
//  1. Compute desired Ceph version from the controller config (driven by
//     the module image in production). For MVP this is
//     v1alpha1.DefaultCephVersion.
//  2. Probe the CephCluster via probeCephUpgradeState and translate the
//     result into the (done, inProgress, msg) tuple the FSM expects.
//  3. Pre-upgrade health gate: if a switch is requested and the cluster is
//     not HEALTH_OK / HEALTH_WARN, the probe surfaces (Done=false,
//     InProgress=false) so the operator notices before Rook is bumped.
//     The actual bump happens in ensureCephCluster on the next reconcile,
//     which is intentionally separated to keep the stages single-purpose.
//
// The probe also runs (and is published) from ensureCephCluster directly
// so the UpgradeInProgress signal stays accurate even when the FSM gates
// upstream (typically while CephCluster.status.phase=Progressing during
// the rollout itself). ensureUpgrade therefore re-reads the same state;
// the redundancy is intentional — both code paths are cheap reads from
// the cache and the second one drives the explicit UpgradeReady gate.
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
		return false, false, "storage backend not yet visible", nil
	}
	if err != nil {
		r.Log.Error(err, "[ensureUpgrade] failed to get CephCluster")
		return false, false, "", errProvisionStorageBackend
	}

	probe := probeCephUpgradeState(cc, desiredImage, desiredVersion)
	applyUpgradeProbeToStatus(status, desiredVersion, probe)
	return probe.Done, probe.InProgress, probe.Msg, nil
}

// applyUpgradeProbeToStatus publishes probe results onto the EC status
// builder. Centralised so ensureCephCluster and ensureUpgrade write the
// same shape — divergence between the two paths showed up as a stale
// `Ceph` printcolumn during rolling upgrades before the helper existed.
func applyUpgradeProbeToStatus(status *ecStatusBuilder, desiredVersion string, probe cephUpgradeProbe) {
	if status.cephVersion == nil {
		status.cephVersion = &v1alpha1.CephVersionStatus{}
	}
	status.cephVersion.Requested = desiredVersion
	status.cephVersion.Running = probe.Running
}

// probeCephUpgradeState inspects a CephCluster object and returns where
// it sits in a rolling upgrade. Pure function: no side effects, no
// API calls — the caller fetches `cc` once and the result can be safely
// reused across `ensureCephCluster` (publishes the signal early so the
// FSM gate cannot clobber it) and `ensureUpgrade` (gates UpgradeReady).
//
// Convergence priority:
//
//  1. spec.cephVersion.image vs desiredImage — if they differ, an
//     image bump is queued. We surface InProgress=true unless the
//     pre-upgrade health gate (cluster not HEALTH_OK / HEALTH_WARN)
//     blocks the bump, in which case both flags are false so the
//     operator fixes the gate before Rook even starts rolling pods.
//
//  2. status.ceph.versions.overall — Rook's per-daemon histogram. The
//     ground truth for "every daemon is on version X": exactly one key,
//     and that key matches desired. Multi-key (mixed cluster) means a
//     rollout is in flight regardless of what status.phase says, since
//     Rook updates status.version.version very early (it's "what version
//     I am targeting", not "every daemon converged").
//
//  3. status.version.version — fallback for clusters whose Rook has not
//     yet populated `versions.overall` (rare; observed only on freshly
//     bootstrapped clusters between cluster creation and the first
//     status probe). Preserves the legacy behaviour for those edges.
func probeCephUpgradeState(cc *unstructured.Unstructured, desiredImage, desiredVersion string) cephUpgradeProbe {
	currentImage, _, _ := unstructured.NestedString(cc.Object, "spec", "cephVersion", "image")
	cephHealth, _, _ := unstructured.NestedString(cc.Object, "status", "ceph", "health")
	runningFromVersionField, _, _ := unstructured.NestedString(cc.Object, "status", "version", "version")

	overall := overallVersions(cc)

	probe := cephUpgradeProbe{
		Running: pickRunningVersion(overall, runningFromVersionField),
	}

	if currentImage != desiredImage {
		if !cephHealthOK(cephHealth) {
			probe.Msg = fmt.Sprintf(
				"pre-upgrade gate: cluster health=%q (need HEALTH_OK before bumping to %s)",
				cephHealth, desiredVersion,
			)
			return probe
		}
		probe.InProgress = true
		probe.Msg = fmt.Sprintf("upgrade to %s queued; rolling update will start", desiredVersion)
		return probe
	}

	if len(overall) == 0 {
		// versions.overall has never been populated — fall back to
		// Rook's status.version.version. Same semantics the controller
		// has had since the first MVP, kept for bootstrap edge cases.
		if runningFromVersionField == "" {
			probe.InProgress = true
			probe.Msg = "no running version reported yet"
			return probe
		}
		if !versionMatches(runningFromVersionField, desiredVersion) {
			probe.InProgress = true
			probe.Msg = fmt.Sprintf("rolling update in progress: running=%s desired=%s", shortenCephVersion(runningFromVersionField), desiredVersion)
			return probe
		}
		probe.Done = true
		probe.Msg = fmt.Sprintf("running version %s", shortenCephVersion(runningFromVersionField))
		return probe
	}

	if len(overall) > 1 {
		probe.InProgress = true
		probe.Msg = fmt.Sprintf("rolling update in progress: %s desired=%s", formatVersionsHistogram(overall), desiredVersion)
		return probe
	}

	// Exactly one entry in `versions.overall` — the cluster is
	// (transiently) homogeneous. Done iff it matches desired.
	var onlyKey string
	for k := range overall {
		onlyKey = k
	}
	if !versionMatches(onlyKey, desiredVersion) {
		probe.InProgress = true
		probe.Msg = fmt.Sprintf("rolling update in progress: running=%s desired=%s", shortenCephVersion(onlyKey), desiredVersion)
		return probe
	}
	probe.Done = true
	probe.Msg = fmt.Sprintf("running version %s", shortenCephVersion(onlyKey))
	return probe
}

// overallVersions extracts CephCluster.status.ceph.versions.overall as
// a map[versionString]count. Returns nil when the path is absent or
// empty so the caller can branch on len() == 0.
//
// Counts are converted to int64 with the same float64/int/int64 fan-in
// as parseCephDaemonsByVersion to tolerate Rook's wire format
// (numbers come through unstructured as float64).
func overallVersions(cc *unstructured.Unstructured) map[string]int64 {
	cephObj, ok, _ := unstructured.NestedMap(cc.Object, "status", "ceph")
	if !ok || cephObj == nil {
		return nil
	}
	versions, ok := cephObj["versions"].(map[string]interface{})
	if !ok || len(versions) == 0 {
		return nil
	}
	overall, ok := versions["overall"].(map[string]interface{})
	if !ok || len(overall) == 0 {
		return nil
	}
	out := make(map[string]int64, len(overall))
	for ver, raw := range overall {
		var count int64
		switch v := raw.(type) {
		case int64:
			count = v
		case int:
			count = int64(v)
		case float64:
			count = int64(v)
		}
		if count <= 0 {
			continue
		}
		out[ver] = count
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pickRunningVersion returns the version string to surface on
// EC.status.cephVersion.running. While daemons disagree the lagging
// (oldest) version is published — the printcolumn intentionally tracks
// what callers still hit on the slowest-rolling daemon (typically
// OSDs), not Rook's already-bumped target marker.
//
// "Oldest" is determined lexicographically over the full version
// strings Rook publishes ("ceph version 19.2.3 (...)" sorts before
// "ceph version 20.2.1 (...)"). Lexicographic ordering is correct as
// long as the major/minor segments stay zero-padded relative to each
// other, which is true for every Ceph release in the supported range.
//
// Falls back to runningFromVersionField for bootstrap clusters whose
// versions.overall has not yet been populated.
func pickRunningVersion(overall map[string]int64, runningFromVersionField string) string {
	if len(overall) == 0 {
		return runningFromVersionField
	}
	var oldest string
	for k := range overall {
		if oldest == "" || k < oldest {
			oldest = k
		}
	}
	return oldest
}

// formatVersionsHistogram renders a versions.overall map into a stable,
// human-readable summary for the UpgradeInProgress condition message.
// Output example: `19.2.3 4 → 20.2.1 5`. Sorted by version asc so the
// oldest (lagging) version appears first.
func formatVersionsHistogram(overall map[string]int64) string {
	keys := make([]string, 0, len(overall))
	for k := range overall {
		keys = append(keys, k)
	}
	// Simple insertion sort over the small (1-3 element) slice — avoids
	// pulling in sort just for this.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", shortenCephVersion(k), overall[k]))
	}
	return strings.Join(parts, " \u2192 ")
}

// shortenCephVersion strips the "ceph version " prefix and the
// "(<sha>) <codename> ..." tail so messages stay readable. Tolerates
// inputs that already lack the prefix (Rook sometimes publishes bare
// "X.Y.Z-N" on `status.version.version`).
func shortenCephVersion(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "ceph version ")
	if idx := strings.Index(s, " "); idx > 0 {
		s = s[:idx]
	}
	return s
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
