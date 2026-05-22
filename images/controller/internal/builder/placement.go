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

package builder

import (
	"encoding/json"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

// buildPlacement converts the typed PlacementSpec from the SdsElasticCluster CR
// into Rook's unstructured spec.placement map. Roles with an empty entry are
// dropped. The result is nil-safe: returns nil when no placement is requested
// so the Rook spec key is omitted.
func buildPlacement(p *v1alpha1.PlacementSpec) map[string]interface{} {
	if p == nil {
		return nil
	}
	out := map[string]interface{}{}
	addPlacementEntry(out, "all", p.All)
	addPlacementEntry(out, "mon", p.Mon)
	addPlacementEntry(out, "mgr", p.Mgr)
	addPlacementEntry(out, "osd", p.OSD)
	addPlacementEntry(out, "mds", p.MDS)
	addPlacementEntry(out, "rgw", p.RGW)
	addPlacementEntry(out, "cleanup", p.Cleanup)
	addPlacementEntry(out, "arbiter", p.Arbiter)
	if len(out) == 0 {
		return nil
	}
	return out
}

func addPlacementEntry(out map[string]interface{}, role string, e *v1alpha1.PlacementEntry) {
	if e == nil {
		return
	}
	entry := map[string]interface{}{}
	if e.NodeAffinity != nil {
		entry["nodeAffinity"] = toUnstructured(e.NodeAffinity)
	}
	if e.PodAffinity != nil {
		entry["podAffinity"] = toUnstructured(e.PodAffinity)
	}
	if e.PodAntiAffinity != nil {
		entry["podAntiAffinity"] = toUnstructured(e.PodAntiAffinity)
	}
	if len(e.Tolerations) > 0 {
		toleration := make([]interface{}, 0, len(e.Tolerations))
		for i := range e.Tolerations {
			toleration = append(toleration, toUnstructured(&e.Tolerations[i]))
		}
		entry["tolerations"] = toleration
	}
	if len(e.TopologySpreadConstraints) > 0 {
		spread := make([]interface{}, 0, len(e.TopologySpreadConstraints))
		for i := range e.TopologySpreadConstraints {
			spread = append(spread, toUnstructured(&e.TopologySpreadConstraints[i]))
		}
		entry["topologySpreadConstraints"] = spread
	}
	if len(entry) == 0 {
		return
	}
	out[role] = entry
}

// toUnstructured converts a typed K8s object into the JSON-friendly
// map[string]interface{} / []interface{} tree that unstructured.Unstructured
// (and therefore Rook's CRD client-side validation) expects. Marshal+unmarshal
// is the canonical round-trip used by controller-runtime.
//
// NOTE: keep the input parameter as a pointer to avoid copying potentially
// large affinity structs.
func toUnstructured(v interface{}) interface{} {
	// We swallow Marshal errors deliberately: the inputs come from typed
	// corev1.* values that always round-trip through JSON, and the alternative
	// (returning an error from buildPlacement) would force every caller in the
	// reconcile path to handle "impossible" failures. If a panic is ever
	// observed here, the CRD types involved are the bug.
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}
