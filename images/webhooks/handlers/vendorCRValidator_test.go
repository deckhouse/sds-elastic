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

package handlers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/slok/kubewebhook/v2/pkg/model"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// vendorCR builds an unstructured CR fixture for the vendor-CR validator.
// An empty namespace mirrors a cluster-scoped object (the validator then
// falls back to the AdmissionReview namespace).
func vendorCR(group, version, kind, namespace, name string) *unstructured.Unstructured {
	out := &unstructured.Unstructured{}
	out.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
	out.SetName(name)
	if namespace != "" {
		out.SetNamespace(namespace)
	}
	return out
}

func vendorReview(username, namespace string) *model.AdmissionReview {
	return &model.AdmissionReview{
		Operation: model.OperationUpdate,
		Namespace: namespace,
		UserInfo:  authenticationv1.UserInfo{Username: username},
	}
}

var _ = Describe("VendorCRValidate", func() {
	ctx := context.Background()

	const (
		rookGroup = "internal.sdselastic.deckhouse.io"
		humanUser = "kubernetes-admin"
		foreignSA = "system:serviceaccount:d8-sds-elastic:foo"
		ourNS     = "d8-sds-elastic"
		otherNS   = "default"
	)

	It("passes through CRs outside the vendor API groups", func() {
		// A Deployment is not a vendor resource: the validator must not
		// gate it regardless of who issues the request.
		obj := vendorCR("apps", "v1", "Deployment", ourNS, "whatever")
		res, err := VendorCRValidate(ctx, vendorReview(humanUser, ourNS), obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Valid).To(BeTrue())
	})

	It("passes through objectbucket.io CRs (no longer a vendor group)", func() {
		// Regression guard for C0: the module ships with the Rook OBC
		// controller disabled and does not vendor objectbucket.io CRDs,
		// so these objects must not be gated by this webhook anymore.
		obj := vendorCR("objectbucket.io", "v1alpha1", "ObjectBucket", ourNS, "ob")
		res, err := VendorCRValidate(ctx, vendorReview(humanUser, ourNS), obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Valid).To(BeTrue())
	})

	It("passes through vendor CRs in a foreign namespace", func() {
		// The webhook only guards the module namespace; a (hypothetical)
		// vendor CR elsewhere is out of scope.
		obj := vendorCR(rookGroup, "v1", "CephCluster", otherNS, "ceph-cluster")
		res, err := VendorCRValidate(ctx, vendorReview(humanUser, otherNS), obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Valid).To(BeTrue())
	})

	It("allows the controller ServiceAccount to manage vendor CRs", func() {
		obj := vendorCR(rookGroup, "v1", "CephCluster", ourNS, "ceph-cluster")
		res, err := VendorCRValidate(ctx, vendorReview(allowedControllerUser, ourNS), obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Valid).To(BeTrue())
	})

	It("allows the operator ServiceAccount to manage vendor CRs", func() {
		obj := vendorCR(rookGroup, "v1", "CephBlockPool", ourNS, "pool")
		res, err := VendorCRValidate(ctx, vendorReview(allowedOperatorUser, ourNS), obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Valid).To(BeTrue())
	})

	It("denies a human user from editing a vendor CR", func() {
		obj := vendorCR(rookGroup, "v1", "CephCluster", ourNS, "ceph-cluster")
		res, err := VendorCRValidate(ctx, vendorReview(humanUser, ourNS), obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Valid).To(BeFalse())
		Expect(res.Message).To(ContainSubstring(humanUser))
	})

	It("denies a foreign ServiceAccount in the module namespace", func() {
		obj := vendorCR(rookGroup, "v1", "CephFilesystem", ourNS, "fs")
		res, err := VendorCRValidate(ctx, vendorReview(foreignSA, ourNS), obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Valid).To(BeFalse())
		Expect(res.Message).To(ContainSubstring(foreignSA))
	})

	It("denies a human user editing other vendor kinds (CephBlockPool)", func() {
		obj := vendorCR(rookGroup, "v1", "CephBlockPool", ourNS, "pool")
		res, err := VendorCRValidate(ctx, vendorReview(humanUser, ourNS), obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Valid).To(BeFalse())
	})

	It("gates cluster-scoped vendor CRs by falling back to the review namespace", func() {
		// A cluster-scoped vendor CR carries no namespace on the object;
		// the validator falls back to arReview.Namespace. With the module
		// namespace there, a human user is still denied.
		obj := vendorCR(rookGroup, "v1", "CephCluster", "", "ceph-cluster")
		res, err := VendorCRValidate(ctx, vendorReview(humanUser, ourNS), obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Valid).To(BeFalse())
	})

	It("does not gate a non-unstructured object (type-assertion guard)", func() {
		// kubewebhook may hand a typed object; the validator must not
		// panic and must return without an error.
		res, err := VendorCRValidate(ctx, vendorReview(humanUser, ourNS), &corev1.Pod{})
		Expect(err).NotTo(HaveOccurred())
		Expect(res).NotTo(BeNil())
	})
})
