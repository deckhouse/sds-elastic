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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

var _ = Describe("ensureCsiCeph", func() {
	var (
		ctx    = context.Background()
		ec     = newTestElasticCluster()
		status *ecStatusBuilder
		r      *ElasticClusterReconciler
	)

	BeforeEach(func() {
		status = newECStatusBuilder(ec)
		status.cephFSID = "fsid-abc"
		status.monEndpoints = []string{"10.0.0.1:6789"}
		status.credentialsRef = &v1alpha1.ElasticClusterCredentialRef{Name: testECName}
	})

	It("upserts CephClusterConnection and waits for Created phase", func() {
		ecc := &v1alpha1.ElasticClusterCredential{
			ObjectMeta: metav1.ObjectMeta{Name: testECName},
			Spec: v1alpha1.ElasticClusterCredentialSpec{
				AdminSecret: "admin-key",
			},
		}
		conn := newCephClusterConnectionUnstructured(testECName, "Created")
		cl := newFakeClient(ecc, conn)
		r = newElasticClusterReconciler(cl)

		done, msg, err := r.ensureCsiCeph(ctx, ec, status)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
		Expect(msg).To(ContainSubstring("ready"))

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(external.CephClusterConnectionGVK)
		Expect(cl.Get(ctx, types.NamespacedName{Name: builder.ECCephClusterConnectionName(ec)}, got)).To(Succeed())
	})

	It("returns in-progress when connection is not yet visible", func() {
		ecc := &v1alpha1.ElasticClusterCredential{
			ObjectMeta: metav1.ObjectMeta{Name: testECName},
			Spec:       v1alpha1.ElasticClusterCredentialSpec{AdminSecret: "admin-key"},
		}
		cl := newFakeClient(ecc)
		r = newElasticClusterReconciler(cl)

		done, msg, err := r.ensureCsiCeph(ctx, ec, status)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(msg).To(ContainSubstring("phase="))
	})
})
