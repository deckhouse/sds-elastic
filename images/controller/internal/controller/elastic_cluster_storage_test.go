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
	corev1 "k8s.io/api/core/v1"
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

// markStorageProvisioned simulates the happy outcome of all downstream
// provisioners (sds-node-configurator + K8s PV binder) by flipping the
// observed phases on every LVG/LLV/PV the controller has just upserted.
func markStorageProvisioned(ctx context.Context, cl client.Client, ec *v1alpha1.ElasticCluster) {
	markUnstructuredPhase(ctx, cl, external.LVMVolumeGroupGVK, ec, "Ready")
	markUnstructuredPhase(ctx, cl, external.LVMLogicalVolumeGVK, ec, "Created")
	markPVPhase(ctx, cl, ec, corev1.VolumeAvailable)
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

		It("first reconcile upserts LVG/LLV/PV and gates on LVG phase", func() {
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

				llv := &unstructured.Unstructured{}
				llv.SetGroupVersionKind(external.LVMLogicalVolumeGVK)
				Expect(cl.Get(ctx, types.NamespacedName{Name: builder.ECOSDResourceName(ec, bdName)}, llv)).To(Succeed())

				pv := &corev1.PersistentVolume{}
				Expect(cl.Get(ctx, types.NamespacedName{Name: builder.ECOSDResourceName(ec, bdName)}, pv)).To(Succeed())
				Expect(pv.Labels[external.ECClusterLabel]).To(Equal(testECName))
			}
		})

		It("becomes Ready after sds-node-configurator and the PV binder converge", func() {
			done, _, _, _, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())

			markStorageProvisioned(ctx, cl, ec)

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
		})

		It("reports WaitingForLLV once LVG is Ready but LLV is not Created", func() {
			markUnstructuredPhase(ctx, cl, external.LVMVolumeGroupGVK, ec, "Ready")
			done, _, _, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(reason).To(Equal(storageReasonWaitingForLLV))
			Expect(msg).To(ContainSubstring("0/2 LVMLogicalVolumes Created"))
		})

		It("reports WaitingForPV once LVG/LLV are converged but PV phase has not flipped", func() {
			markUnstructuredPhase(ctx, cl, external.LVMVolumeGroupGVK, ec, "Ready")
			markUnstructuredPhase(ctx, cl, external.LVMLogicalVolumeGVK, ec, "Created")
			done, _, _, reason, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(reason).To(Equal(storageReasonWaitingForPV))
			Expect(msg).To(ContainSubstring("PersistentVolumes Available|Bound"))
		})

		It("highlights a single laggard LVG via per-resource phase token", func() {
			markUnstructuredPhase(ctx, cl, external.LVMVolumeGroupGVK, ec, "Ready")
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
		})

		It("becomes Ready once all phases match targets", func() {
			markStorageProvisioned(ctx, cl, ec)
			done, _, _, reason, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(reason).To(BeEmpty())
		})

		It("accepts Bound PVs as Ready (post-binding state)", func() {
			markUnstructuredPhase(ctx, cl, external.LVMVolumeGroupGVK, ec, "Ready")
			markUnstructuredPhase(ctx, cl, external.LVMLogicalVolumeGVK, ec, "Created")
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

		It("provisions healthy devices and short-circuits with WaitingForBlockDevices", func() {
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
		It("keeps selecting BD with ECClusterLabel and progresses once phases converge", func() {
			cl = newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-adopted", "node-a", "100Gi", false, map[string]string{
					external.ECClusterLabel: testECName,
				}),
			)
			r = newElasticClusterReconciler(cl)

			_, _, _, _, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			markStorageProvisioned(ctx, cl, ec)

			done, osdCount, pvcRequest, reason, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(reason).To(BeEmpty())
			Expect(osdCount).To(Equal(int32(1)))
			expected100Gi := resource.MustParse("100Gi")
			Expect(pvcRequest.Cmp(expected100Gi)).To(Equal(0))
		})
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
