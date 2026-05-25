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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// markUnstructuredPhase patches status.phase on every CR matching
// ECClusterLabel=<ec.Name> for the given GVK. Mirrors what
// sds-node-configurator does once it has provisioned a VG/LV on the
// host: post-Create the controller reconciles a second time and
// observes the new phase.
func markUnstructuredPhase(ctx context.Context, cl client.Client, gvk schema.GroupVersionKind, ec *v1alpha1.ElasticCluster, phase string) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List",
	})
	Expect(cl.List(ctx, list, client.MatchingLabels{external.ECClusterLabel: ec.Name})).To(Succeed())
	for i := range list.Items {
		obj := &list.Items[i]
		if obj.Object["status"] == nil {
			obj.Object["status"] = map[string]interface{}{}
		}
		status := obj.Object["status"].(map[string]interface{})
		status["phase"] = phase
		Expect(cl.Update(ctx, obj)).To(Succeed())
	}
}

// markPVPhase patches status.phase on every PV labelled with
// ECClusterLabel=<ec.Name>. Same role as markUnstructuredPhase: the
// fake client has no built-in volume binder, so the test plays the
// role of the binder.
func markPVPhase(ctx context.Context, cl client.Client, ec *v1alpha1.ElasticCluster, phase corev1.PersistentVolumePhase) {
	list := &corev1.PersistentVolumeList{}
	Expect(cl.List(ctx, list, client.MatchingLabels{external.ECClusterLabel: ec.Name})).To(Succeed())
	for i := range list.Items {
		pv := list.Items[i]
		pv.Status.Phase = phase
		Expect(cl.Status().Update(ctx, &pv)).To(Succeed())
	}
}

// markLVGsReady / markLLVsCreated / markPVsAvailable wrap the generic
// helpers with the target phase used by ensureStorage gates. Tests use
// them to clearly express which downstream provisioner just finished.
func markLVGsReady(ctx context.Context, cl client.Client, ec *v1alpha1.ElasticCluster) {
	markUnstructuredPhase(ctx, cl, external.LVMVolumeGroupGVK, ec, "Ready")
}
func markLLVsCreated(ctx context.Context, cl client.Client, ec *v1alpha1.ElasticCluster) {
	markUnstructuredPhase(ctx, cl, external.LVMLogicalVolumeGVK, ec, "Created")
}
func markPVsAvailable(ctx context.Context, cl client.Client, ec *v1alpha1.ElasticCluster) {
	markPVPhase(ctx, cl, ec, corev1.VolumeAvailable)
}

// driveStorageToReady walks ensureStorage through the full sequential
// chain — call → mark LVGs Ready → call → mark LLVs Created → call →
// mark PVs Available → call done=true. Tests that only care about the
// final state use this to keep the body compact.
func driveStorageToReady(ctx context.Context, r *ElasticClusterReconciler, cl client.Client, ec *v1alpha1.ElasticCluster) {
	// Phase 1 → Phase 2 transition.
	done, _, _, reason, _, err := r.ensureStorage(ctx, ec)
	Expect(err).NotTo(HaveOccurred())
	Expect(done).To(BeFalse())
	Expect(reason).To(Equal(storageReasonWaitingForLVG))
	markLVGsReady(ctx, cl, ec)

	// Phase 2 → Phase 3 transition.
	done, _, _, reason, _, err = r.ensureStorage(ctx, ec)
	Expect(err).NotTo(HaveOccurred())
	Expect(done).To(BeFalse())
	Expect(reason).To(Equal(storageReasonWaitingForLLV))
	markLLVsCreated(ctx, cl, ec)

	// Phase 3 → done transition.
	done, _, _, reason, _, err = r.ensureStorage(ctx, ec)
	Expect(err).NotTo(HaveOccurred())
	Expect(done).To(BeFalse())
	Expect(reason).To(Equal(storageReasonWaitingForPV))
	markPVsAvailable(ctx, cl, ec)

	done, _, _, reason, _, err = r.ensureStorage(ctx, ec)
	Expect(err).NotTo(HaveOccurred())
	Expect(done).To(BeTrue())
	Expect(reason).To(BeEmpty())
}

// expectAbsent asserts that no CR of the given GVK exists with the
// ECClusterLabel set to ec.Name. Used by the sequential-flow tests to
// pin down the invariant that LLV/PV CRs are NOT created before their
// upstream stage gate has been cleared.
func expectAbsent(ctx context.Context, cl client.Client, gvk schema.GroupVersionKind, ec *v1alpha1.ElasticCluster) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List",
	})
	Expect(cl.List(ctx, list, client.MatchingLabels{external.ECClusterLabel: ec.Name})).To(Succeed())
	Expect(list.Items).To(BeEmpty(),
		"%s CRs must not exist before their stage gate has cleared", gvk.Kind)
}

func expectAbsentPVs(ctx context.Context, cl client.Client, ec *v1alpha1.ElasticCluster) {
	list := &corev1.PersistentVolumeList{}
	Expect(cl.List(ctx, list, client.MatchingLabels{external.ECClusterLabel: ec.Name})).To(Succeed())
	Expect(list.Items).To(BeEmpty(),
		"local PersistentVolumes must not exist before their stage gate has cleared")
}

var _ = Describe("ensureStorage", func() {
	var (
		ctx = context.Background()
		ec  = newTestElasticCluster()
		r   *ElasticClusterReconciler
		cl  client.Client
	)

	Context("happy path with three BlockDevices", func() {
		BeforeEach(func() {
			cl = newFakeClient(
				ec,
				newTestNode("node-a"),
				newTestNode("node-b"),
				newBlockDevice("bd-a", "node-a", "100Gi", true, nil),
				newBlockDevice("bd-b", "node-a", "200Gi", true, nil),
				newBlockDevice("bd-c", "node-b", "150Gi", true, nil),
			)
			r = newElasticClusterReconciler(cl)
		})

		It("first reconcile upserts only LVGs and gates on LVG phase", func() {
			done, osdCount, pvcRequest, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse(),
				"first reconcile must wait for sds-node-configurator to flip LVG.status.phase=Ready")
			Expect(reason).To(Equal(storageReasonWaitingForLVG))
			Expect(msg).To(ContainSubstring("LVMVolumeGroups Ready"))
			Expect(osdCount).To(Equal(int32(3)),
				"osdCount surfaces the count even while gated so the next stages can plan ahead")
			expected100Gi := resource.MustParse("100Gi")
			Expect(pvcRequest.Cmp(expected100Gi)).To(Equal(0),
				"pvcRequest must equal min(BD.size) = 100Gi, got %s", pvcRequest.String())

			for _, bdName := range []string{"bd-a", "bd-b", "bd-c"} {
				bd := &unstructured.Unstructured{}
				bd.SetGroupVersionKind(external.BlockDeviceGVK)
				Expect(cl.Get(ctx, types.NamespacedName{Name: bdName}, bd)).To(Succeed())
				Expect(bd.GetLabels()[external.ECClusterLabel]).To(Equal(testECName))

				lvg := &unstructured.Unstructured{}
				lvg.SetGroupVersionKind(external.LVMVolumeGroupGVK)
				Expect(cl.Get(ctx, types.NamespacedName{Name: builder.ECOSDResourceName(ec, bdName)}, lvg)).To(Succeed())
			}

			// LLV/PV must NOT have been created yet — sequencing is the
			// whole point of the sequential refactor: SNC's watch race
			// on out-of-order LLV must not be reachable.
			expectAbsent(ctx, cl, external.LVMLogicalVolumeGVK, ec)
			expectAbsentPVs(ctx, cl, ec)
		})

		It("creates LLVs only after every LVG is Ready, PVs only after every LLV is Created", func() {
			// Phase 1 reconcile: only LVGs.
			_, _, _, reason, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(storageReasonWaitingForLVG))
			expectAbsent(ctx, cl, external.LVMLogicalVolumeGVK, ec)
			expectAbsentPVs(ctx, cl, ec)

			markLVGsReady(ctx, cl, ec)

			// Phase 2 reconcile: LLVs created, no PVs.
			_, _, _, reason, _, err = r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(storageReasonWaitingForLLV))
			for _, bdName := range []string{"bd-a", "bd-b", "bd-c"} {
				llv := &unstructured.Unstructured{}
				llv.SetGroupVersionKind(external.LVMLogicalVolumeGVK)
				Expect(cl.Get(ctx, types.NamespacedName{Name: builder.ECOSDResourceName(ec, bdName)}, llv)).To(Succeed())
			}
			expectAbsentPVs(ctx, cl, ec)

			markLLVsCreated(ctx, cl, ec)

			// Phase 3 reconcile: PVs created.
			_, _, _, reason, _, err = r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(storageReasonWaitingForPV))
			for _, bdName := range []string{"bd-a", "bd-b", "bd-c"} {
				pv := &corev1.PersistentVolume{}
				Expect(cl.Get(ctx, types.NamespacedName{Name: builder.ECOSDResourceName(ec, bdName)}, pv)).To(Succeed())
				Expect(pv.Labels[external.ECClusterLabel]).To(Equal(testECName))
			}

			markPVsAvailable(ctx, cl, ec)

			// Final reconcile: done.
			done, osdCount, pvcRequest, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(reason).To(BeEmpty())
			Expect(osdCount).To(Equal(int32(3)))
			expected100Gi := resource.MustParse("100Gi")
			Expect(pvcRequest.Cmp(expected100Gi)).To(Equal(0))
			Expect(msg).To(ContainSubstring("OSD volumes ready"))
		})
	})

	Context("phase progression", func() {
		BeforeEach(func() {
			cl = newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-a", "node-a", "100Gi", true, nil),
				newBlockDevice("bd-b", "node-a", "100Gi", true, nil),
			)
			r = newElasticClusterReconciler(cl)
			// Phase 0+1: upsert LVGs. Subsequent specs decide where to
			// drive the chain to next.
			_, _, _, _, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
		})

		It("reports WaitingForLVG until LVG.phase==Ready on every BD", func() {
			done, _, _, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(reason).To(Equal(storageReasonWaitingForLVG))
			Expect(msg).To(ContainSubstring("0/2 LVMVolumeGroups Ready"))
			Expect(msg).To(ContainSubstring("(NoStatus)"))
			expectAbsent(ctx, cl, external.LVMLogicalVolumeGVK, ec)
			expectAbsentPVs(ctx, cl, ec)
		})

		It("reports WaitingForLLV once LVG is Ready and the next reconcile creates LLVs", func() {
			markLVGsReady(ctx, cl, ec)
			done, _, _, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(reason).To(Equal(storageReasonWaitingForLLV))
			Expect(msg).To(ContainSubstring("0/2 LVMLogicalVolumes Created"))
			Expect(msg).To(ContainSubstring("(NoStatus)"))
			// PV creation must still be gated.
			expectAbsentPVs(ctx, cl, ec)
		})

		It("reports WaitingForPV once LVG/LLV are converged and the next reconcile creates PVs", func() {
			markLVGsReady(ctx, cl, ec)
			_, _, _, _, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			markLLVsCreated(ctx, cl, ec)
			done, _, _, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(reason).To(Equal(storageReasonWaitingForPV))
			Expect(msg).To(ContainSubstring("PersistentVolumes Available|Bound"))
		})

		It("highlights a single laggard LVG via per-resource phase token", func() {
			markLVGsReady(ctx, cl, ec)
			lvg := &unstructured.Unstructured{}
			lvg.SetGroupVersionKind(external.LVMVolumeGroupGVK)
			Expect(cl.Get(ctx, types.NamespacedName{Name: builder.ECOSDResourceName(ec, "bd-a")}, lvg)).To(Succeed())
			status := lvg.Object["status"].(map[string]interface{})
			status["phase"] = "Pending"
			Expect(cl.Update(ctx, lvg)).To(Succeed())

			done, _, _, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(reason).To(Equal(storageReasonWaitingForLVG))
			Expect(msg).To(ContainSubstring("1/2 LVMVolumeGroups Ready"))
			Expect(msg).To(ContainSubstring("(Pending)"))
			expectAbsent(ctx, cl, external.LVMLogicalVolumeGVK, ec)
		})

		It("becomes Ready after the full sequential chain converges", func() {
			markLVGsReady(ctx, cl, ec)
			_, _, _, _, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			markLLVsCreated(ctx, cl, ec)
			_, _, _, _, _, err = r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			markPVsAvailable(ctx, cl, ec)

			done, _, _, reason, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(reason).To(BeEmpty())
		})

		It("accepts Bound PVs as Ready (post-binding state)", func() {
			markLVGsReady(ctx, cl, ec)
			_, _, _, _, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			markLLVsCreated(ctx, cl, ec)
			_, _, _, _, _, err = r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			markPVPhase(ctx, cl, ec, corev1.VolumeBound)

			done, _, _, _, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
		})
	})

	Context("soft-skip malformed BlockDevices", func() {
		BeforeEach(func() {
			cl = newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-good", "node-a", "100Gi", true, nil),
				newBlockDevice("bd-bad-node", "", "100Gi", true, nil),
				newBlockDevice("bd-bad-size", "node-a", "not-a-quantity", true, nil),
			)
			r = newElasticClusterReconciler(cl)
		})

		It("provisions healthy LVGs and short-circuits with WaitingForBlockDevices", func() {
			done, osdCount, pvcRequest, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(reason).To(Equal(storageReasonWaitingForBlockDevices),
				"the skipped-BD branch must short-circuit before LVG phase checks so the operator sees the validation reason first")
			Expect(osdCount).To(Equal(int32(1)))
			expected100Gi := resource.MustParse("100Gi")
			Expect(pvcRequest.Cmp(expected100Gi)).To(Equal(0),
				"only bd-good is selected, so pvcRequest must equal its 100Gi size, got %s", pvcRequest.String())
			Expect(msg).To(ContainSubstring("skipped"))
			Expect(msg).To(ContainSubstring("bd-bad-size"))
			expectAbsent(ctx, cl, external.LVMLogicalVolumeGVK, ec)
			expectAbsentPVs(ctx, cl, ec)
		})
	})

	Context("no matching BlockDevices", func() {
		It("returns NoBlockDevices reason and zero osdCount", func() {
			cl = newFakeClient(ec, newTestNode("node-a"))
			r = newElasticClusterReconciler(cl)
			done, osdCount, pvcRequest, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(reason).To(Equal(storageReasonNoBlockDevices))
			Expect(osdCount).To(Equal(int32(0)))
			Expect(pvcRequest.IsZero()).To(BeTrue(), "pvcRequest must be zero when no BD selected")
			Expect(msg).To(ContainSubstring("no BlockDevices match"))
		})
	})

	Context("adopted non-consumable BlockDevice", func() {
		It("keeps selecting BD with ECClusterLabel and converges through the sequential chain", func() {
			cl = newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-adopted", "node-a", "100Gi", false, map[string]string{
					external.ECClusterLabel: testECName,
				}),
			)
			r = newElasticClusterReconciler(cl)

			driveStorageToReady(ctx, r, cl, ec)

			done, osdCount, pvcRequest, reason, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(reason).To(BeEmpty())
			Expect(osdCount).To(Equal(int32(1)))
			expected100Gi := resource.MustParse("100Gi")
			Expect(pvcRequest.Cmp(expected100Gi)).To(Equal(0))
		})
	})

	Context("invariant: LLV/PV are not created before their stage gate", func() {
		It("never creates LLVs while any LVG is still NoStatus, even across many reconciles", func() {
			cl = newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-a", "node-a", "100Gi", true, nil),
				newBlockDevice("bd-b", "node-a", "200Gi", true, nil),
			)
			r = newElasticClusterReconciler(cl)
			for i := 0; i < 5; i++ {
				_, _, _, reason, _, err := r.ensureStorage(ctx, ec)
				Expect(err).NotTo(HaveOccurred())
				Expect(reason).To(Equal(storageReasonWaitingForLVG))
				expectAbsent(ctx, cl, external.LVMLogicalVolumeGVK, ec)
				expectAbsentPVs(ctx, cl, ec)
			}
		})

		It("never creates PVs while any LLV is still NoStatus, even across many reconciles", func() {
			cl = newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-a", "node-a", "100Gi", true, nil),
				newBlockDevice("bd-b", "node-a", "200Gi", true, nil),
			)
			r = newElasticClusterReconciler(cl)

			_, _, _, _, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			markLVGsReady(ctx, cl, ec)

			for i := 0; i < 5; i++ {
				_, _, _, reason, _, err := r.ensureStorage(ctx, ec)
				Expect(err).NotTo(HaveOccurred())
				Expect(reason).To(Equal(storageReasonWaitingForLLV))
				expectAbsentPVs(ctx, cl, ec)
			}
		})

		It("propagates a transient SNC LLV failure (Failed phase) as WaitingForLLV", func() {
			cl = newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-a", "node-a", "100Gi", true, nil),
			)
			r = newElasticClusterReconciler(cl)
			_, _, _, _, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			markLVGsReady(ctx, cl, ec)
			_, _, _, _, _, err = r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			markUnstructuredPhase(ctx, cl, external.LVMLogicalVolumeGVK, ec, "Failed")

			done, _, _, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(reason).To(Equal(storageReasonWaitingForLLV))
			Expect(msg).To(ContainSubstring("(Failed)"))
		})
	})

	Context("BD owned by another ElasticCluster", func() {
		It("short-circuits with reason=OwnershipConflict and refuses to overwrite the foreign label", func() {
			cl = newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-foreign", "node-a", "100Gi", true, map[string]string{
					external.ECClusterLabel: "other-ec",
				}),
			)
			r = newElasticClusterReconciler(cl)

			done, osdCount, pvcRequest, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred(),
				"OwnershipConflict is a recoverable state; not a hard reconcile error")
			Expect(done).To(BeFalse())
			Expect(reason).To(Equal(v1alpha1.ECReasonOwnershipConflict))
			Expect(osdCount).To(Equal(int32(0)))
			Expect(pvcRequest.IsZero()).To(BeTrue())
			Expect(msg).To(ContainSubstring("bd-foreign: claimed by other-ec"))

			// BD label must remain pointing at the original owner — silently
			// overwriting was the bug we are guarding against.
			bd := &unstructured.Unstructured{}
			bd.SetGroupVersionKind(external.BlockDeviceGVK)
			Expect(cl.Get(ctx, types.NamespacedName{Name: "bd-foreign"}, bd)).To(Succeed())
			Expect(bd.GetLabels()[external.ECClusterLabel]).To(Equal("other-ec"))

			// Downstream resources must not have been provisioned: stage
			// halted before Phase 0 could touch LVG/LLV/PV.
			expectAbsent(ctx, cl, external.LVMVolumeGroupGVK, ec)
			expectAbsent(ctx, cl, external.LVMLogicalVolumeGVK, ec)
			expectAbsentPVs(ctx, cl, ec)
		})

		It("blocks the entire stage even when free BDs are also present (strict mode)", func() {
			cl = newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-foreign", "node-a", "100Gi", true, map[string]string{
					external.ECClusterLabel: "other-ec",
				}),
				newBlockDevice("bd-free-1", "node-a", "100Gi", true, nil),
				newBlockDevice("bd-free-2", "node-a", "200Gi", true, nil),
			)
			r = newElasticClusterReconciler(cl)

			done, osdCount, _, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(reason).To(Equal(v1alpha1.ECReasonOwnershipConflict))
			Expect(osdCount).To(Equal(int32(0)),
				"strict: even free BDs do not get adopted while a conflict is unresolved")
			Expect(msg).To(ContainSubstring("bd-foreign: claimed by other-ec"))

			// Free BDs must NOT have been adopted: the stage exited before
			// Phase 0's adopt loop, so no label patches should have landed.
			for _, bdName := range []string{"bd-free-1", "bd-free-2"} {
				bd := &unstructured.Unstructured{}
				bd.SetGroupVersionKind(external.BlockDeviceGVK)
				Expect(cl.Get(ctx, types.NamespacedName{Name: bdName}, bd)).To(Succeed())
				_, ok := bd.GetLabels()[external.ECClusterLabel]
				Expect(ok).To(BeFalse(),
					"free BD %q must remain unadopted while stage is blocked by a conflict", bdName)
			}
			expectAbsent(ctx, cl, external.LVMVolumeGroupGVK, ec)
		})

		It("aggregates multiple foreign-owned BDs into a sorted message", func() {
			cl = newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-c", "node-a", "100Gi", true, map[string]string{
					external.ECClusterLabel: "ec-gamma",
				}),
				newBlockDevice("bd-a", "node-a", "100Gi", true, map[string]string{
					external.ECClusterLabel: "ec-alpha",
				}),
				newBlockDevice("bd-b", "node-a", "100Gi", true, map[string]string{
					external.ECClusterLabel: "ec-beta",
				}),
			)
			r = newElasticClusterReconciler(cl)

			_, _, _, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(v1alpha1.ECReasonOwnershipConflict))
			// Sorted by bdName for stable diffs in EC.status.
			Expect(msg).To(ContainSubstring("bd-a: claimed by ec-alpha"))
			Expect(msg).To(ContainSubstring("bd-b: claimed by ec-beta"))
			Expect(msg).To(ContainSubstring("bd-c: claimed by ec-gamma"))
			// The "bd-a" entry sorts before "bd-b" in the joined message.
			Expect(msg).To(MatchRegexp("bd-a:.*bd-b:.*bd-c:"))
		})

		It("treats a label key with empty value as 'no owner' and adopts normally", func() {
			cl = newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-a", "node-a", "100Gi", true, map[string]string{
					external.ECClusterLabel: "",
				}),
			)
			r = newElasticClusterReconciler(cl)

			_, osdCount, _, reason, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(reason).To(Equal(storageReasonWaitingForLVG),
				"empty label value is treated as no owner; stage proceeds to LVG provisioning")
			Expect(osdCount).To(Equal(int32(1)))

			bd := &unstructured.Unstructured{}
			bd.SetGroupVersionKind(external.BlockDeviceGVK)
			Expect(cl.Get(ctx, types.NamespacedName{Name: "bd-a"}, bd)).To(Succeed())
			Expect(bd.GetLabels()[external.ECClusterLabel]).To(Equal(testECName),
				"adoption must overwrite an empty-string label value")
		})
	})
})

var _ = Describe("adoptBlockDevice", func() {
	var (
		ctx = context.Background()
		ec  = newTestElasticCluster()
	)

	It("is a no-op when the BD already carries our label", func() {
		bd := newBlockDevice("bd-a", "node-a", "100Gi", true, map[string]string{
			external.ECClusterLabel: testECName,
		})
		cl := newFakeClient(ec, bd)
		r := newElasticClusterReconciler(cl)

		// Use the in-memory bd object directly so we observe whether the
		// function tried to mutate it.
		err := r.adoptBlockDevice(ctx, bd, ec)
		Expect(err).NotTo(HaveOccurred())
	})

	It("returns ErrOwnershipConflict for a foreign label and leaves it untouched", func() {
		bd := newBlockDevice("bd-foreign", "node-a", "100Gi", true, map[string]string{
			external.ECClusterLabel: "other-ec",
		})
		cl := newFakeClient(ec, bd)
		r := newElasticClusterReconciler(cl)

		err := r.adoptBlockDevice(ctx, bd, ec)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, ErrOwnershipConflict)).To(BeTrue(),
			"caller dispatches on errors.Is, the wrapper message is informational")
		Expect(err.Error()).To(ContainSubstring("bd-foreign"))
		Expect(err.Error()).To(ContainSubstring("other-ec"))

		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(external.BlockDeviceGVK)
		Expect(cl.Get(ctx, types.NamespacedName{Name: "bd-foreign"}, fresh)).To(Succeed())
		Expect(fresh.GetLabels()[external.ECClusterLabel]).To(Equal("other-ec"))
	})

	It("adopts a BD with no ECClusterLabel at all", func() {
		bd := newBlockDevice("bd-a", "node-a", "100Gi", true, nil)
		cl := newFakeClient(ec, bd)
		r := newElasticClusterReconciler(cl)

		Expect(r.adoptBlockDevice(ctx, bd, ec)).To(Succeed())

		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(external.BlockDeviceGVK)
		Expect(cl.Get(ctx, types.NamespacedName{Name: "bd-a"}, fresh)).To(Succeed())
		Expect(fresh.GetLabels()[external.ECClusterLabel]).To(Equal(testECName))
	})

	It("adopts a BD whose ECClusterLabel value is empty (treated as no owner)", func() {
		bd := newBlockDevice("bd-a", "node-a", "100Gi", true, map[string]string{
			external.ECClusterLabel: "",
		})
		cl := newFakeClient(ec, bd)
		r := newElasticClusterReconciler(cl)

		Expect(r.adoptBlockDevice(ctx, bd, ec)).To(Succeed())

		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(external.BlockDeviceGVK)
		Expect(cl.Get(ctx, types.NamespacedName{Name: "bd-a"}, fresh)).To(Succeed())
		Expect(fresh.GetLabels()[external.ECClusterLabel]).To(Equal(testECName))
	})
})

var _ = Describe("ensureStorage non-existent CRD branches", func() {
	It("no longer reaches LLV/PV creation when LVG-only fakes are wired", func() {
		// Sanity check: even when the test suite has all GVKs registered,
		// the sequential FSM must hold the LLV creation back. This guards
		// against regressions where a later refactor inadvertently puts
		// the LLV upsert back into the LVG loop.
		ec := newTestElasticCluster()
		ctx := context.Background()
		cl := newFakeClient(
			ec,
			newTestNode("node-a"),
			newBlockDevice("bd-a", "node-a", "100Gi", true, nil),
		)
		r := newElasticClusterReconciler(cl)

		_, _, _, _, _, err := r.ensureStorage(ctx, ec)
		Expect(err).NotTo(HaveOccurred())

		llv := &unstructured.Unstructured{}
		llv.SetGroupVersionKind(external.LVMLogicalVolumeGVK)
		err = cl.Get(ctx, types.NamespacedName{Name: builder.ECOSDResourceName(ec, "bd-a")}, llv)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"LLV must not exist after the first reconcile — only after the LVG phase gate has cleared")
	})
})

var _ = Describe("assessPhases", func() {
	It("counts ready and reports pending names with their observed phase", func() {
		ready, pending := assessPhases(
			[]string{"a", "b", "c", "d"},
			map[string]string{"a": "Ready", "b": "Pending", "c": "Ready"},
			"Ready",
		)
		Expect(ready).To(Equal(2))
		Expect(pending).To(ConsistOf("b(Pending)", "d(NotFound)"))
	})

	It("emits NoStatus token when phase is empty string", func() {
		ready, pending := assessPhases(
			[]string{"a"},
			map[string]string{"a": ""},
			"Ready",
		)
		Expect(ready).To(Equal(0))
		Expect(pending).To(Equal([]string{"a(NoStatus)"}))
	})

	It("matches any of the supplied target phases", func() {
		ready, pending := assessPhases(
			[]string{"a", "b"},
			map[string]string{"a": "Available", "b": "Bound"},
			"Available", "Bound",
		)
		Expect(ready).To(Equal(2))
		Expect(pending).To(BeEmpty())
	})
})
