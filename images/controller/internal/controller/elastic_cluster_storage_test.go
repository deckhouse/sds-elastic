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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

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

		It("provisions LVG, LLV, PV and adopts BlockDevices", func() {
			done, osdCount, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(osdCount).To(Equal(int32(3)))
			Expect(msg).To(ContainSubstring("selected 3 BlockDevices"))

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

		It("provisions healthy devices and reports skipped ones", func() {
			done, osdCount, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(osdCount).To(Equal(int32(1)))
			Expect(msg).To(ContainSubstring("skipped"))
			// Empty nodeName is filtered in listMatchingBlockDevices (not matchingNodes[""]).
			Expect(msg).To(ContainSubstring("bd-bad-size"))
		})
	})

	Context("no matching BlockDevices", func() {
		It("returns in-progress with zero osdCount", func() {
			cl = newFakeClient(ec, newTestNode("node-a"))
			r = newElasticClusterReconciler(cl)
			done, osdCount, msg, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(osdCount).To(Equal(int32(0)))
			Expect(msg).To(ContainSubstring("no BlockDevices match"))
		})
	})

	Context("adopted non-consumable BlockDevice", func() {
		It("keeps selecting BD with ECClusterLabel even when consumable=false", func() {
			cl = newFakeClient(
				ec,
				newTestNode("node-a"),
				newBlockDevice("bd-adopted", "node-a", "100Gi", false, map[string]string{
					external.ECClusterLabel: testECName,
				}),
			)
			r = newElasticClusterReconciler(cl)
			done, osdCount, _, err := r.ensureStorage(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(osdCount).To(Equal(int32(1)))
		})
	})
})
