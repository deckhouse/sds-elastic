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
)

var _ = Describe("ensureCredentials", func() {
	var (
		ctx    = context.Background()
		ec     = newTestElasticCluster()
		status *ecStatusBuilder
		r      *ElasticClusterReconciler
	)

	BeforeEach(func() {
		status = newECStatusBuilder(ec)
	})

	It("populates all status fields atomically on happy path", func() {
		cl := newFakeClient(
			newRookMonSecret("fsid-abc", "admin-key", "mon-key"),
			newRookMonEndpointsCM("a=10.0.0.1:6789,b=10.0.0.2:6789", "b"),
		)
		r = newElasticClusterReconciler(cl)

		done, msg, err := r.ensureCredentials(ctx, ec, status)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
		Expect(msg).To(ContainSubstring("2 endpoints"))
		Expect(msg).NotTo(ContainSubstring("mon"))

		Expect(status.cephFSID).To(Equal("fsid-abc"))
		Expect(status.monEndpoints).To(Equal([]string{"10.0.0.1:6789", "10.0.0.2:6789"}))
		Expect(status.monMaxID).To(Equal("b"))
		Expect(status.credentialsRef).NotTo(BeNil())
		Expect(status.credentialsRef.Name).To(Equal(testECName))
	})

	It("does not populate status when Secret is missing", func() {
		cl := newFakeClient(newRookMonEndpointsCM("a=10.0.0.1:6789", "a"))
		r = newElasticClusterReconciler(cl)

		done, msg, err := r.ensureCredentials(ctx, ec, status)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(msg).To(ContainSubstring("waiting for cluster credentials"))
		Expect(msg).NotTo(ContainSubstring("rook-ceph-mon"))
		Expect(status.cephFSID).To(BeEmpty())
		Expect(status.credentialsRef).To(BeNil())
	})

	It("does not populate status when ConfigMap is missing", func() {
		cl := newFakeClient(newRookMonSecret("fsid-abc", "admin", "mon"))
		r = newElasticClusterReconciler(cl)

		done, msg, err := r.ensureCredentials(ctx, ec, status)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(msg).To(ContainSubstring("waiting for cluster connection details"))
		Expect(msg).NotTo(ContainSubstring("rook-ceph-mon"))
		Expect(status.cephFSID).To(BeEmpty())
	})
})
