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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

var _ = Describe("ElasticCluster FSM scaffolding", func() {
	var (
		ec     = newTestElasticCluster()
		status *ecStatusBuilder
		r      *ElasticClusterReconciler
	)

	BeforeEach(func() {
		status = newECStatusBuilder(ec)
		r = newElasticClusterReconciler(newFakeClient())
	})

	Describe("advance", func() {
		It("returns true and sets Ready on success", func() {
			Expect(r.advance(status, v1alpha1.ECConditionStorageReady, true, "ok", nil)).To(BeTrue())
			c := findCondition(status.conditions, v1alpha1.ECConditionStorageReady)
			Expect(c).NotTo(BeNil())
			Expect(c.Status).To(Equal(metav1.ConditionTrue))
			Expect(c.Reason).To(Equal("Ready"))
		})

		It("gates downstream on error", func() {
			Expect(r.advance(status, v1alpha1.ECConditionStorageReady, false, "", errors.New("boom"))).To(BeFalse())
			Expect(findCondition(status.conditions, v1alpha1.ECConditionStorageReady).Reason).To(Equal("Error"))
			Expect(findCondition(status.conditions, v1alpha1.ECConditionCephClusterReady).Reason).To(Equal("WaitingForPrev"))
			Expect(findCondition(status.conditions, v1alpha1.ECConditionReady).Status).To(Equal(metav1.ConditionFalse))
		})

		It("gates downstream on in-progress", func() {
			Expect(r.advance(status, v1alpha1.ECConditionCephClusterReady, false, "waiting", nil)).To(BeFalse())
			Expect(findCondition(status.conditions, v1alpha1.ECConditionCredentialsReady).Reason).To(Equal("WaitingForPrev"))
		})
	})

	Describe("gateAfter", func() {
		It("does not touch UpgradeInProgress when gating after UpgradeReady", func() {
			gateAfter(status, v1alpha1.ECConditionUpgradeReady)
			Expect(findCondition(status.conditions, v1alpha1.ECConditionUpgradeInProgress)).To(BeNil())
		})

		It("sets UpgradeInProgress WaitingForPrev for upstream gates", func() {
			gateAfter(status, v1alpha1.ECConditionStorageReady)
			c := findCondition(status.conditions, v1alpha1.ECConditionUpgradeInProgress)
			Expect(c).NotTo(BeNil())
			Expect(c.Reason).To(Equal("WaitingForPrev"))
		})
	})

	Describe("setUpgradeInProgress", func() {
		It("records upgrading signal", func() {
			setUpgradeInProgress(status, true, "rolling")
			c := findCondition(status.conditions, v1alpha1.ECConditionUpgradeInProgress)
			Expect(c.Status).To(Equal(metav1.ConditionTrue))
			Expect(c.Reason).To(Equal("Upgrading"))
		})
	})

	Describe("isAggregateReady", func() {
		It("reads the last Ready condition", func() {
			status.setCondition(v1alpha1.ECConditionReady, metav1.ConditionFalse, "WaitingForPrev", "")
			status.setCondition(v1alpha1.ECConditionReady, metav1.ConditionTrue, "Ready", "")
			Expect(isAggregateReady(status)).To(BeTrue())
		})
	})
})
