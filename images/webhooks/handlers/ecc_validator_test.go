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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe("ElasticClusterCredentialValidate", func() {
	var ctx = context.Background()

	validate := func(op model.AdmissionReviewOp, oldObj, newObj *unstructured.Unstructured) (bool, string, error) {
		res, err := ElasticClusterCredentialValidate(ctx, admissionReview(op, oldObj), newObj)
		if err != nil {
			return false, "", err
		}
		return res.Valid, res.Message, nil
	}

	It("accepts CREATE unconditionally", func() {
		obj := newECCUnstructured(eccSpec("", "", ""))
		valid, _, err := validate(model.OperationCreate, nil, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeTrue())
	})

	It("accepts UPDATE from empty fsid to populated", func() {
		old := newECCUnstructured(eccSpec("", "", ""))
		new := newECCUnstructured(eccSpec("fsid-1", "mon", "admin"))
		valid, _, err := validate(model.OperationUpdate, old, new)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeTrue())
	})

	It("accepts UPDATE when fsid unchanged", func() {
		old := newECCUnstructured(eccSpec("fsid-1", "mon", "admin"))
		new := newECCUnstructured(eccSpec("fsid-1", "mon-rotated", "admin-rotated"))
		valid, _, err := validate(model.OperationUpdate, old, new)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeTrue())
	})

	It("rejects UPDATE when fsid changes", func() {
		old := newECCUnstructured(eccSpec("fsid-1", "mon", "admin"))
		new := newECCUnstructured(eccSpec("fsid-2", "mon", "admin"))
		valid, msg, err := validate(model.OperationUpdate, old, new)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("immutable"))
	})

	It("rejects UPDATE when fsid is cleared", func() {
		old := newECCUnstructured(eccSpec("fsid-1", "mon", "admin"))
		new := newECCUnstructured(eccSpec("", "mon", "admin"))
		valid, _, err := validate(model.OperationUpdate, old, new)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
	})

	It("fail-closes on unexpected object type", func() {
		_, err := ElasticClusterCredentialValidate(ctx, admissionReview(model.OperationUpdate, nil), &corev1.Pod{})
		Expect(err).To(HaveOccurred())
	})
})
