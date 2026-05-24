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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

// ECPlacementAll converts ElasticCluster.spec.storage.NodeSelector plus
// module-level tolerations into the Rook spec.placement.all map. The result
// is plugged into ECCephCluster() and applied to every Ceph role
// (mon/mgr/osd/mds), matching the expected demo topology where the same
// pool of storage nodes runs every daemon.
//
// When both nodeSelector and tolerations are empty, returns nil so the
// caller can omit the placement key entirely.
func ECPlacementAll(ec *v1alpha1.ElasticCluster, tolerations []corev1.Toleration) map[string]interface{} {
	if ec == nil {
		return nil
	}
	out := map[string]interface{}{}

	if sel := ec.Spec.Storage.NodeSelector; sel != nil {
		na := nodeAffinityFromLabelSelector(sel)
		if na != nil {
			out["nodeAffinity"] = toUnstructured(na)
		}
	}

	if len(tolerations) > 0 {
		tolList := make([]interface{}, 0, len(tolerations))
		for i := range tolerations {
			tolList = append(tolList, toUnstructured(&tolerations[i]))
		}
		out["tolerations"] = tolList
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// nodeAffinityFromLabelSelector translates a metav1.LabelSelector into the
// equivalent corev1.NodeAffinity (RequiredDuringSchedulingIgnoredDuringExecution).
// matchLabels are merged with matchExpressions; an empty selector returns nil.
func nodeAffinityFromLabelSelector(sel *metav1.LabelSelector) *corev1.NodeAffinity {
	if sel == nil {
		return nil
	}
	req := make([]corev1.NodeSelectorRequirement, 0,
		len(sel.MatchLabels)+len(sel.MatchExpressions))

	for k, v := range sel.MatchLabels {
		req = append(req, corev1.NodeSelectorRequirement{
			Key:      k,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{v},
		})
	}
	for _, me := range sel.MatchExpressions {
		req = append(req, corev1.NodeSelectorRequirement{
			Key:      me.Key,
			Operator: corev1.NodeSelectorOperator(me.Operator),
			Values:   append([]string(nil), me.Values...),
		})
	}
	if len(req) == 0 {
		return nil
	}
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: req},
			},
		},
	}
}
