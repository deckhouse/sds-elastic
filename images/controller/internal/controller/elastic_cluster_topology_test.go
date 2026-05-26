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

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

func newTestESC(name, clusterRef string, repl v1alpha1.ReplicationMode) *v1alpha1.ElasticStorageClass {
	return &v1alpha1.ElasticStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ElasticStorageClassSpec{
			ClusterRef:  clusterRef,
			Type:        v1alpha1.StorageClassTypeRBD,
			Replication: repl,
		},
	}
}

var _ = Describe("computeCephTopology", func() {
	ctx := context.Background()
	ec := func() *v1alpha1.ElasticCluster { return newTestElasticCluster() }

	It("returns the standard profile (3, 2) when no ESC exists", func() {
		cl := newFakeClient(ec())
		r := newElasticClusterReconciler(cl)

		mon, mgr, reason, err := r.computeCephTopology(ctx, ec())
		Expect(err).NotTo(HaveOccurred())
		Expect(mon).To(Equal(int32(3)))
		Expect(mgr).To(Equal(int32(2)))
		Expect(reason).To(Equal(v1alpha1.CephTopologyReasonStandard))
	})

	It("keeps standard profile when only non-HighRedundancy ESCs exist", func() {
		cl := newFakeClient(
			ec(),
			newTestESC("rbd-prod", testECName, v1alpha1.ReplicationConsistencyAndAvailability),
			newTestESC("rbd-cheap", testECName, v1alpha1.ReplicationAvailabilityWithoutConsistency),
		)
		r := newElasticClusterReconciler(cl)

		mon, mgr, reason, err := r.computeCephTopology(ctx, ec())
		Expect(err).NotTo(HaveOccurred())
		Expect(mon).To(Equal(int32(3)))
		Expect(mgr).To(Equal(int32(2)))
		Expect(reason).To(Equal(v1alpha1.CephTopologyReasonStandard))
	})

	It("promotes to (5, 3) when at least one HighRedundancy ESC references the EC", func() {
		cl := newFakeClient(
			ec(),
			newTestESC("rbd-prod", testECName, v1alpha1.ReplicationConsistencyAndAvailability),
			newTestESC("rbd-hr", testECName, v1alpha1.ReplicationHighRedundancy),
		)
		r := newElasticClusterReconciler(cl)

		mon, mgr, reason, err := r.computeCephTopology(ctx, ec())
		Expect(err).NotTo(HaveOccurred())
		Expect(mon).To(Equal(int32(5)))
		Expect(mgr).To(Equal(int32(3)))
		Expect(reason).To(Equal(v1alpha1.CephTopologyReasonHighRedundancyESCPresent))
	})

	It("ignores HighRedundancy ESCs that reference a different EC", func() {
		cl := newFakeClient(
			ec(),
			newTestESC("rbd-hr-foreign", "some-other-ec", v1alpha1.ReplicationHighRedundancy),
		)
		r := newElasticClusterReconciler(cl)

		mon, mgr, reason, err := r.computeCephTopology(ctx, ec())
		Expect(err).NotTo(HaveOccurred())
		Expect(mon).To(Equal(int32(3)))
		Expect(mgr).To(Equal(int32(2)))
		Expect(reason).To(Equal(v1alpha1.CephTopologyReasonStandard))
	})

	It("preserves the high-water-mark when the HighRedundancy ESC has been removed", func() {
		// EC.status records a previous promotion; no live HR ESC.
		ecWithHWM := ec()
		ecWithHWM.Status = &v1alpha1.ElasticClusterStatus{
			CephTopology: &v1alpha1.CephTopologyStatus{MonCount: 5, MgrCount: 3},
		}
		cl := newFakeClient(ecWithHWM)
		r := newElasticClusterReconciler(cl)

		mon, mgr, reason, err := r.computeCephTopology(ctx, ecWithHWM)
		Expect(err).NotTo(HaveOccurred())
		Expect(mon).To(Equal(int32(5)),
			"sticky high-water-mark must keep mon.count above the live ESC inventory's demand")
		Expect(mgr).To(Equal(int32(3)))
		Expect(reason).To(Equal(v1alpha1.CephTopologyReasonStickyHighWaterMark))
	})

	It("reports HighRedundancyESCPresent (not Sticky) when the live ESC matches the recorded counts", func() {
		ecWithHWM := ec()
		ecWithHWM.Status = &v1alpha1.ElasticClusterStatus{
			CephTopology: &v1alpha1.CephTopologyStatus{MonCount: 5, MgrCount: 3},
		}
		cl := newFakeClient(
			ecWithHWM,
			newTestESC("rbd-hr", testECName, v1alpha1.ReplicationHighRedundancy),
		)
		r := newElasticClusterReconciler(cl)

		mon, mgr, reason, err := r.computeCephTopology(ctx, ecWithHWM)
		Expect(err).NotTo(HaveOccurred())
		Expect(mon).To(Equal(int32(5)))
		Expect(mgr).To(Equal(int32(3)))
		Expect(reason).To(Equal(v1alpha1.CephTopologyReasonHighRedundancyESCPresent),
			"when the live ESC inventory and the recorded counts agree, prefer the more specific reason")
	})

	It("never lowers below recorded values even when only one count is above standard", func() {
		// Hypothetical asymmetric high-water-mark (e.g. someone manually
		// raised mon to 5 but mgr stayed at 2). Live ESCs do not demand
		// a HighRedundancy promotion. Expectation: mon stays sticky at 5,
		// mgr stays at the standard 2.
		ecWithHWM := ec()
		ecWithHWM.Status = &v1alpha1.ElasticClusterStatus{
			CephTopology: &v1alpha1.CephTopologyStatus{MonCount: 5, MgrCount: 2},
		}
		cl := newFakeClient(ecWithHWM)
		r := newElasticClusterReconciler(cl)

		mon, mgr, reason, err := r.computeCephTopology(ctx, ecWithHWM)
		Expect(err).NotTo(HaveOccurred())
		Expect(mon).To(Equal(int32(5)))
		Expect(mgr).To(Equal(int32(2)))
		Expect(reason).To(Equal(v1alpha1.CephTopologyReasonStickyHighWaterMark))
	})
})

var _ = Describe("buildCephTopologyStatus", func() {
	ec := func() *v1alpha1.ElasticCluster { return newTestElasticCluster() }

	It("does NOT stamp LastPromotedAt for a first reconcile that lands at the standard profile", func() {
		out := buildCephTopologyStatus(ec(), 3, 2, v1alpha1.CephTopologyReasonStandard)
		Expect(out.MonCount).To(Equal(int32(3)))
		Expect(out.MgrCount).To(Equal(int32(2)))
		Expect(out.LastPromotedAt).To(BeNil())
	})

	It("stamps LastPromotedAt when the first reconcile lands directly at (5, 3)", func() {
		out := buildCephTopologyStatus(ec(), 5, 3, v1alpha1.CephTopologyReasonHighRedundancyESCPresent)
		Expect(out.LastPromotedAt).NotTo(BeNil())
	})

	It("stamps a fresh LastPromotedAt when previous counts are below new counts AND new is above defaults", func() {
		old := metav1.NewTime(metav1.Now().Add(-1))
		ecWithPrev := ec()
		ecWithPrev.Status = &v1alpha1.ElasticClusterStatus{
			CephTopology: &v1alpha1.CephTopologyStatus{MonCount: 3, MgrCount: 2, LastPromotedAt: &old},
		}
		out := buildCephTopologyStatus(ecWithPrev, 5, 3, v1alpha1.CephTopologyReasonHighRedundancyESCPresent)
		Expect(out.LastPromotedAt).NotTo(BeNil())
		Expect(out.LastPromotedAt.UnixNano()).To(BeNumerically(">", old.UnixNano()),
			"a promotion must update LastPromotedAt to a fresh timestamp")
	})

	It("preserves the previous LastPromotedAt when counts are unchanged", func() {
		old := metav1.NewTime(metav1.Now().Add(-1))
		ecWithPrev := ec()
		ecWithPrev.Status = &v1alpha1.ElasticClusterStatus{
			CephTopology: &v1alpha1.CephTopologyStatus{MonCount: 5, MgrCount: 3, LastPromotedAt: &old},
		}
		out := buildCephTopologyStatus(ecWithPrev, 5, 3, v1alpha1.CephTopologyReasonHighRedundancyESCPresent)
		Expect(out.LastPromotedAt).NotTo(BeNil())
		Expect(out.LastPromotedAt.UnixNano()).To(Equal(old.UnixNano()),
			"sticky steady-state reconcile must not rewrite LastPromotedAt")
	})
})
