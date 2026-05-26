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

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

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

		It("returns Error when any stage has Error reason", func() {
			conds := []metav1.Condition{
				{Type: v1alpha1.ECConditionStorageReady, Status: metav1.ConditionFalse, Reason: "Error"},
				{Type: v1alpha1.ECConditionCephClusterReady, Status: metav1.ConditionFalse, Reason: "InProgress"},
			}
			Expect(deriveECPhase(conds)).To(Equal(v1alpha1.PhaseError))
		})

		It("returns InProgress when a stage is False but not Error", func() {
			conds := []metav1.Condition{
				{Type: v1alpha1.ECConditionStorageReady, Status: metav1.ConditionTrue},
				{Type: v1alpha1.ECConditionCephClusterReady, Status: metav1.ConditionFalse, Reason: "InProgress"},
			}
			Expect(deriveECPhase(conds)).To(Equal(v1alpha1.PhaseInProgress))
		})

		It("returns Ready when all stage conditions are True", func() {
			conds := make([]metav1.Condition, 0, len(stageOrder))
			for _, t := range stageOrder {
				conds = append(conds, metav1.Condition{Type: t, Status: metav1.ConditionTrue})
			}
			Expect(deriveECPhase(conds)).To(Equal(v1alpha1.PhaseReady))
		})

		It("ignores aggregate Ready and UpgradeInProgress", func() {
			conds := []metav1.Condition{
				{Type: v1alpha1.ECConditionReady, Status: metav1.ConditionTrue},
				{Type: v1alpha1.ECConditionUpgradeInProgress, Status: metav1.ConditionTrue},
			}
			Expect(deriveECPhase(conds)).To(Equal(v1alpha1.PhaseReady))
		})
	})
})
