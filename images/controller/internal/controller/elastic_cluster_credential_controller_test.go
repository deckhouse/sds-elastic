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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

var _ = Describe("ElasticClusterCredentialReconciler", func() {
	var ctx = context.Background()

	Describe("desiredECCSpec", func() {
		It("populates all three fields from a modern Rook secret", func() {
			secret := newRookMonSecret("fsid-1", "admin-1", "mon-1")
			Expect(desiredECCSpec(secret)).To(Equal(v1alpha1.ElasticClusterCredentialSpec{
				FSID: "fsid-1", AdminSecret: "admin-1", MonSecret: "mon-1",
			}))
		})

		It("falls back to ceph-secret when admin-secret is absent", func() {
			secret := newRookMonSecretLegacy("fsid-1", "legacy-key", "mon-1")
			Expect(desiredECCSpec(secret)).To(Equal(v1alpha1.ElasticClusterCredentialSpec{
				FSID: "fsid-1", AdminSecret: "legacy-key", MonSecret: "mon-1",
			}),
				"older Rook (and the version currently shipped with Deckhouse) only populates ceph-secret; without the fallback the ECC stays Pending forever")
		})

		It("prefers admin-secret over ceph-secret when both are present", func() {
			secret := newRookMonSecret("fsid-1", "admin-rotated", "mon-1")
			secret.Data[external.RookCephMonSecretCephSecretKey] = []byte("legacy-stale")
			Expect(desiredECCSpec(secret).AdminSecret).To(Equal("admin-rotated"),
				"admin-secret wins so a freshly-rotated key always trumps the legacy mirror")
		})

		It("treats blank admin-secret the same as missing and uses ceph-secret", func() {
			secret := newRookMonSecretLegacy("fsid-1", "fallback-key", "mon-1")
			secret.Data[external.RookCephMonSecretAdminSecretKey] = []byte("   \n  ")
			Expect(desiredECCSpec(secret).AdminSecret).To(Equal("fallback-key"))
		})

		It("returns empty AdminSecret when the secret has neither key", func() {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      external.RookCephMonSecretName,
					Namespace: testNamespace,
				},
				Data: map[string][]byte{
					external.RookCephMonSecretFSIDKey:      []byte("fsid-1"),
					external.RookCephMonSecretMonSecretKey: []byte("mon-1"),
				},
			}
			Expect(desiredECCSpec(secret).AdminSecret).To(BeEmpty())
		})
	})

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

		It("back-syncs a legacy Rook secret (ceph-secret only) into AdminSecret", func() {
			ec := newTestElasticCluster()
			cl := newFakeClient(ec, newRookMonSecretLegacy("fsid-x", "legacy-key", "mon-x"))
			r := newElasticClusterCredentialReconciler(cl)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}})
			Expect(err).NotTo(HaveOccurred())

			ecc := &v1alpha1.ElasticClusterCredential{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, ecc)).To(Succeed())
			Expect(ecc.Spec.AdminSecret).To(Equal("legacy-key"),
				"reproduces the in-cluster bug where Rook only writes 'ceph-secret' and the ECC stayed Pending until the fallback was added")
			Expect(ecc.Status.Phase).To(Equal(v1alpha1.ECCPhasePopulated))
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
