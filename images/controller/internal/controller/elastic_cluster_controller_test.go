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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

var _ = Describe("ElasticClusterReconciler.Reconcile", func() {
	var ctx = context.Background()

	Context("empty cluster without BlockDevices", func() {
		It("gates downstream stages and leaves aggregate Ready=False", func() {
			ec := newTestElasticCluster()
			cl := newFakeClient(ec, newTestNode("node-a"))
			r := newElasticClusterReconciler(cl)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}})
			Expect(err).NotTo(HaveOccurred())

			latest := &v1alpha1.ElasticCluster{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())
			Expect(latest.Status.Phase).To(Equal(v1alpha1.PhaseInProgress))

			Expect(findCondition(latest.Status.Conditions, v1alpha1.ECConditionStorageReady).Status).
				To(Equal(metav1.ConditionFalse))
			Expect(findCondition(latest.Status.Conditions, v1alpha1.ECConditionCephClusterReady).Reason).
				To(Equal("WaitingForPrev"))
			Expect(findCondition(latest.Status.Conditions, v1alpha1.ECConditionReady).Status).
				To(Equal(metav1.ConditionFalse))
		})
	})

	Context("fully seeded cluster", func() {
		It("reaches Ready=True only after the sequential LVG → LLV → PV chain converges", func() {
			ec := newTestElasticCluster()
			cephImage := newTestCfg().CephImages[v1alpha1.DefaultCephVersion]

			cl := newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-a", "node-a", "100Gi", true, nil),
				newRookMonSecret("fsid-abc", "admin-key", "mon-key"),
				newRookMonEndpointsCM("a=10.0.0.1:6789", "a"),
				newCephClusterUnstructured(ec, "Ready", "v19.2.3", cephImage),
				&v1alpha1.ElasticClusterCredential{
					ObjectMeta: metav1.ObjectMeta{Name: testECName},
					Spec:       v1alpha1.ElasticClusterCredentialSpec{AdminSecret: "admin-key"},
				},
				newCephClusterConnectionUnstructured(testECName, "Created"),
			)
			r := newElasticClusterReconciler(cl)

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}}
			latest := &v1alpha1.ElasticCluster{}

			// Reconcile #1 — only LVG CRs created; gate is WaitingForLVG.
			result, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).NotTo(BeZero(),
				"non-ready cluster must keep requeueing until phases converge")
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())
			Expect(findCondition(latest.Status.Conditions, v1alpha1.ECConditionStorageReady).Reason).
				To(Equal(storageReasonWaitingForLVG))
			expectAbsent(ctx, cl, external.LVMLogicalVolumeGVK, ec)
			expectAbsentPVs(ctx, cl, ec)

			// SNC reports the host VG ready — Reconcile #2 creates LLVs.
			markLVGsReady(ctx, cl, ec)
			result, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).NotTo(BeZero())
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())
			Expect(findCondition(latest.Status.Conditions, v1alpha1.ECConditionStorageReady).Reason).
				To(Equal(storageReasonWaitingForLLV))
			expectAbsentPVs(ctx, cl, ec)

			// SNC reports the LV created — Reconcile #3 creates PVs.
			markLLVsCreated(ctx, cl, ec)
			result, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).NotTo(BeZero())
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())
			Expect(findCondition(latest.Status.Conditions, v1alpha1.ECConditionStorageReady).Reason).
				To(Equal(storageReasonWaitingForPV))

			// PV binder flips the local PVs to Available — Reconcile #4
			// drains every stage and reaches Ready=True.
			markPVsAvailable(ctx, cl, ec)
			result, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())
			Expect(latest.Status.Phase).To(Equal(v1alpha1.PhaseReady))
			Expect(findCondition(latest.Status.Conditions, v1alpha1.ECConditionReady).Status).
				To(Equal(metav1.ConditionTrue))
			Expect(latest.Status.CephFSID).To(Equal("fsid-abc"))
		})
	})
})
