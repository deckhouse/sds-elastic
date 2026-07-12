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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

var _ = Describe("ElasticStorageClass FSM and Reconcile", func() {
	var (
		escName = "pool-demo"
		ctx     = context.Background()
	)

	Describe("deriveESCPhase", func() {
		It("returns Pending for empty conditions", func() {
			Expect(deriveESCPhase(nil)).To(Equal(v1alpha1.PhasePending))
		})

		It("returns Error when a stage has Error reason", func() {
			conds := []metav1.Condition{
				{Type: v1alpha1.ESCConditionPoolReady, Status: metav1.ConditionFalse, Reason: "Error"},
			}
			Expect(deriveESCPhase(conds)).To(Equal(v1alpha1.PhaseError))
		})

		It("returns Ready when all stages are True", func() {
			conds := []metav1.Condition{
				{Type: v1alpha1.ESCConditionPoolReady, Status: metav1.ConditionTrue},
				{Type: v1alpha1.ESCConditionCsiStorageClassReady, Status: metav1.ConditionTrue},
			}
			Expect(deriveESCPhase(conds)).To(Equal(v1alpha1.PhaseReady))
		})
	})

	Describe("ecCephClusterReadyState", func() {
		It("returns empty for nil EC or missing status", func() {
			Expect(ecCephClusterReadyState(nil)).To(BeEmpty())
			Expect(ecCephClusterReadyState(&v1alpha1.ElasticCluster{})).To(BeEmpty())
		})

		It("returns True status when condition is set", func() {
			ec := ecWithCephClusterReady(newTestElasticCluster())
			Expect(ecCephClusterReadyState(ec)).To(Equal(string(metav1.ConditionTrue)))
		})
	})

	Describe("advanceESC and gateAfterESC", func() {
		var (
			esc    = newTestElasticStorageClass(escName, v1alpha1.StorageClassTypeRBD)
			status *escStatusBuilder
			r      *ElasticStorageClassReconciler
		)

		BeforeEach(func() {
			status = newESCStatusBuilder(esc)
			r = newElasticStorageClassReconciler(newFakeClient())
		})

		It("gates CsiStorageClassReady on pool error", func() {
			Expect(r.advanceESC(status, v1alpha1.ESCConditionPoolReady, false, "", errors.New("fail"))).To(BeFalse())
			Expect(findCondition(status.conditions, v1alpha1.ESCConditionCsiStorageClassReady).Reason).
				To(Equal("WaitingForPrev"))
		})
	})

	Describe("Reconcile", func() {
		It("waits for parent EC CephClusterReady", func() {
			esc := newTestElasticStorageClass(escName, v1alpha1.StorageClassTypeRBD)
			ec := newTestElasticCluster()
			cl := newFakeClient(esc, ec)
			r := newElasticStorageClassReconciler(cl)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: escName}})
			Expect(err).NotTo(HaveOccurred())

			latest := &v1alpha1.ElasticStorageClass{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: escName}, latest)).To(Succeed())
			Expect(findCondition(latest.Status.Conditions, v1alpha1.ESCConditionPoolReady).Reason).
				To(Equal("InProgress"))
			Expect(findCondition(latest.Status.Conditions, v1alpha1.ESCConditionCsiStorageClassReady).Reason).
				To(Equal("WaitingForPrev"))
		})

		It("reaches Ready when pool and csi-ceph SC are Created", func() {
			esc := newTestElasticStorageClass(escName, v1alpha1.StorageClassTypeRBD)
			ec := ecWithCephClusterReady(newTestElasticCluster())
			cl := newFakeClient(
				esc,
				ec,
				newCephBlockPoolUnstructured(escName),
				newCephStorageClassUnstructured(escName),
			)
			r := newElasticStorageClassReconciler(cl)

			result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: escName}})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			latest := &v1alpha1.ElasticStorageClass{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: escName}, latest)).To(Succeed())
			Expect(latest.Status.Phase).To(Equal(v1alpha1.PhaseReady))
			Expect(findCondition(latest.Status.Conditions, v1alpha1.ESCConditionReady).Status).
				To(Equal(metav1.ConditionTrue))
		})
	})
})
