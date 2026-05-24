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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe("webhook helpers", func() {
	Describe("decodeUnstructured", func() {
		It("returns nil for empty input", func() {
			obj, err := decodeUnstructured(nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(obj).To(BeNil())
		})

		It("decodes valid JSON", func() {
			raw := []byte(`{"spec":{"fsid":"abc"}}`)
			obj, err := decodeUnstructured(raw)
			Expect(err).NotTo(HaveOccurred())
			fsid, _, _ := unstructured.NestedString(obj.Object, "spec", "fsid")
			Expect(fsid).To(Equal("abc"))
		})

		It("errors on malformed JSON", func() {
			_, err := decodeUnstructured([]byte(`{not-json`))
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("reject", func() {
		It("returns Valid=false with message", func() {
			res := reject("denied")
			Expect(res.Valid).To(BeFalse())
			Expect(res.Message).To(Equal("denied"))
		})
	})
})
