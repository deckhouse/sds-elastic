/*
Copyright 2025 Flant JSC

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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/deckhouse/sds-elastic/api/v1alpha1"
)

// A condition type that is declared but never written is worse than one that
// does not exist: an absent condition is indistinguishable from "not yet
// evaluated", so an operator waits for a verdict that never comes and an alert
// on it never fires.
//
// The declared set, the FSM stage order and the signal conditions live in three
// different places, and nothing but these specs keeps them from drifting apart.

func conditionTypesOf(conds []metav1.Condition) []string {
	types := make([]string, 0, len(conds))
	for _, c := range conds {
		types = append(types, c.Type)
	}
	return types
}

var _ = Describe("ElasticCluster condition contract", func() {
	// UpgradeInProgress is a signal, not a stage: gateAfter deliberately leaves
	// it alone so a rolling upgrade keeps reporting True while upstream stages
	// are gated. setUpgradeInProgress is its only publisher.
	signalConditions := []string{v1alpha1.ECConditionUpgradeInProgress}

	It("declares exactly the types the reconciler is wired to write", func() {
		wired := append([]string{}, stageOrder...)
		wired = append(wired, v1alpha1.ECConditionReady)
		wired = append(wired, signalConditions...)

		Expect(v1alpha1.ECConditionTypes).To(ConsistOf(wired))
	})

	It("keeps the aggregate Ready out of the stage order", func() {
		Expect(stageOrder).NotTo(ContainElement(v1alpha1.ECConditionReady))
	})

	It("writes every declared type when the reconcile runs to the end", func() {
		r := &ElasticClusterReconciler{}
		status := newECStatusBuilder(&v1alpha1.ElasticCluster{})

		for _, stage := range stageOrder {
			Expect(r.advance(status, stage, true, "", "done", nil)).To(BeTrue(),
				"a stage that succeeded must let the FSM proceed")
		}
		setUpgradeInProgress(status, false, "no upgrade in progress")
		status.setCondition(v1alpha1.ECConditionReady, metav1.ConditionTrue, "Ready", "all stages reconciled")

		Expect(conditionTypesOf(status.conditions)).To(ConsistOf(v1alpha1.ECConditionTypes))
	})

	// The failure path is where an absent condition costs the most: the operator
	// is looking at the resource precisely because something is wrong.
	It("still writes every stage and the aggregate Ready when the first stage fails", func() {
		r := &ElasticClusterReconciler{}
		status := newECStatusBuilder(&v1alpha1.ElasticCluster{})

		Expect(r.advance(status, stageOrder[0], false, "", "", errors.New("the backend is unreachable"))).
			To(BeFalse())

		expected := append([]string{}, stageOrder...)
		expected = append(expected, v1alpha1.ECConditionReady)
		Expect(conditionTypesOf(status.conditions)).To(ConsistOf(expected))

		for _, c := range status.conditions {
			Expect(c.Status).To(Equal(metav1.ConditionFalse), "condition "+c.Type)
			Expect(c.Reason).NotTo(BeEmpty(), "condition "+c.Type+" needs a machine-readable reason")
		}
	})

	It("publishes the UpgradeInProgress signal both ways", func() {
		for _, inProgress := range []bool{true, false} {
			status := newECStatusBuilder(&v1alpha1.ElasticCluster{})
			setUpgradeInProgress(status, inProgress, "message")

			Expect(conditionTypesOf(status.conditions)).
				To(ContainElement(v1alpha1.ECConditionUpgradeInProgress))
		}
	})
})

var _ = Describe("ElasticStorageClass condition contract", func() {
	It("declares exactly the types the reconciler is wired to write", func() {
		wired := append([]string{}, escStageOrder...)
		wired = append(wired, v1alpha1.ESCConditionReady)

		Expect(v1alpha1.ESCConditionTypes).To(ConsistOf(wired))
	})

	It("keeps the aggregate Ready out of the stage order", func() {
		Expect(escStageOrder).NotTo(ContainElement(v1alpha1.ESCConditionReady))
	})

	It("writes every declared type when the reconcile runs to the end", func() {
		r := &ElasticStorageClassReconciler{}
		status := newESCStatusBuilder(&v1alpha1.ElasticStorageClass{})

		for _, stage := range escStageOrder {
			Expect(r.advanceESC(status, stage, true, "done", nil)).To(BeTrue())
		}
		status.setCondition(v1alpha1.ESCConditionReady, metav1.ConditionTrue, "Ready", "all stages reconciled")

		Expect(conditionTypesOf(status.conditions)).To(ConsistOf(v1alpha1.ESCConditionTypes))
	})

	It("still writes every stage and the aggregate Ready when the first stage fails", func() {
		r := &ElasticStorageClassReconciler{}
		status := newESCStatusBuilder(&v1alpha1.ElasticStorageClass{})

		Expect(r.advanceESC(status, escStageOrder[0], false, "", errors.New("the pool is gone"))).
			To(BeFalse())

		expected := append([]string{}, escStageOrder...)
		expected = append(expected, v1alpha1.ESCConditionReady)
		Expect(conditionTypesOf(status.conditions)).To(ConsistOf(expected))

		for _, c := range status.conditions {
			Expect(c.Status).To(Equal(metav1.ConditionFalse), "condition "+c.Type)
			Expect(c.Reason).NotTo(BeEmpty(), "condition "+c.Type+" needs a machine-readable reason")
		}
	})
})
