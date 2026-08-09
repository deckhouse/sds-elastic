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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/sds-common-lib/conditions"
	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

// stageConditions builds a full set of EC stage conditions, which is what the
// FSM always writes, so a fixture only has to state what it is about.
func stageConditions(status metav1.ConditionStatus, reason string) []metav1.Condition {
	conds := make([]metav1.Condition, 0, len(stageOrder))
	for _, t := range stageOrder {
		conds = append(conds, metav1.Condition{Type: t, Status: status, Reason: reason})
	}
	return conds
}

func setStageCondition(conds []metav1.Condition, condType string, status metav1.ConditionStatus, reason string) {
	for i := range conds {
		if conds[i].Type == condType {
			conds[i].Status, conds[i].Reason = status, reason
			return
		}
	}
}

var _ = Describe("ElasticCluster pure helpers", func() {
	DescribeTable("versionMatches",
		func(running, desired string, want bool) {
			Expect(versionMatches(running, desired)).To(Equal(want))
		},
		Entry("exact match", "v19.2.3", "v19.2.3", true),
		Entry("bare vs prefixed", "19.2.3", "v19.2.3", true),
		Entry("rook suffix", "ceph version 19.2.3 (abc)", "v19.2.3", true),
		Entry("boundary: must not match longer patch", "19.2.30", "v19.2.3", false),
		Entry("mismatch", "18.2.0", "v19.2.3", false),
	)

	DescribeTable("cephHealthOK",
		func(health string, want bool) {
			Expect(cephHealthOK(health)).To(Equal(want))
		},
		Entry("ok", "HEALTH_OK", true),
		Entry("warn", "health_warn", true),
		Entry("err", "HEALTH_ERR", false),
		Entry("empty", "", false),
	)

	Describe("deriveECPhase", func() {
		It("returns Pending when no stage conditions", func() {
			Expect(deriveECPhase(nil)).To(Equal(v1alpha1.PhasePending))
		})

		// The fixtures carry every stage, because that is what the FSM leaves
		// behind: advance gates the remaining stages whenever one does not pass,
		// so a half-reported set never reaches the API server. A phase derived
		// from an incomplete set is Pending, which is asserted separately.
		It("returns Error when any stage has Error reason", func() {
			conds := stageConditions(metav1.ConditionTrue, conditions.ReasonReconciled)
			setStageCondition(conds, v1alpha1.ECConditionStorageReady, metav1.ConditionFalse, conditions.ReasonReconcileFailed)
			setStageCondition(conds, v1alpha1.ECConditionCephClusterReady, metav1.ConditionFalse, conditions.ReasonPending)

			Expect(deriveECPhase(conds)).To(Equal(v1alpha1.PhaseError))
		})

		It("returns InProgress when a stage is False but not Error", func() {
			conds := stageConditions(metav1.ConditionTrue, conditions.ReasonReconciled)
			setStageCondition(conds, v1alpha1.ECConditionCephClusterReady, metav1.ConditionFalse, conditions.ReasonPending)

			Expect(deriveECPhase(conds)).To(Equal(v1alpha1.PhaseInProgress))
		})

		It("is Pending while any stage has no verdict", func() {
			conds := stageConditions(metav1.ConditionTrue, conditions.ReasonReconciled)[1:]

			Expect(deriveECPhase(conds)).To(Equal(v1alpha1.PhasePending))
		})

		It("returns Ready when all stage conditions are True", func() {
			conds := make([]metav1.Condition, 0, len(stageOrder))
			for _, t := range stageOrder {
				conds = append(conds, metav1.Condition{Type: t, Status: metav1.ConditionTrue})
			}
			Expect(deriveECPhase(conds)).To(Equal(v1alpha1.PhaseReady))
		})

		// Neither the aggregate nor the upgrade signal is a stage, so neither may
		// move the phase. Asserted against a full set of stage conditions: with
		// none present there is nothing for them to be ignored in favour of, and
		// the case below covers that separately.
		It("ignores aggregate Ready and UpgradeInProgress", func() {
			conds := make([]metav1.Condition, 0, len(stageOrder)+2)
			for _, t := range stageOrder {
				conds = append(conds, metav1.Condition{Type: t, Status: metav1.ConditionTrue})
			}
			conds = append(conds,
				metav1.Condition{Type: v1alpha1.ECConditionReady, Status: metav1.ConditionFalse, Reason: conditions.ReasonReconcileFailed},
				metav1.Condition{Type: v1alpha1.ECConditionUpgradeInProgress, Status: metav1.ConditionTrue},
			)
			Expect(deriveECPhase(conds)).To(Equal(v1alpha1.PhaseReady))
		})

		// A cluster carrying only those two has had no stage evaluated, and
		// answering Ready there would call it healthy on no evidence. The
		// previous implementation did, because it only guarded against an empty
		// condition list rather than against an empty set of stage verdicts.
		It("is Pending when no stage has a verdict, whatever else is set", func() {
			conds := []metav1.Condition{
				{Type: v1alpha1.ECConditionReady, Status: metav1.ConditionTrue},
				{Type: v1alpha1.ECConditionUpgradeInProgress, Status: metav1.ConditionTrue},
			}
			Expect(deriveECPhase(conds)).To(Equal(v1alpha1.PhasePending))
		})
	})
})
