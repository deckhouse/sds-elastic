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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	Context("BlockDevice owned by another ElasticCluster", func() {
		It("surfaces OwnershipConflict on StorageReady and never adopts the foreign BD", func() {
			ec := newTestElasticCluster()
			cl := newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-foreign", "node-a", "100Gi", true, map[string]string{
					external.ECClusterLabel: "other-ec",
				}),
			)
			r := newElasticClusterReconciler(cl)
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}}

			result, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred(),
				"OwnershipConflict is reported via status, not raised as a reconcile error")
			Expect(result.RequeueAfter).NotTo(BeZero(),
				"controller must keep requeueing so the operator's manual fix is picked up promptly")

			latest := &v1alpha1.ElasticCluster{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())
			Expect(latest.Status.Phase).To(Equal(v1alpha1.PhaseInProgress))

			storage := findCondition(latest.Status.Conditions, v1alpha1.ECConditionStorageReady)
			Expect(storage.Status).To(Equal(metav1.ConditionFalse))
			Expect(storage.Reason).To(Equal(v1alpha1.ECReasonOwnershipConflict))
			Expect(storage.Message).To(ContainSubstring("bd-foreign: claimed by other-ec"))

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
				newCephClusterUnstructured(ec, "Ready", v1alpha1.DefaultCephVersion, cephImage),
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

	Context("Ceph topology auto-promotion and stickiness", func() {
		// Drives a fully-seeded cluster through three reconcile rounds:
		//   1. Bootstrap with no ESC → cephTopology = (3, 2) Standard.
		//   2. Add a HighRedundancy ESC → cephTopology promotes to (5, 3)
		//      with reason=HighRedundancyESCPresent.
		//   3. Delete the ESC → cephTopology stays at (5, 3) with reason=
		//      StickyHighWaterMark, proving the high-water-mark survives
		//      ESC deletion.
		It("publishes Standard, promotes on HighRedundancy ESC creation, then keeps the high-water-mark on deletion", func() {
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

			driveToReady := func() {
				_, err := r.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				markLVGsReady(ctx, cl, ec)
				_, err = r.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				markLLVsCreated(ctx, cl, ec)
				_, err = r.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				markPVsAvailable(ctx, cl, ec)
				_, err = r.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
			}

			// Round 1: bootstrap with no ESC.
			driveToReady()
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())
			Expect(latest.Status.CephTopology).NotTo(BeNil())
			Expect(latest.Status.CephTopology.MonCount).To(Equal(int32(3)))
			Expect(latest.Status.CephTopology.MgrCount).To(Equal(int32(2)))
			Expect(latest.Status.CephTopology.Reason).To(Equal(v1alpha1.CephTopologyReasonStandard))
			Expect(latest.Status.CephTopology.LastPromotedAt).To(BeNil(),
				"Standard profile must NOT stamp LastPromotedAt — the field is reserved as a binary 'we left baseline' marker for the UI")

			// Round 2: add a HighRedundancy ESC, expect promotion to (5, 3).
			Expect(cl.Create(ctx, &v1alpha1.ElasticStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "rbd-hr"},
				Spec: v1alpha1.ElasticStorageClassSpec{
					ClusterRef:  testECName,
					Type:        v1alpha1.StorageClassTypeRBD,
					Replication: v1alpha1.ReplicationHighRedundancy,
				},
			})).To(Succeed())

			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())
			Expect(latest.Status.CephTopology.MonCount).To(Equal(int32(5)))
			Expect(latest.Status.CephTopology.MgrCount).To(Equal(int32(3)))
			Expect(latest.Status.CephTopology.Reason).To(Equal(v1alpha1.CephTopologyReasonHighRedundancyESCPresent))
			Expect(latest.Status.CephTopology.LastPromotedAt).NotTo(BeNil(),
				"crossing above the standard baseline must stamp LastPromotedAt")

			// Verify the CephCluster CR Rook would consume now reflects (5, 3).
			cc := &unstructured.Unstructured{}
			cc.SetGroupVersionKind(external.CephClusterGVK)
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName, Namespace: testNamespace}, cc)).To(Succeed())
			monCount, _, _ := unstructured.NestedInt64(cc.Object, "spec", "mon", "count")
			Expect(monCount).To(Equal(int64(5)))
			mgrCount, _, _ := unstructured.NestedInt64(cc.Object, "spec", "mgr", "count")
			Expect(mgrCount).To(Equal(int64(3)))

			promotedAt := latest.Status.CephTopology.LastPromotedAt.DeepCopy()

			// Round 3: delete the HR ESC; counts must remain at (5, 3) and
			// LastPromotedAt must NOT change.
			Expect(cl.Delete(ctx, &v1alpha1.ElasticStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "rbd-hr"},
			})).To(Succeed())

			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())
			Expect(latest.Status.CephTopology.MonCount).To(Equal(int32(5)),
				"sticky high-water-mark must survive deletion of the triggering ESC")
			Expect(latest.Status.CephTopology.MgrCount).To(Equal(int32(3)))
			Expect(latest.Status.CephTopology.Reason).To(Equal(v1alpha1.CephTopologyReasonStickyHighWaterMark))
			Expect(latest.Status.CephTopology.LastPromotedAt).NotTo(BeNil())
			Expect(latest.Status.CephTopology.LastPromotedAt.UnixNano()).To(Equal(promotedAt.UnixNano()),
				"a sticky-only reconcile must NOT rewrite LastPromotedAt")

			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName, Namespace: testNamespace}, cc)).To(Succeed())
			monCount, _, _ = unstructured.NestedInt64(cc.Object, "spec", "mon", "count")
			Expect(monCount).To(Equal(int64(5)),
				"CephCluster.spec.mon.count must remain 5 even after the trigger ESC is gone")
		})
	})

	Context("rolling Ceph upgrade", func() {
		// Reproduces the user-reported flap: while Rook rolls daemons,
		// CephCluster.status.phase=Progressing causes ensureCephCluster
		// to return done=false, which (before the fix) tripped
		// gateAfter and forced UpgradeInProgress=False/WaitingForPrev
		// every reconcile. The probe must run, publish UpgradeInProgress=
		// True/Upgrading, and the gate must leave it alone — and the
		// `Ceph` printcolumn (status.cephVersion.running) must show the
		// lagging version, not Rook's already-bumped target marker.
		It("keeps UpgradeInProgress=True/Upgrading while CephCluster.status.phase=Progressing with mixed versions.overall", func() {
			ec := newTestElasticCluster()
			cephImage := newTestCfg().CephImages[v1alpha1.DefaultCephVersion]

			cc := newCephClusterUnstructured(ec, "Progressing", "20.2.1-0", cephImage)
			withCephClusterCephStatus(cc, "HEALTH_OK", "", "", 0, 0, 0, "", nil, map[string]map[string]int32{
				"mon":     {cephVerString2021: 3},
				"mgr":     {cephVerString2021: 2},
				"osd":     {cephVerString1923: 4},
				"overall": {cephVerString1923: 4, cephVerString2021: 5},
			})

			cl := newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-a", "node-a", "100Gi", true, nil),
				newRookMonSecret("fsid-abc", "admin-key", "mon-key"),
				newRookMonEndpointsCM("a=10.0.0.1:6789", "a"),
				cc,
				&v1alpha1.ElasticClusterCredential{
					ObjectMeta: metav1.ObjectMeta{Name: testECName},
					Spec:       v1alpha1.ElasticClusterCredentialSpec{AdminSecret: "admin-key"},
				},
				newCephClusterConnectionUnstructured(testECName, "Created"),
			)
			r := newElasticClusterReconciler(cl)

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}}
			latest := &v1alpha1.ElasticCluster{}

			// Drive the storage chain to Ready so the FSM reaches the
			// CephCluster stage; the upgrade probe runs there. We do
			// not need to drain the rest of the FSM — the gate at
			// CephClusterReady (phase=Progressing) is exactly the
			// scenario under test.
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			markLVGsReady(ctx, cl, ec)
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			markLLVsCreated(ctx, cl, ec)
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			markPVsAvailable(ctx, cl, ec)
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())

			// CephClusterReady must be False (Rook is still rolling),
			// downstream stages gated.
			ccReady := findCondition(latest.Status.Conditions, v1alpha1.ECConditionCephClusterReady)
			Expect(ccReady).NotTo(BeNil())
			Expect(ccReady.Status).To(Equal(metav1.ConditionFalse))
			Expect(ccReady.Message).To(ContainSubstring("phase=\"Progressing\""))

			// Core assertion: UpgradeInProgress survived the gate.
			upg := findCondition(latest.Status.Conditions, v1alpha1.ECConditionUpgradeInProgress)
			Expect(upg).NotTo(BeNil(), "UpgradeInProgress must be published by ensureCephCluster even while gating")
			Expect(upg.Status).To(Equal(metav1.ConditionTrue))
			Expect(upg.Reason).To(Equal("Upgrading"))
			Expect(upg.Message).To(ContainSubstring("Rook rolling pods"))

			// Lagging version on the printcolumn so callers see the
			// still-rolling daemons' version, not Rook's target marker.
			Expect(latest.Status.CephVersion).NotTo(BeNil())
			Expect(latest.Status.CephVersion.Running).To(Equal(cephVerString1923))
			Expect(latest.Status.CephVersion.Requested).To(Equal(v1alpha1.DefaultCephVersion))
		})
	})
})
