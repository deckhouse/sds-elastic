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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// ecWithFinalizer returns the shared test EC carrying ECFinalizer.
func ecWithFinalizer() *v1alpha1.ElasticCluster {
	ec := newTestElasticCluster()
	ec.Finalizers = []string{external.ECFinalizer}
	return ec
}

// markDeleting deletes the object so the fake client stamps a
// DeletionTimestamp (the finalizer keeps it around), then returns the
// refreshed copy.
func markDeleting(ctx context.Context, cl client.Client, name string) *v1alpha1.ElasticCluster {
	stale := &v1alpha1.ElasticCluster{}
	ExpectWithOffset(1, cl.Get(ctx, types.NamespacedName{Name: name}, stale)).To(Succeed())
	ExpectWithOffset(1, cl.Delete(ctx, stale)).To(Succeed())
	latest := &v1alpha1.ElasticCluster{}
	ExpectWithOffset(1, cl.Get(ctx, types.NamespacedName{Name: name}, latest)).To(Succeed())
	ExpectWithOffset(1, latest.DeletionTimestamp).NotTo(BeNil())
	return latest
}

// rookHeldCephCluster builds a CephCluster fixture that Rook still holds
// (its own finalizer), optionally carrying the DeletionIsBlocked=True
// condition. Because the finalizer is present, the fake client keeps the
// object after Delete — mirroring a CephCluster stuck in Terminating.
func rookHeldCephCluster(ec *v1alpha1.ElasticCluster, deletionBlocked bool) *unstructured.Unstructured {
	cc := newCephClusterUnstructured(ec, "Ready", "", "")
	cc.SetFinalizers([]string{"ceph.rook.io/disaster-protection"})
	if deletionBlocked {
		_ = unstructured.SetNestedSlice(cc.Object, []interface{}{
			map[string]interface{}{
				"type":   rookDeletionBlockedConditionType,
				"status": "True",
			},
		}, "status", "conditions")
	}
	return cc
}

// rookHeldCephClusterConnection builds a CephClusterConnection fixture
// that csi-ceph still holds (its own finalizer), so the fake client keeps
// it after Delete — mirroring a connection stuck in Terminating.
func rookHeldCephClusterConnection() *unstructured.Unstructured {
	conn := newCephClusterConnectionUnstructured()
	conn.SetFinalizers([]string{"storage.deckhouse.io/csi-ceph"})
	return conn
}

func ecReadyReason(ctx context.Context, cl client.Client, name string) string {
	latest := &v1alpha1.ElasticCluster{}
	ExpectWithOffset(1, cl.Get(ctx, types.NamespacedName{Name: name}, latest)).To(Succeed())
	if latest.Status == nil {
		return ""
	}
	cond := apimeta.FindStatusCondition(latest.Status.Conditions, v1alpha1.ECConditionReady)
	if cond == nil {
		return ""
	}
	return cond.Reason
}

func cephClusterExists(ctx context.Context, cl client.Client) bool {
	cc := &unstructured.Unstructured{}
	cc.SetGroupVersionKind(external.CephClusterGVK)
	err := cl.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: testECName}, cc)
	return err == nil
}

func cephClusterConnectionExists(ctx context.Context, cl client.Client) bool {
	conn := &unstructured.Unstructured{}
	conn.SetGroupVersionKind(external.CephClusterConnectionGVK)
	err := cl.Get(ctx, types.NamespacedName{Name: testECName}, conn)
	return err == nil
}

var _ = Describe("ElasticCluster teardown", func() {
	ctx := context.Background()

	Describe("ensureECFinalizer", func() {
		It("adds the finalizer when missing", func() {
			ec := newTestElasticCluster()
			cl := newFakeClient(ec)
			r := newElasticClusterReconciler(cl)

			Expect(r.ensureECFinalizer(ctx, ec)).To(Succeed())

			latest := &v1alpha1.ElasticCluster{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())
			Expect(latest.Finalizers).To(ContainElement(external.ECFinalizer))
		})

		It("is a no-op when already present", func() {
			ec := ecWithFinalizer()
			cl := newFakeClient(ec)
			r := newElasticClusterReconciler(cl)

			Expect(r.ensureECFinalizer(ctx, ec)).To(Succeed())

			latest := &v1alpha1.ElasticCluster{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())
			Expect(latest.Finalizers).To(Equal([]string{external.ECFinalizer}))
		})
	})

	Describe("reconcileDelete", func() {
		It("returns immediately when the finalizer is absent", func() {
			ec := newTestElasticCluster()
			cl := newFakeClient(ec)
			r := newElasticClusterReconciler(cl)

			res, err := r.reconcileDelete(ctx, ec)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero())
		})

		DescribeTable("blocks while a dependent ElasticStorageClass exists",
			func(escs []*v1alpha1.ElasticStorageClass) {
				ec := ecWithFinalizer()
				cc := newCephClusterUnstructured(ec, "Ready", "", "")
				ccc := newCephClusterConnectionUnstructured()
				objs := []client.Object{ec, cc, ccc}
				for _, esc := range escs {
					objs = append(objs, esc)
				}
				cl := newFakeClient(objs...)
				r := newElasticClusterReconciler(cl)

				latest := markDeleting(ctx, cl, testECName)
				res, err := r.reconcileDelete(ctx, latest)
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(BeNumerically(">", 0))

				// Guard fires BEFORE any vendor resource is deleted.
				Expect(cephClusterExists(ctx, cl)).To(BeTrue())
				Expect(cephClusterConnectionExists(ctx, cl)).To(BeTrue())
				Expect(ecReadyReason(ctx, cl, testECName)).To(Equal(v1alpha1.ECReasonStorageClassesExist))

				// Finalizer still held.
				held := &v1alpha1.ElasticCluster{}
				Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, held)).To(Succeed())
				Expect(held.Finalizers).To(ContainElement(external.ECFinalizer))
			},
			Entry("single RBD ESC", []*v1alpha1.ElasticStorageClass{
				newTestElasticStorageClass("rbd-sc", v1alpha1.StorageClassTypeRBD),
			}),
			Entry("single CephFS ESC", []*v1alpha1.ElasticStorageClass{
				newTestElasticStorageClass("fs-sc", v1alpha1.StorageClassTypeCephFS),
			}),
			Entry("both RBD and CephFS ESCs", []*v1alpha1.ElasticStorageClass{
				newTestElasticStorageClass("rbd-sc", v1alpha1.StorageClassTypeRBD),
				newTestElasticStorageClass("fs-sc", v1alpha1.StorageClassTypeCephFS),
			}),
		)

		It("ignores an ESC that references a different cluster", func() {
			ec := ecWithFinalizer()
			foreign := newTestElasticStorageClass("foreign-sc", v1alpha1.StorageClassTypeRBD)
			foreign.Spec.ClusterRef = "other-cluster"
			cl := newFakeClient(ec, foreign,
				newCephClusterUnstructured(ec, "Ready", "", ""),
				newCephClusterConnectionUnstructured(),
			)
			r := newElasticClusterReconciler(cl)

			latest := markDeleting(ctx, cl, testECName)
			_, err := r.reconcileDelete(ctx, latest)
			Expect(err).NotTo(HaveOccurred())

			// No dependent ESC ⇒ teardown proceeded and removed the
			// vendor resources; the EC is gone (finalizer dropped).
			Expect(cephClusterExists(ctx, cl)).To(BeFalse())
			Expect(cephClusterConnectionExists(ctx, cl)).To(BeFalse())
			gone := &v1alpha1.ElasticCluster{}
			err = cl.Get(ctx, types.NamespacedName{Name: testECName}, gone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("deletes CephCluster + CephClusterConnection and drops the finalizer when no ESC remains", func() {
			ec := ecWithFinalizer()
			pv := &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "pv-keep"},
				Spec:       corev1.PersistentVolumeSpec{StorageClassName: "rbd-sc"},
			}
			cl := newFakeClient(ec,
				newCephClusterUnstructured(ec, "Ready", "", ""),
				newCephClusterConnectionUnstructured(),
				pv,
			)
			r := newElasticClusterReconciler(cl)

			latest := markDeleting(ctx, cl, testECName)
			res, err := r.reconcileDelete(ctx, latest)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero())

			Expect(cephClusterExists(ctx, cl)).To(BeFalse())
			Expect(cephClusterConnectionExists(ctx, cl)).To(BeFalse())

			gone := &v1alpha1.ElasticCluster{}
			err = cl.Get(ctx, types.NamespacedName{Name: testECName}, gone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			// PV is left for the operator to clean up by label.
			keep := &corev1.PersistentVolume{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: "pv-keep"}, keep)).To(Succeed())
		})

		It("surfaces VolumesExist while Rook holds the CephCluster with DeletionIsBlocked", func() {
			ec := ecWithFinalizer()
			cl := newFakeClient(ec, rookHeldCephCluster(ec, true))
			r := newElasticClusterReconciler(cl)

			latest := markDeleting(ctx, cl, testECName)
			res, err := r.reconcileDelete(ctx, latest)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))

			Expect(ecReadyReason(ctx, cl, testECName)).To(Equal(v1alpha1.ECReasonVolumesExist))

			// Finalizer retained until the backend is actually gone.
			held := &v1alpha1.ElasticCluster{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, held)).To(Succeed())
			Expect(held.Finalizers).To(ContainElement(external.ECFinalizer))

			// The teardown message must not leak Rook entity names.
			cond := apimeta.FindStatusCondition(held.Status.Conditions, v1alpha1.ECConditionReady)
			Expect(cond).NotTo(BeNil())
			expectNoVendorLeak(cond.Message)
		})

		It("surfaces Terminating while only the CephClusterConnection is still being removed", func() {
			ec := ecWithFinalizer()
			cl := newFakeClient(ec, rookHeldCephClusterConnection())
			r := newElasticClusterReconciler(cl)

			latest := markDeleting(ctx, cl, testECName)
			res, err := r.reconcileDelete(ctx, latest)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))

			// CephCluster never existed; the connection is still held,
			// so the finalizer must be retained.
			Expect(cephClusterConnectionExists(ctx, cl)).To(BeTrue())
			Expect(ecReadyReason(ctx, cl, testECName)).To(Equal(v1alpha1.ECReasonTerminating))
			held := &v1alpha1.ElasticCluster{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, held)).To(Succeed())
			Expect(held.Finalizers).To(ContainElement(external.ECFinalizer))
		})

		It("surfaces Terminating while the CephCluster is still being removed (no block condition)", func() {
			ec := ecWithFinalizer()
			cl := newFakeClient(ec, rookHeldCephCluster(ec, false))
			r := newElasticClusterReconciler(cl)

			latest := markDeleting(ctx, cl, testECName)
			res, err := r.reconcileDelete(ctx, latest)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(ecReadyReason(ctx, cl, testECName)).To(Equal(v1alpha1.ECReasonTerminating))
		})

		It("is idempotent once the backend is gone", func() {
			ec := ecWithFinalizer()
			cl := newFakeClient(ec)
			r := newElasticClusterReconciler(cl)

			latest := markDeleting(ctx, cl, testECName)
			_, err := r.reconcileDelete(ctx, latest)
			Expect(err).NotTo(HaveOccurred())

			gone := &v1alpha1.ElasticCluster{}
			err = cl.Get(ctx, types.NamespacedName{Name: testECName}, gone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Describe("Reconcile entry point", func() {
		It("adds the finalizer on the normal path", func() {
			ec := newTestElasticCluster()
			cl := newFakeClient(ec, newTestNode("node-a"))
			r := newElasticClusterReconciler(cl)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}})
			Expect(err).NotTo(HaveOccurred())

			latest := &v1alpha1.ElasticCluster{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())
			Expect(latest.Finalizers).To(ContainElement(external.ECFinalizer))
		})

		It("routes a terminating ElasticCluster to reconcileDelete (not reconcileNormal)", func() {
			ec := ecWithFinalizer()
			// A dependent ESC makes the teardown observable and proves
			// reconcileNormal did not run (it would try to provision the
			// CephCluster instead of blocking on the guard).
			esc := newTestElasticStorageClass("rbd-sc", v1alpha1.StorageClassTypeRBD)
			cl := newFakeClient(ec, esc)
			r := newElasticClusterReconciler(cl)

			markDeleting(ctx, cl, testECName)
			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(ecReadyReason(ctx, cl, testECName)).To(Equal(v1alpha1.ECReasonStorageClassesExist))
			Expect(cephClusterExists(ctx, cl)).To(BeFalse())
		})
	})
})

var _ = Describe("externalDeletionBlocked", func() {
	It("is true only for DeletionIsBlocked=True", func() {
		cc := newCephClusterUnstructured(newTestElasticCluster(), "Ready", "", "")
		Expect(externalDeletionBlocked(cc)).To(BeFalse())

		_ = unstructured.SetNestedSlice(cc.Object, []interface{}{
			map[string]interface{}{"type": rookDeletionBlockedConditionType, "status": "False"},
		}, "status", "conditions")
		Expect(externalDeletionBlocked(cc)).To(BeFalse())

		_ = unstructured.SetNestedSlice(cc.Object, []interface{}{
			map[string]interface{}{"type": rookDeletionBlockedConditionType, "status": "True"},
		}, "status", "conditions")
		Expect(externalDeletionBlocked(cc)).To(BeTrue())
	})
})

var _ = Describe("dependentESCNames", func() {
	ctx := context.Background()

	It("returns only the ESCs referencing the cluster, sorted", func() {
		ec := newTestElasticCluster()
		mine1 := newTestElasticStorageClass("b-sc", v1alpha1.StorageClassTypeRBD)
		mine2 := newTestElasticStorageClass("a-sc", v1alpha1.StorageClassTypeCephFS)
		other := newTestElasticStorageClass("c-sc", v1alpha1.StorageClassTypeRBD)
		other.Spec.ClusterRef = "another"
		cl := newFakeClient(ec, mine1, mine2, other)
		r := newElasticClusterReconciler(cl)

		names, err := r.dependentESCNames(ctx, ec)
		Expect(err).NotTo(HaveOccurred())
		Expect(names).To(Equal([]string{"a-sc", "b-sc"}))
	})
})
