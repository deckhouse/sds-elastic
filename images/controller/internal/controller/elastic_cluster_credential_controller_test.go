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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

var _ = Describe("ElasticClusterCredentialReconciler", func() {
	var ctx = context.Background()

	DescribeTable("desiredECCSpec",
		func(fsid, admin, mon string, want v1alpha1.ElasticClusterCredentialSpec) {
			secret := newRookMonSecret(fsid, admin, mon)
			Expect(desiredECCSpec(secret)).To(Equal(want))
		},
		Entry("all fields",
			"fsid-1", "admin-1", "mon-1",
			v1alpha1.ElasticClusterCredentialSpec{FSID: "fsid-1", AdminSecret: "admin-1", MonSecret: "mon-1"},
		),
	)

	Describe("getOrCreateECC", func() {
		It("creates ECC when missing", func() {
			ec := newTestElasticCluster()
			cl := newFakeClient(ec)
			r := newElasticClusterCredentialReconciler(cl)

			ecc, err := r.getOrCreateECC(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(ecc.Name).To(Equal(testECName))

			got := &v1alpha1.ElasticClusterCredential{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, got)).To(Succeed())
		})

		It("returns existing ECC without recreating", func() {
			ec := newTestElasticCluster()
			existing := &v1alpha1.ElasticClusterCredential{
				ObjectMeta: metav1.ObjectMeta{Name: testECName},
				Spec:       v1alpha1.ElasticClusterCredentialSpec{FSID: "keep"},
			}
			cl := newFakeClient(ec, existing)
			r := newElasticClusterCredentialReconciler(cl)

			ecc, err := r.getOrCreateECC(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(ecc.Spec.FSID).To(Equal("keep"))
		})
	})

	Describe("Reconcile", func() {
		It("is a no-op when parent EC is missing", func() {
			cl := newFakeClient()
			r := newElasticClusterCredentialReconciler(cl)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}})
			Expect(err).NotTo(HaveOccurred())

			got := &v1alpha1.ElasticClusterCredential{}
			err = cl.Get(ctx, types.NamespacedName{Name: testECName}, got)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("creates ECC and sets Phase=Pending when Secret is missing", func() {
			ec := newTestElasticCluster()
			cl := newFakeClient(ec)
			r := newElasticClusterCredentialReconciler(cl)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}})
			Expect(err).NotTo(HaveOccurred())

			ecc := &v1alpha1.ElasticClusterCredential{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, ecc)).To(Succeed())
			Expect(ecc.Status.Phase).To(Equal(v1alpha1.ECCPhasePending))
			Expect(ecc.Status.LastSyncTime).To(BeNil())
		})

		It("back-syncs Secret and sets Phase=Populated with LastSyncTime", func() {
			ec := newTestElasticCluster()
			cl := newFakeClient(ec, newRookMonSecret("fsid-x", "admin-x", "mon-x"))
			r := newElasticClusterCredentialReconciler(cl)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}})
			Expect(err).NotTo(HaveOccurred())

			ecc := &v1alpha1.ElasticClusterCredential{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, ecc)).To(Succeed())
			Expect(ecc.Spec.FSID).To(Equal("fsid-x"))
			Expect(ecc.Status.Phase).To(Equal(v1alpha1.ECCPhasePopulated))
			Expect(ecc.Status.LastSyncTime).NotTo(BeNil())
		})

		It("bumps LastSyncTime when admin-secret rotates", func() {
			ec := newTestElasticCluster()
			secret := newRookMonSecret("fsid-x", "admin-v1", "mon-x")
			cl := newFakeClient(ec, secret)
			r := newElasticClusterCredentialReconciler(cl)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}})
			Expect(err).NotTo(HaveOccurred())

			ecc := &v1alpha1.ElasticClusterCredential{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, ecc)).To(Succeed())
			firstSync := ecc.Status.LastSyncTime.Time

			secret.Data["admin-secret"] = []byte("admin-v2")
			Expect(cl.Update(ctx, secret)).To(Succeed())
			time.Sleep(1100 * time.Millisecond)

			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}})
			Expect(err).NotTo(HaveOccurred())

			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, ecc)).To(Succeed())
			Expect(ecc.Spec.AdminSecret).To(Equal("admin-v2"))
			Expect(ecc.Status.LastSyncTime).NotTo(BeNil())
			Expect(ecc.Status.LastSyncTime.Time).To(BeTemporally(">", firstSync))
		})

		It("steady-state reconcile does not rewrite LastSyncTime", func() {
			ec := newTestElasticCluster()
			cl := newFakeClient(ec, newRookMonSecret("fsid-x", "admin-x", "mon-x"))
			r := newElasticClusterCredentialReconciler(cl)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}})
			Expect(err).NotTo(HaveOccurred())

			ecc := &v1alpha1.ElasticClusterCredential{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, ecc)).To(Succeed())
			firstSync := ecc.Status.LastSyncTime.DeepCopy()

			_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}})
			Expect(err).NotTo(HaveOccurred())

			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, ecc)).To(Succeed())
			Expect(ecc.Status.LastSyncTime).To(Equal(firstSync))
		})
	})
})
