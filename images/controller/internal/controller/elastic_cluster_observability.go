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
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// maxHealthChecks bounds the number of health-check entries surfaced on
// EC.status.health.checks so a misbehaving cluster cannot grow the status
// past the etcd object size limit. A sensible UI shows only the top few
// anyway.
const maxHealthChecks = 20

// populateObservability is a best-effort enrichment of EC.status with
// observability data: Ceph health, capacity, and per-daemon version
// histograms (osd/mon/mgr). It is invoked from reconcileNormal AFTER
// the FSM stages have run, so the status published in this reconcile
// contains both the FSM verdict and the latest ground-truth surface.
//
// All data is sourced from a single Get on CephCluster — the controller
// intentionally does not List rook-ceph-{osd,mon,mgr} Pods. Trade-off:
// "running"/"byNode" granularity is gone, but RBAC is simpler (no
// cluster-wide pods/list/watch) and there is one fewer Watch in the
// cache. Operators can still see Pod-level state via
// `kubectl get pod -l app=rook-ceph-osd`.
//
// Errors are intentionally swallowed and logged: stale observability is
// strictly better than aborting a reconcile that already converged the
// FSM. A future tick will re-fetch.
//
// `desiredOSDs` comes from ensureStorage's view of the world (selected
// BlockDevice count). It is published even when CephCluster has not yet
// been created so the UI can render "Provisioning N OSDs" right after
// the user creates an EC, without waiting for Rook to settle.
func (r *ElasticClusterReconciler) populateObservability(
	ctx context.Context,
	ec *v1alpha1.ElasticCluster,
	sb *ecStatusBuilder,
	desiredOSDs int32,
) {
	cc, ok := r.fetchCephCluster(ctx, ec)
	if !ok {
		// CephCluster missing / CRD not installed — still surface the
		// desired-OSD count so the UI does not lose that data point
		// during the first few reconciles after EC creation.
		sb.osds = osdSummaryFromDesired(desiredOSDs)
		return
	}

	sb.health = parseCephHealth(cc)
	sb.capacity = parseCephCapacity(cc)

	cephObj, _, _ := unstructured.NestedMap(cc.Object, "status", "ceph")
	osdKnown, osdByVersion := parseCephDaemonsByVersion(cephObj, "osd")
	monKnown, monByVersion := parseCephDaemonsByVersion(cephObj, "mon")
	mgrKnown, mgrByVersion := parseCephDaemonsByVersion(cephObj, "mgr")

	if desiredOSDs > 0 || osdKnown > 0 {
		sb.osds = &v1alpha1.OSDStatus{
			Desired:     desiredOSDs,
			KnownToCeph: osdKnown,
			ByVersion:   osdByVersion,
		}
	}
	if monKnown > 0 {
		sb.mons = &v1alpha1.DaemonStatus{KnownToCeph: monKnown, ByVersion: monByVersion}
	}
	if mgrKnown > 0 {
		sb.mgrs = &v1alpha1.DaemonStatus{KnownToCeph: mgrKnown, ByVersion: mgrByVersion}
	}
}

// fetchCephCluster returns the Rook CephCluster owned by this
// ElasticCluster (or false when missing / CRD not installed). All errors
// are folded into "not ok" — the caller is best-effort observability.
func (r *ElasticClusterReconciler) fetchCephCluster(
	ctx context.Context, ec *v1alpha1.ElasticCluster,
) (*unstructured.Unstructured, bool) {
	cc := &unstructured.Unstructured{}
	cc.SetGroupVersionKind(external.CephClusterGVK)
	err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      builder.ECCephClusterName(ec),
	}, cc)
	switch {
	case err == nil:
		return cc, true
	case apierrors.IsNotFound(err) || isNoMatchErr(err):
		return nil, false
	default:
		r.Log.Warning(fmt.Sprintf("[populateObservability] get CephCluster: %s", err))
		return nil, false
	}
}

// parseCephHealth lifts CephCluster.status.ceph into the typed
// CephHealthStatus published on EC. Returns nil when CephCluster has
// not yet emitted a health probe so the EC.status field stays empty
// (instead of HEALTH_OK with zeroed timestamps, which would falsely
// imply a green cluster).
func parseCephHealth(cc *unstructured.Unstructured) *v1alpha1.CephHealthStatus {
	cephObj, found, err := unstructured.NestedMap(cc.Object, "status", "ceph")
	if err != nil || !found || len(cephObj) == 0 {
		return nil
	}

	healthStr, _, _ := unstructured.NestedString(cc.Object, "status", "ceph", "health")
	message, _, _ := unstructured.NestedString(cc.Object, "status", "ceph", "message")
	lastCheckedStr, _, _ := unstructured.NestedString(cc.Object, "status", "ceph", "lastChecked")

	out := &v1alpha1.CephHealthStatus{
		Status:      healthStr,
		Message:     message,
		LastChecked: parseRFC3339(lastCheckedStr),
	}

	// status.ceph.details is a map keyed by check name, with each value
	// being a {message, severity} object. Rook's schema:
	//   details:
	//     MON_DOWN:
	//       message: "1/3 mons down, quorum a,c"
	//       severity: HEALTH_WARN
	if details, ok := cephObj["details"].(map[string]interface{}); ok {
		out.Checks = make([]v1alpha1.CephHealthCheck, 0, len(details))
		for name, raw := range details {
			detail, _ := raw.(map[string]interface{})
			msg, _ := detail["message"].(string)
			sev, _ := detail["severity"].(string)
			out.Checks = append(out.Checks, v1alpha1.CephHealthCheck{
				Name:     name,
				Severity: sev,
				Message:  msg,
			})
		}
		// Stable order: HEALTH_ERR first, then HEALTH_WARN, then by name
		// — keeps the most actionable check on top in the UI.
		sort.SliceStable(out.Checks, func(i, j int) bool {
			a, b := out.Checks[i], out.Checks[j]
			if a.Severity != b.Severity {
				return checkSeverityRank(a.Severity) < checkSeverityRank(b.Severity)
			}
			return a.Name < b.Name
		})
		if len(out.Checks) > maxHealthChecks {
			out.Checks = out.Checks[:maxHealthChecks]
		}
	}

	if out.Status == "" && out.Message == "" && len(out.Checks) == 0 && out.LastChecked == nil {
		return nil
	}
	return out
}

// parseCephCapacity lifts CephCluster.status.ceph.capacity into the
// typed CephCapacityStatus, exposing the raw byte counters as
// resource.Quantity (BinarySI) so they serialize as "500Gi" / "1.2Ti"
// — UI-friendly without extra formatting. Empty / partially-populated
// capacity blocks (no bytesTotal yet) are treated as missing.
func parseCephCapacity(cc *unstructured.Unstructured) *v1alpha1.CephCapacityStatus {
	capObj, found, err := unstructured.NestedMap(cc.Object, "status", "ceph", "capacity")
	if err != nil || !found || len(capObj) == 0 {
		return nil
	}
	total := nestedInt64(capObj, "bytesTotal")
	used := nestedInt64(capObj, "bytesUsed")
	avail := nestedInt64(capObj, "bytesAvailable")
	if total == 0 && used == 0 && avail == 0 {
		return nil
	}
	lastUpdatedStr, _ := capObj["lastUpdated"].(string)
	out := &v1alpha1.CephCapacityStatus{
		Total:       *resource.NewQuantity(total, resource.BinarySI),
		Used:        *resource.NewQuantity(used, resource.BinarySI),
		Available:   *resource.NewQuantity(avail, resource.BinarySI),
		LastUpdated: parseRFC3339(lastUpdatedStr),
	}
	if total > 0 {
		out.UsedPercent = fmt.Sprintf("%.2f", float64(used)*100.0/float64(total))
	}
	return out
}

// parseCephDaemonsByVersion folds CephCluster.status.ceph.versions.<kind>
// — a flat map[versionString]count — into (sum, sortedSlice). Output
// slice is sorted by count desc, then version asc for stable diffs;
// zero/negative counts are dropped. Returns (0, nil) when the kind is
// absent or empty so the caller can decide whether to publish a daemon
// status block at all.
//
// Rook's shape (since at least v1.10):
//
//	status:
//	  ceph:
//	    versions:
//	      mon: { "ceph version 19.2.3 ...": 3 }
//	      mgr: { "ceph version 19.2.3 ...": 1 }
//	      osd: { "ceph version 19.2.3 ...": 6, "ceph version 18.2.0 ...": 3 }
//	      overall: { ... }   # ignored — we expose per-kind only
func parseCephDaemonsByVersion(cephObj map[string]interface{}, kind string) (int32, []v1alpha1.DaemonVersionCount) {
	versions, ok := cephObj["versions"].(map[string]interface{})
	if !ok || len(versions) == 0 {
		return 0, nil
	}
	kindMap, ok := versions[kind].(map[string]interface{})
	if !ok || len(kindMap) == 0 {
		return 0, nil
	}

	out := make([]v1alpha1.DaemonVersionCount, 0, len(kindMap))
	var total int32
	for version, raw := range kindMap {
		count := int32(0)
		switch v := raw.(type) {
		case int64:
			count = int32(v)
		case int:
			count = int32(v)
		case float64:
			count = int32(v)
		}
		if count <= 0 {
			continue
		}
		out = append(out, v1alpha1.DaemonVersionCount{Version: version, Count: count})
		total += count
	}
	if len(out) == 0 {
		return 0, nil
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Version < out[j].Version
	})
	return total, out
}

// osdSummaryFromDesired is used when CephCluster is not reachable yet:
// still surface the desired count so the UI does not lose the "we asked
// Rook for N OSDs" data point during the first few reconciles after EC
// creation.
func osdSummaryFromDesired(desired int32) *v1alpha1.OSDStatus {
	if desired == 0 {
		return nil
	}
	return &v1alpha1.OSDStatus{Desired: desired}
}

// checkSeverityRank orders Ceph health severities so the UI sees the
// most urgent checks first. Unknown severities sort last to keep them
// visible but below well-known categories.
func checkSeverityRank(severity string) int {
	switch severity {
	case "HEALTH_ERR":
		return 0
	case "HEALTH_WARN":
		return 1
	default:
		return 2
	}
}

// parseRFC3339 returns *metav1.Time for a non-empty RFC3339 string, or
// nil for missing / unparseable input. Rook always emits timestamps in
// RFC3339, but tests may inject empty strings.
func parseRFC3339(s string) *metav1.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	mt := metav1.NewTime(t)
	return &mt
}

// nestedInt64 returns the int64 value at path, normalising the float64
// values that come out of unstructured.Unstructured (which goes through
// json.Unmarshal-into-interface{}). Missing keys → 0.
func nestedInt64(m map[string]interface{}, key string) int64 {
	switch v := m[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}
