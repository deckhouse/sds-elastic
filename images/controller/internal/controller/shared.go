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
	"sort"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
)

// mergeLabels returns a new map containing every key from `existing`
// overridden by every key from `desired`. Both inputs may be nil.
//
// It lives in shared.go (rather than the legacy unstructured.go) so that
// commit B22 can drop the old SdsElasticCluster controller files
// wholesale without taking this helper — which the new ElasticCluster
// reconciler also depends on — with them.
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

// parseMonEndpoints turns Rook's "<id>=<host>:<port>,..." mon-endpoints
// representation into a deduplicated, sorted []string of just the
// "<host>:<port>" parts. Empty entries and trailing whitespace are ignored.
// Duplicate endpoints (same host:port under different mon ids) collapse
// to a single entry — sds-elastic feeds the result into
// CephClusterConnection.spec.monitors, which Rook expects to be unique.
//
// Same B22-survival rationale as mergeLabels: shared by both the legacy
// SdsElasticCluster reconciler and the new ElasticCluster credentials
// stage, so it must outlive the legacy files.
func parseMonEndpoints(data string) []string {
	if data == "" {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, p := range strings.Split(data, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if i := strings.Index(p, "="); i >= 0 {
			p = p[i+1:]
		}
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// isNoMatchErr returns true when the apimachinery error indicates that
// the GVK is not registered in the cluster (the corresponding CRD has
// not been applied yet). Used by every stage that touches optional
// dependency CRDs (Rook, csi-ceph, sds-node-configurator) so that
// installing sds-elastic before its dependencies surfaces a clear
// "waiting for CRD X" condition rather than a crash-loop on
// meta.NoMatchError.
func isNoMatchErr(err error) bool {
	return apimeta.IsNoMatchError(err)
}
