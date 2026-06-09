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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

// vendorEntitySubstrings enumerates the vendor (Rook / csi-ceph)
// identifiers that must never surface in a user-facing
// .status.conditions[].message. Operators consume EC/ESC conditions in
// domain terms ("storage backend", "storage pool", ...); the underlying
// Rook/csi-ceph resource kinds and the rook-ceph-mon Secret/ConfigMap are
// an internal implementation detail kept to the controller logs only.
// Matched case-insensitively against the lowercased message: Rook emits
// the full version banner ("ceph version 19.2.3 (...) squid") in
// lowercase, so a case-sensitive "Ceph" check would miss it.
var vendorEntitySubstrings = []string{"ceph", "rook", "rook-ceph-mon", "csi-ceph"}

func expectNoVendorLeak(msg string) {
	low := strings.ToLower(msg)
	for _, s := range vendorEntitySubstrings {
		ExpectWithOffset(1, strings.Contains(low, s)).
			To(BeFalse(), "condition message must not leak vendor entity %q: %q", s, msg)
	}
}

var _ = Describe("condition message sanitization (no vendor entity leak)", func() {
	ctx := context.Background()

	Describe("EC upgrade probe messages", func() {
		desiredImage := "registry.example.com/ceph:v19.2.3"
		desiredVersion := v1alpha1.DefaultCephVersion
		otherImage := "registry.example.com/ceph:v20.2.1"

		It("single converged version", func() {
			cc := newCephClusterUnstructured(newTestElasticCluster(), "Ready", "v19.2.3", desiredImage)
			withCephClusterCephStatus(cc, "HEALTH_OK", "", "", 0, 0, 0, "", nil, map[string]map[string]int32{
				"overall": {cephVerString1923: 9},
			})
			expectNoVendorLeak(probeCephUpgradeState(cc, desiredImage, desiredVersion).Msg)
		})

		It("mixed versions mid-roll", func() {
			cc := newCephClusterUnstructured(newTestElasticCluster(), "Progressing", "20.2.1-0", otherImage)
			withCephClusterCephStatus(cc, "HEALTH_OK", "", "", 0, 0, 0, "", nil, map[string]map[string]int32{
				"overall": {cephVerString1923: 4, cephVerString2021: 5},
			})
			expectNoVendorLeak(probeCephUpgradeState(cc, otherImage, "v20.2.1").Msg)
		})

		It("queued bump on healthy cluster", func() {
			cc := newCephClusterUnstructured(newTestElasticCluster(), "Ready", "v19.2.3", otherImage)
			withCephClusterCephStatus(cc, "HEALTH_OK", "", "", 0, 0, 0, "", nil, nil)
			expectNoVendorLeak(probeCephUpgradeState(cc, desiredImage, desiredVersion).Msg)
		})

		It("no running version published", func() {
			ec := newTestElasticCluster()
			cc := newCephClusterUnstructured(ec, "Progressing", "", desiredImage)
			expectNoVendorLeak(probeCephUpgradeState(cc, desiredImage, desiredVersion).Msg)
		})

		It("fallback version mismatch", func() {
			cc := newCephClusterUnstructured(newTestElasticCluster(), "Ready", "v18.2.0", desiredImage)
			expectNoVendorLeak(probeCephUpgradeState(cc, desiredImage, desiredVersion).Msg)
		})
	})

	Describe("EC credentials stage messages", func() {
		It("waiting for the mon secret", func() {
			r := newElasticClusterReconciler(newFakeClient())
			ec := newTestElasticCluster()
			_, msg, err := r.ensureCredentials(ctx, ec, newECStatusBuilder(ec))
			Expect(err).NotTo(HaveOccurred())
			expectNoVendorLeak(msg)
		})

		It("waiting for the mon endpoints configmap", func() {
			r := newElasticClusterReconciler(newFakeClient(newRookMonSecret("fsid", "admin", "mon")))
			ec := newTestElasticCluster()
			_, msg, err := r.ensureCredentials(ctx, ec, newECStatusBuilder(ec))
			Expect(err).NotTo(HaveOccurred())
			expectNoVendorLeak(msg)
		})

		It("captured credentials happy path", func() {
			r := newElasticClusterReconciler(newFakeClient(
				newRookMonSecret("fsid", "admin", "mon"),
				newRookMonEndpointsCM("a=10.0.0.1:6789,b=10.0.0.2:6789", "b"),
			))
			ec := newTestElasticCluster()
			done, msg, err := r.ensureCredentials(ctx, ec, newECStatusBuilder(ec))
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			expectNoVendorLeak(msg)
		})
	})

	Describe("ESC pool/filesystem/storageclass stage messages", func() {
		It("RBD pool not ready", func() {
			esc := newTestElasticStorageClass("rbd-sc", v1alpha1.StorageClassTypeRBD)
			ec := ecWithCephClusterReady(newTestElasticCluster())
			r := newElasticStorageClassReconciler(newFakeClient(esc, ec))
			_, msg, err := r.ensureRBDPool(ctx, esc)
			Expect(err).NotTo(HaveOccurred())
			expectNoVendorLeak(msg)
		})

		It("CephFS not ready", func() {
			esc := newTestElasticStorageClass("fs-sc", v1alpha1.StorageClassTypeCephFS)
			ec := ecWithCephClusterReady(newTestElasticCluster())
			r := newElasticStorageClassReconciler(newFakeClient(esc, ec))
			_, msg, err := r.ensureCephFS(ctx, esc)
			Expect(err).NotTo(HaveOccurred())
			expectNoVendorLeak(msg)
		})

		It("storage class not ready", func() {
			esc := newTestElasticStorageClass("rbd-sc", v1alpha1.StorageClassTypeRBD)
			r := newElasticStorageClassReconciler(newFakeClient(esc))
			_, msg, err := r.ensureCsiStorageClass(ctx, esc)
			Expect(err).NotTo(HaveOccurred())
			expectNoVendorLeak(msg)
		})

		It("waiting for parent EC readiness", func() {
			esc := newTestElasticStorageClass("rbd-sc", v1alpha1.StorageClassTypeRBD)
			r := newElasticStorageClassReconciler(newFakeClient(esc))
			_, msg, err := r.getReadyEC(ctx, esc)
			Expect(err).NotTo(HaveOccurred())
			expectNoVendorLeak(msg)
		})
	})
})
