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

var _ = Describe("ElasticStorageClassValidate", func() {
	var ctx = context.Background()

	validate := func(op model.AdmissionReviewOp, oldObj, newObj *unstructured.Unstructured) (bool, string, error) {
		res, err := ElasticStorageClassValidate(ctx, admissionReview(op, oldObj), newObj)
		if err != nil {
			return false, "", err
		}
		return res.Valid, res.Message, nil
	}

	It("rejects reserved OSD StorageClass name on CREATE", func() {
		obj := newESCUnstructured(reservedOSDStorageClassName, escSpec("demo", storageClassTypeRBD, ""))
		valid, msg, err := validate(model.OperationCreate, nil, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("reserved"))
	})

	It("rejects RBD + ErasureCodedCompact on CREATE", func() {
		obj := newESCUnstructured("pool", escSpec("demo", storageClassTypeRBD, replicationErasureCodedCompact))
		valid, _, err := validate(model.OperationCreate, nil, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
	})

	It("accepts valid CREATE", func() {
		obj := newESCUnstructured("pool", escSpec("demo", storageClassTypeRBD, "ConsistencyAndAvailability"))
		valid, _, err := validate(model.OperationCreate, nil, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeTrue())
	})

	It("rejects clusterRef mutation on UPDATE", func() {
		old := newESCUnstructured("pool", escSpec("ec-a", storageClassTypeRBD, ""))
		new := newESCUnstructured("pool", escSpec("ec-b", storageClassTypeRBD, ""))
		valid, msg, err := validate(model.OperationUpdate, old, new)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("clusterRef"))
	})

	It("accepts unchanged clusterRef on UPDATE", func() {
		spec := escSpec("demo", storageClassTypeRBD, "")
		old := newESCUnstructured("pool", spec)
		new := newESCUnstructured("pool", spec)
		valid, _, err := validate(model.OperationUpdate, old, new)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeTrue())
	})

	It("rejects type mutation on UPDATE", func() {
		old := newESCUnstructured("pool", escSpec("demo", storageClassTypeRBD, ""))
		new := newESCUnstructured("pool", escSpec("demo", "CephFS", ""))
		valid, _, err := validate(model.OperationUpdate, old, new)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
	})

	It("rejects replication mutation on UPDATE", func() {
		old := newESCUnstructured("pool", escSpec("demo", storageClassTypeRBD, "ConsistencyAndAvailability"))
		new := newESCUnstructured("pool", escSpec("demo", storageClassTypeRBD, "AvailabilityWithoutConsistency"))
		valid, msg, err := validate(model.OperationUpdate, old, new)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("replication"))
	})

	It("fail-closes on unexpected object type", func() {
		_, err := ElasticStorageClassValidate(ctx, admissionReview(model.OperationCreate, nil), &corev1.Pod{})
		Expect(err).To(HaveOccurred())
	})
})
