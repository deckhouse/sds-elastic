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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

const escTestName = "pool-demo"

// escWithFinalizer returns a test ESC of the given type carrying ESCFinalizer.
func escWithFinalizer(name string, scType v1alpha1.StorageClassType) *v1alpha1.ElasticStorageClass {
	esc := newTestElasticStorageClass(name, scType)
	esc.Finalizers = []string{external.ESCFinalizer}
	return esc
}

// markDeletingESC deletes the ESC so the fake client stamps a
// DeletionTimestamp (the finalizer keeps it around), then returns the
// refreshed copy.
func markDeletingESC(ctx context.Context, cl client.Client, name string) *v1alpha1.ElasticStorageClass {
	stale := &v1alpha1.ElasticStorageClass{}
	ExpectWithOffset(1, cl.Get(ctx, types.NamespacedName{Name: name}, stale)).To(Succeed())
	ExpectWithOffset(1, cl.Delete(ctx, stale)).To(Succeed())
	latest := &v1alpha1.ElasticStorageClass{}
	ExpectWithOffset(1, cl.Get(ctx, types.NamespacedName{Name: name}, latest)).To(Succeed())
	ExpectWithOffset(1, latest.DeletionTimestamp).NotTo(BeNil())
	return latest
}

func escReadyReason(ctx context.Context, cl client.Client, name string) string {
	latest := &v1alpha1.ElasticStorageClass{}
	ExpectWithOffset(1, cl.Get(ctx, types.NamespacedName{Name: name}, latest)).To(Succeed())
	if latest.Status == nil {
		return ""
	}
	cond := apimeta.FindStatusCondition(latest.Status.Conditions, v1alpha1.ESCConditionReady)
	if cond == nil {
		return ""
	}
	return cond.Reason
}

func boundPV(name, scName string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PersistentVolumeSpec{StorageClassName: scName},
		Status:     corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
}

func releasedPV(name, scName string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PersistentVolumeSpec{StorageClassName: scName},
		Status:     corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
}

// rookHeldCephBlockPool builds a CephBlockPool fixture that Rook still
// holds (its own finalizer keeps it around after Delete), optionally
// carrying the PoolDeletionIsBlocked=True condition (non-empty pool).
func rookHeldCephBlockPool(name string, blocked bool) *unstructured.Unstructured {
	pool := newCephBlockPoolUnstructured(name, testNamespace, "Ready")
	pool.SetFinalizers([]string{"cephblockpool.ceph.rook.io"})
	if blocked {
		_ = unstructured.SetNestedSlice(pool.Object, []interface{}{
			map[string]interface{}{"type": rookPoolDeletionBlockedConditionType, "status": "True"},
		}, "status", "conditions")
	}
	return pool
}

// rookHeldCephFilesystem builds a CephFilesystem fixture preserved-on-delete
// (mirroring the builder default) that Rook still holds via its finalizer,
// optionally carrying the DeletionIsBlocked=True condition (subvolumes left).
func rookHeldCephFilesystem(name string, blocked bool) *unstructured.Unstructured {
	fs := &unstructured.Unstructured{}
	fs.SetGroupVersionKind(external.CephFilesystemGVK)
	fs.SetName(name)
	fs.SetNamespace(testNamespace)
	fs.SetFinalizers([]string{"cephfilesystem.ceph.rook.io"})
	fs.Object["spec"] = map[string]interface{}{
		"preserveFilesystemOnDelete": true,
	}
	fs.Object["status"] = map[string]interface{}{"phase": "Ready"}
	if blocked {
		_ = unstructured.SetNestedSlice(fs.Object, []interface{}{
			map[string]interface{}{"type": rookDeletionBlockedConditionType, "status": "True"},
		}, "status", "conditions")
	}
	return fs
}

func getExternalForTest(ctx context.Context, cl client.Client, gvk schema.GroupVersionKind, nn types.NamespacedName) (*unstructured.Unstructured, bool) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := cl.Get(ctx, nn, u)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return u, true
}

var _ = Describe("ElasticStorageClass teardown", func() {
	ctx := context.Background()

	Describe("ensureESCFinalizer", func() {
		It("adds the finalizer when missing", func() {
			esc := newTestElasticStorageClass(escTestName, v1alpha1.StorageClassTypeRBD)
			cl := newFakeClient(esc)
			r := newElasticStorageClassReconciler(cl)

			Expect(r.ensureESCFinalizer(ctx, esc)).To(Succeed())

			latest := &v1alpha1.ElasticStorageClass{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: escTestName}, latest)).To(Succeed())
			Expect(latest.Finalizers).To(ContainElement(external.ESCFinalizer))
		})

		It("is a no-op when already present", func() {
			esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeRBD)
			cl := newFakeClient(esc)
			r := newElasticStorageClassReconciler(cl)

			Expect(r.ensureESCFinalizer(ctx, esc)).To(Succeed())

			latest := &v1alpha1.ElasticStorageClass{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: escTestName}, latest)).To(Succeed())
			Expect(latest.Finalizers).To(Equal([]string{external.ESCFinalizer}))
		})
	})

	Describe("reconcileDeleteESC", func() {
		It("returns immediately when the finalizer is absent", func() {
			esc := newTestElasticStorageClass(escTestName, v1alpha1.StorageClassTypeRBD)
			cl := newFakeClient(esc)
			r := newElasticStorageClassReconciler(cl)

			res, err := r.reconcileDeleteESC(ctx, esc)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero())
		})

		It("hard-blocks while a bound PV references the StorageClass", func() {
			esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeRBD)
			pool := newCephBlockPoolUnstructured(escTestName, testNamespace, "Ready")
			csc := newCephStorageClassUnstructured(escTestName, "Created")
			cl := newFakeClient(esc, pool, csc, boundPV("pv-bound", escTestName))
			r := newElasticStorageClassReconciler(cl)

			latest := markDeletingESC(ctx, cl, escTestName)
			res, err := r.reconcileDeleteESC(ctx, latest)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))

			Expect(escReadyReason(ctx, cl, escTestName)).To(Equal(v1alpha1.ESCReasonBoundVolumesExist))
			// Guard fires before anything is deleted.
			_, poolFound := getExternalForTest(ctx, cl, external.CephBlockPoolGVK, types.NamespacedName{Namespace: testNamespace, Name: escTestName})
			Expect(poolFound).To(BeTrue())
			_, cscFound := getExternalForTest(ctx, cl, external.CephStorageClassGVK, types.NamespacedName{Name: escTestName})
			Expect(cscFound).To(BeTrue())
		})

		It("does not let the force annotation bypass the bound-PV guard", func() {
			esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeRBD)
			esc.Annotations = map[string]string{external.ESCForceDeleteAnnotation: "true"}
			pool := rookHeldCephBlockPool(escTestName, true)
			cl := newFakeClient(esc, pool, boundPV("pv-bound", escTestName))
			r := newElasticStorageClassReconciler(cl)

			latest := markDeletingESC(ctx, cl, escTestName)
			_, err := r.reconcileDeleteESC(ctx, latest)
			Expect(err).NotTo(HaveOccurred())

			Expect(escReadyReason(ctx, cl, escTestName)).To(Equal(v1alpha1.ESCReasonBoundVolumesExist))
			// Force annotation must NOT have been propagated while bound.
			got, found := getExternalForTest(ctx, cl, external.CephBlockPoolGVK, types.NamespacedName{Namespace: testNamespace, Name: escTestName})
			Expect(found).To(BeTrue())
			Expect(got.GetAnnotations()).NotTo(HaveKey(external.RookForceDeletionAnnotation))
		})

		It("does not block on Released PVs", func() {
			esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeRBD)
			cl := newFakeClient(esc, releasedPV("pv-released", escTestName))
			r := newElasticStorageClassReconciler(cl)

			latest := markDeletingESC(ctx, cl, escTestName)
			res, err := r.reconcileDeleteESC(ctx, latest)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero())

			gone := &v1alpha1.ElasticStorageClass{}
			err = cl.Get(ctx, types.NamespacedName{Name: escTestName}, gone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("deletes an empty RBD pool without force and drops the finalizer", func() {
			esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeRBD)
			pool := newCephBlockPoolUnstructured(escTestName, testNamespace, "Ready")
			csc := newCephStorageClassUnstructured(escTestName, "Created")
			cl := newFakeClient(esc, pool, csc)
			r := newElasticStorageClassReconciler(cl)

			latest := markDeletingESC(ctx, cl, escTestName)
			res, err := r.reconcileDeleteESC(ctx, latest)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero())

			_, poolFound := getExternalForTest(ctx, cl, external.CephBlockPoolGVK, types.NamespacedName{Namespace: testNamespace, Name: escTestName})
			Expect(poolFound).To(BeFalse())
			_, cscFound := getExternalForTest(ctx, cl, external.CephStorageClassGVK, types.NamespacedName{Name: escTestName})
			Expect(cscFound).To(BeFalse())

			gone := &v1alpha1.ElasticStorageClass{}
			err = cl.Get(ctx, types.NamespacedName{Name: escTestName}, gone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("surfaces DataPresentInPool for a non-empty RBD pool without force", func() {
			esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeRBD)
			pool := rookHeldCephBlockPool(escTestName, true)
			cl := newFakeClient(esc, pool)
			r := newElasticStorageClassReconciler(cl)

			latest := markDeletingESC(ctx, cl, escTestName)
			res, err := r.reconcileDeleteESC(ctx, latest)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))

			Expect(escReadyReason(ctx, cl, escTestName)).To(Equal(v1alpha1.ESCReasonDataPresentInPool))
			// No force annotation propagated; pool retained.
			got, found := getExternalForTest(ctx, cl, external.CephBlockPoolGVK, types.NamespacedName{Namespace: testNamespace, Name: escTestName})
			Expect(found).To(BeTrue())
			Expect(got.GetAnnotations()).NotTo(HaveKey(external.RookForceDeletionAnnotation))

			// The teardown message must not leak Rook entity names.
			held := &v1alpha1.ElasticStorageClass{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: escTestName}, held)).To(Succeed())
			expectNoVendorLeak(apimeta.FindStatusCondition(held.Status.Conditions, v1alpha1.ESCConditionReady).Message)
		})

		It("propagates rook.io/force-deletion when the force annotation is set", func() {
			esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeRBD)
			esc.Annotations = map[string]string{external.ESCForceDeleteAnnotation: "true"}
			pool := rookHeldCephBlockPool(escTestName, true)
			cl := newFakeClient(esc, pool)
			r := newElasticStorageClassReconciler(cl)

			latest := markDeletingESC(ctx, cl, escTestName)
			res, err := r.reconcileDeleteESC(ctx, latest)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))

			got, found := getExternalForTest(ctx, cl, external.CephBlockPoolGVK, types.NamespacedName{Namespace: testNamespace, Name: escTestName})
			Expect(found).To(BeTrue())
			Expect(got.GetAnnotations()).To(HaveKeyWithValue(external.RookForceDeletionAnnotation, "true"))
			// With force in flight the reason is the neutral Terminating.
			Expect(escReadyReason(ctx, cl, escTestName)).To(Equal(v1alpha1.ESCReasonTerminating))
		})

		It("flips CephFS to destroy-on-delete and tears it down", func() {
			esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeCephFS)
			fs := rookHeldCephFilesystem(escTestName, false)
			cl := newFakeClient(esc, fs)
			r := newElasticStorageClassReconciler(cl)

			latest := markDeletingESC(ctx, cl, escTestName)
			res, err := r.reconcileDeleteESC(ctx, latest)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))

			// preserveFilesystemOnDelete must have been flipped to false.
			got, found := getExternalForTest(ctx, cl, external.CephFilesystemGVK, types.NamespacedName{Namespace: testNamespace, Name: escTestName})
			Expect(found).To(BeTrue())
			preserve, _, _ := unstructured.NestedBool(got.Object, "spec", "preserveFilesystemOnDelete")
			Expect(preserve).To(BeFalse())
			preservePools, _, _ := unstructured.NestedBool(got.Object, "spec", "preservePoolsOnDelete")
			Expect(preservePools).To(BeFalse())
			Expect(escReadyReason(ctx, cl, escTestName)).To(Equal(v1alpha1.ESCReasonTerminating))
		})

		It("surfaces FilesystemNotEmpty when the filesystem still has volumes", func() {
			esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeCephFS)
			fs := rookHeldCephFilesystem(escTestName, true)
			cl := newFakeClient(esc, fs)
			r := newElasticStorageClassReconciler(cl)

			latest := markDeletingESC(ctx, cl, escTestName)
			res, err := r.reconcileDeleteESC(ctx, latest)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))

			Expect(escReadyReason(ctx, cl, escTestName)).To(Equal(v1alpha1.ESCReasonFilesystemNotEmpty))
			held := &v1alpha1.ElasticStorageClass{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: escTestName}, held)).To(Succeed())
			expectNoVendorLeak(apimeta.FindStatusCondition(held.Status.Conditions, v1alpha1.ESCConditionReady).Message)
		})

		It("deletes an empty CephFS filesystem and drops the finalizer", func() {
			esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeCephFS)
			fs := &unstructured.Unstructured{}
			fs.SetGroupVersionKind(external.CephFilesystemGVK)
			fs.SetName(escTestName)
			fs.SetNamespace(testNamespace)
			fs.Object["spec"] = map[string]interface{}{"preserveFilesystemOnDelete": true}
			cl := newFakeClient(esc, fs)
			r := newElasticStorageClassReconciler(cl)

			latest := markDeletingESC(ctx, cl, escTestName)
			res, err := r.reconcileDeleteESC(ctx, latest)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero())

			_, found := getExternalForTest(ctx, cl, external.CephFilesystemGVK, types.NamespacedName{Namespace: testNamespace, Name: escTestName})
			Expect(found).To(BeFalse())
			gone := &v1alpha1.ElasticStorageClass{}
			err = cl.Get(ctx, types.NamespacedName{Name: escTestName}, gone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("is idempotent once the backend is gone", func() {
			esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeRBD)
			cl := newFakeClient(esc)
			r := newElasticStorageClassReconciler(cl)

			latest := markDeletingESC(ctx, cl, escTestName)
			_, err := r.reconcileDeleteESC(ctx, latest)
			Expect(err).NotTo(HaveOccurred())

			gone := &v1alpha1.ElasticStorageClass{}
			err = cl.Get(ctx, types.NamespacedName{Name: escTestName}, gone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Describe("Reconcile entry point", func() {
		It("adds the finalizer on the normal path", func() {
			esc := newTestElasticStorageClass(escTestName, v1alpha1.StorageClassTypeRBD)
			ec := newTestElasticCluster()
			cl := newFakeClient(esc, ec)
			r := newElasticStorageClassReconciler(cl)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: escTestName}})
			Expect(err).NotTo(HaveOccurred())

			latest := &v1alpha1.ElasticStorageClass{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: escTestName}, latest)).To(Succeed())
			Expect(latest.Finalizers).To(ContainElement(external.ESCFinalizer))
		})

		It("routes a terminating ESC to reconcileDeleteESC (bound-PV guard)", func() {
			esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeRBD)
			cl := newFakeClient(esc, boundPV("pv-bound", escTestName))
			r := newElasticStorageClassReconciler(cl)

			markDeletingESC(ctx, cl, escTestName)
			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: escTestName}})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(escReadyReason(ctx, cl, escTestName)).To(Equal(v1alpha1.ESCReasonBoundVolumesExist))
		})
	})
})

var _ = Describe("boundPVCount", func() {
	ctx := context.Background()

	It("counts only Bound PVs of the matching StorageClass", func() {
		esc := newTestElasticStorageClass(escTestName, v1alpha1.StorageClassTypeRBD)
		cl := newFakeClient(
			boundPV("pv-1", escTestName),
			boundPV("pv-2", escTestName),
			releasedPV("pv-3", escTestName),
			boundPV("pv-other", "different-sc"),
		)
		r := newElasticStorageClassReconciler(cl)

		n, err := r.boundPVCount(ctx, esc)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(2))
	})
})

var _ = Describe("enqueueDeletingESCByPV", func() {
	ctx := context.Background()

	It("enqueues the matching ESC only while it is terminating", func() {
		esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeRBD)
		cl := newFakeClient(esc)
		r := newElasticStorageClassReconciler(cl)
		markDeletingESC(ctx, cl, escTestName)

		reqs := r.enqueueDeletingESCByPV(ctx, boundPV("pv-1", escTestName))
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].Name).To(Equal(escTestName))
	})

	It("does not enqueue a non-terminating ESC", func() {
		esc := newTestElasticStorageClass(escTestName, v1alpha1.StorageClassTypeRBD)
		cl := newFakeClient(esc)
		r := newElasticStorageClassReconciler(cl)

		Expect(r.enqueueDeletingESCByPV(ctx, boundPV("pv-1", escTestName))).To(BeEmpty())
	})

	It("does not enqueue when the PV has no StorageClass", func() {
		cl := newFakeClient()
		r := newElasticStorageClassReconciler(cl)

		Expect(r.enqueueDeletingESCByPV(ctx, boundPV("pv-1", ""))).To(BeEmpty())
	})

	It("does not enqueue when no ESC matches the StorageClass", func() {
		cl := newFakeClient()
		r := newElasticStorageClassReconciler(cl)

		Expect(r.enqueueDeletingESCByPV(ctx, boundPV("pv-1", "missing-sc"))).To(BeEmpty())
	})

	It("ignores non-PV objects", func() {
		cl := newFakeClient()
		r := newElasticStorageClassReconciler(cl)

		Expect(r.enqueueDeletingESCByPV(ctx, newTestElasticCluster())).To(BeEmpty())
	})
})

var _ = Describe("ElasticStorageClass teardown — residual branches", func() {
	ctx := context.Background()

	It("surfaces Terminating while only the CephStorageClass is still being removed", func() {
		esc := escWithFinalizer(escTestName, v1alpha1.StorageClassTypeRBD)
		// CephStorageClass held by csi-ceph finalizer (still terminating);
		// the RBD pool never existed.
		csc := newCephStorageClassUnstructured(escTestName, "Created")
		csc.SetFinalizers([]string{"storage.deckhouse.io/csi-ceph"})
		cl := newFakeClient(esc, csc)
		r := newElasticStorageClassReconciler(cl)

		latest := markDeletingESC(ctx, cl, escTestName)
		res, err := r.reconcileDeleteESC(ctx, latest)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))

		Expect(escReadyReason(ctx, cl, escTestName)).To(Equal(v1alpha1.ESCReasonTerminating))
		held := &v1alpha1.ElasticStorageClass{}
		Expect(cl.Get(ctx, types.NamespacedName{Name: escTestName}, held)).To(Succeed())
		Expect(held.Finalizers).To(ContainElement(external.ESCFinalizer))
	})

	It("drops the finalizer for an unsupported type once the CephStorageClass is gone", func() {
		esc := escWithFinalizer(escTestName, v1alpha1.StorageClassType("Bogus"))
		csc := newCephStorageClassUnstructured(escTestName, "Created")
		cl := newFakeClient(esc, csc)
		r := newElasticStorageClassReconciler(cl)

		latest := markDeletingESC(ctx, cl, escTestName)
		res, err := r.reconcileDeleteESC(ctx, latest)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero())

		gone := &v1alpha1.ElasticStorageClass{}
		err = cl.Get(ctx, types.NamespacedName{Name: escTestName}, gone)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
})
