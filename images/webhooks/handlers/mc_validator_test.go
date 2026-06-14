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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

func boolPtr(b bool) *bool { return &b }

var _ = Describe("ModuleConfigValidate", func() {
	var ctx = context.Background()

	validate := func(dyn dynamic.Interface, op model.AdmissionReviewOp, mc *unstructured.Unstructured) (bool, string, error) {
		fn := NewModuleConfigValidator(dyn)
		res, err := fn(ctx, &model.AdmissionReview{Operation: op}, mc)
		if err != nil {
			return false, "", err
		}
		return res.Valid, res.Message, nil
	}

	Context("when the module stays enabled", func() {
		It("accepts spec.enabled=true even with ElasticClusters present", func() {
			dyn := dynClient(newECObject("ec-a"))
			mc := newMCUnstructured(moduleConfigName, boolPtr(true), nil)
			valid, msg, err := validate(dyn, model.OperationUpdate, mc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeTrue(), "msg=%q", msg)
		})

		It("accepts a ModuleConfig without spec.enabled (settings-only)", func() {
			dyn := dynClient(newECObject("ec-a"))
			mc := newMCUnstructured(moduleConfigName, nil, nil)
			valid, msg, err := validate(dyn, model.OperationUpdate, mc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeTrue(), "msg=%q", msg)
		})
	})

	Context("when disabling the module (spec.enabled=false)", func() {
		It("accepts the disable when no ElasticClusters exist", func() {
			dyn := dynClient()
			mc := newMCUnstructured(moduleConfigName, boolPtr(false), nil)
			valid, msg, err := validate(dyn, model.OperationUpdate, mc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeTrue(), "msg=%q", msg)
		})

		It("rejects the disable when ElasticClusters exist and names them", func() {
			dyn := dynClient(newECObject("ec-beta"), newECObject("ec-alpha"))
			mc := newMCUnstructured(moduleConfigName, boolPtr(false), nil)
			valid, msg, err := validate(dyn, model.OperationUpdate, mc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeFalse())
			Expect(msg).To(ContainSubstring("cannot disable the sds-elastic module"))
			// names are sorted for a stable message
			Expect(msg).To(ContainSubstring("ec-alpha, ec-beta"))
			Expect(msg).To(ContainSubstring(ForceDisableAnnotation))
		})

		It("rejects the disable on CREATE too when ElasticClusters exist", func() {
			dyn := dynClient(newECObject("ec-a"))
			mc := newMCUnstructured(moduleConfigName, boolPtr(false), nil)
			valid, _, err := validate(dyn, model.OperationCreate, mc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeFalse())
		})
	})

	Context("force-disable escape hatch", func() {
		It("allows the disable when the force annotation is true", func() {
			dyn := dynClient(newECObject("ec-a"), newECObject("ec-b"))
			mc := newMCUnstructured(moduleConfigName, boolPtr(false), map[string]string{
				ForceDisableAnnotation: "true",
			})
			valid, msg, err := validate(dyn, model.OperationUpdate, mc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeTrue(), "msg=%q", msg)
		})

		It("still rejects when the force annotation has a non-true value", func() {
			dyn := dynClient(newECObject("ec-a"))
			mc := newMCUnstructured(moduleConfigName, boolPtr(false), map[string]string{
				ForceDisableAnnotation: "yes",
			})
			valid, _, err := validate(dyn, model.OperationUpdate, mc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeFalse())
		})
	})

	Context("scope and operation guards", func() {
		It("ignores a different ModuleConfig even when disabling with ECs present", func() {
			dyn := dynClient(newECObject("ec-a"))
			mc := newMCUnstructured("some-other-module", boolPtr(false), nil)
			valid, msg, err := validate(dyn, model.OperationUpdate, mc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeTrue(), "msg=%q", msg)
		})

		It("never blocks DELETE of the ModuleConfig", func() {
			dyn := dynClient(newECObject("ec-a"))
			mc := newMCUnstructured(moduleConfigName, boolPtr(false), nil)
			valid, msg, err := validate(dyn, model.OperationDelete, mc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeTrue(), "msg=%q", msg)
		})

		It("never blocks an UPDATE on a ModuleConfig already being deleted", func() {
			dyn := dynClient(newECObject("ec-a"))
			mc := newMCUnstructured(moduleConfigName, boolPtr(false), nil)
			now := metav1.Now()
			mc.SetDeletionTimestamp(&now)
			valid, msg, err := validate(dyn, model.OperationUpdate, mc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeTrue(), "msg=%q", msg)
		})
	})

	Context("fail-closed on unexpected input", func() {
		It("returns an error for a non-unstructured admission object", func() {
			dyn := dynClient()
			fn := NewModuleConfigValidator(dyn)
			_, err := fn(ctx, &model.AdmissionReview{Operation: model.OperationUpdate}, &metav1.PartialObjectMetadata{})
			Expect(err).To(HaveOccurred())
		})
	})
})
