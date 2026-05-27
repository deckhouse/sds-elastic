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
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

var _ = Describe("ElasticStorageClass stages", func() {
	var (
		ctx     = context.Background()
		escName = "pool-stages"
		esc     = newTestElasticStorageClass(escName, v1alpha1.StorageClassTypeRBD)
		r       *ElasticStorageClassReconciler
	)

	DescribeTable("getReadyEC",
		func(seed func() *v1alpha1.ElasticCluster, wantReady bool, wantMsgSubstr string) {
			ec := seed()
			cl := newFakeClient()
			if ec != nil {
				Expect(cl.Create(ctx, ec)).To(Succeed())
			}
			r = newElasticStorageClassReconciler(cl)

			gotReady, gotMsg, err := r.getReadyEC(ctx, esc)
			Expect(err).NotTo(HaveOccurred())
			Expect(gotReady).To(Equal(wantReady))
			if wantMsgSubstr != "" {
				Expect(gotMsg).To(ContainSubstring(wantMsgSubstr))
			}
		},
		Entry("missing EC", func() *v1alpha1.ElasticCluster { return nil }, false, "waiting for ElasticCluster"),
		Entry("no status", newTestElasticCluster, false, "no status yet"),
		Entry("CephClusterReady False", func() *v1alpha1.ElasticCluster {
			ec := newTestElasticCluster()
			ec.Status = &v1alpha1.ElasticClusterStatus{
				Conditions: []metav1.Condition{{
					Type:   v1alpha1.ECConditionCephClusterReady,
					Status: metav1.ConditionFalse,
				}},
			}
			return ec
		}, false, "CephClusterReady=True"),
		Entry("CephClusterReady True", func() *v1alpha1.ElasticCluster {
			return ecWithCephClusterReady(newTestElasticCluster())
		}, true, ""),
	)

	It("ensureRBDPool waits when pool is not yet visible", func() {
		ec := ecWithCephClusterReady(newTestElasticCluster())
		cl := newFakeClient(esc, ec)
		r = newElasticStorageClassReconciler(cl)

		done, msg, err := r.ensureRBDPool(ctx, esc)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(msg).To(ContainSubstring("phase="))
	})

	It("ensureCsiStorageClass waits when SC is not yet visible", func() {
		cl := newFakeClient(esc)
		r = newElasticStorageClassReconciler(cl)

		done, msg, err := r.ensureCsiStorageClass(ctx, esc)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(msg).To(ContainSubstring("phase="))
	})

	It("ensurePool returns error for unsupported type", func() {
		bad := newTestElasticStorageClass("bad", v1alpha1.StorageClassType("Unknown"))
		ec := ecWithCephClusterReady(newTestElasticCluster())
		cl := newFakeClient(bad, ec)
		r = newElasticStorageClassReconciler(cl)

		_, _, err := r.ensurePool(ctx, bad)
		Expect(err).To(HaveOccurred())
	})

	It("Reconcile is no-op on deletion", func() {
		cl := newFakeClient(esc)
		r = newElasticStorageClassReconciler(cl)
		latest := &v1alpha1.ElasticStorageClass{}
		Expect(cl.Get(ctx, types.NamespacedName{Name: escName}, latest)).To(Succeed())
		latest.Finalizers = []string{"test.hold"}
		Expect(cl.Update(ctx, latest)).To(Succeed())
		grace := int64(0)
		Expect(cl.Delete(ctx, latest, &client.DeleteOptions{GracePeriodSeconds: &grace})).To(Succeed())

		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: escName}})
		Expect(err).NotTo(HaveOccurred())
	})
})
