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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var _ = Describe("shared helpers", func() {
	Describe("mergeLabels", func() {
		It("merges desired over existing", func() {
			got := mergeLabels(
				map[string]string{"a": "1", "b": "old"},
				map[string]string{"b": "new", "c": "3"},
			)
			Expect(got).To(Equal(map[string]string{
				"a": "1",
				"b": "new",
				"c": "3",
			}))
		})

		It("handles nil maps", func() {
			Expect(mergeLabels(nil, map[string]string{"k": "v"})).To(Equal(map[string]string{"k": "v"}))
			Expect(mergeLabels(map[string]string{"k": "v"}, nil)).To(Equal(map[string]string{"k": "v"}))
		})
	})

	Describe("parseMonEndpoints", func() {
		It("parses, deduplicates, and sorts endpoints", func() {
			data := "a=10.0.0.1:6789, b=10.0.0.2:6789, c=10.0.0.1:6789"
			Expect(parseMonEndpoints(data)).To(Equal([]string{
				"10.0.0.1:6789",
				"10.0.0.2:6789",
			}))
		})

		It("returns nil for empty input", func() {
			Expect(parseMonEndpoints("")).To(BeNil())
		})
	})

	Describe("isNoMatchErr", func() {
		It("detects NoMatchError", func() {
			err := &apimeta.NoResourceMatchError{
				PartialResource: schema.GroupVersionResource{Group: "internal.ceph.rook.io", Resource: "cephclusters"},
			}
			Expect(isNoMatchErr(err)).To(BeTrue())
		})

		It("returns false for nil", func() {
			Expect(isNoMatchErr(nil)).To(BeFalse())
		})
	})
})
