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

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

var _ = Describe("ensureUpgrade", func() {
	var (
		ctx    = context.Background()
		ec     = newTestElasticCluster()
		status *ecStatusBuilder
		r      *ElasticClusterReconciler
	)

	BeforeEach(func() {
		status = newECStatusBuilder(ec)
	})

	It("returns done when running version matches desired", func() {
		cephImage := newTestCfg().CephImages[v1alpha1.DefaultCephVersion]
		cc := newCephClusterUnstructured(ec, "Ready", "v19.2.3", cephImage)
		cl := newFakeClient(cc)
		r = newElasticClusterReconciler(cl)

		done, inProgress, msg, err := r.ensureUpgrade(ctx, ec, status)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
		Expect(inProgress).To(BeFalse())
		Expect(msg).To(ContainSubstring("running ceph version"))
		Expect(status.cephVersion.Running).To(Equal("v19.2.3"))
	})

	It("returns in-progress when running version mismatches", func() {
		cephImage := newTestCfg().CephImages[v1alpha1.DefaultCephVersion]
		cc := newCephClusterUnstructured(ec, "Ready", "v18.2.0", cephImage)
		cl := newFakeClient(cc)
		r = newElasticClusterReconciler(cl)

		done, inProgress, msg, err := r.ensureUpgrade(ctx, ec, status)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(inProgress).To(BeTrue())
		Expect(msg).To(ContainSubstring("running=v18.2.0"))
		Expect(msg).To(ContainSubstring("desired=" + v1alpha1.DefaultCephVersion))
	})

	It("returns in-progress when CephCluster is not yet visible", func() {
		cl := newFakeClient()
		r = newElasticClusterReconciler(cl)

		done, inProgress, msg, err := r.ensureUpgrade(ctx, ec, status)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(inProgress).To(BeFalse())
		Expect(msg).To(ContainSubstring("not yet visible"))
	})
})
