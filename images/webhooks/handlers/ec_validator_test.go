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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// dynClient builds a fake dynamic.Interface preloaded with the supplied
// unstructured objects. The list-kind hints are required because the
// fake client cannot infer them from a bare schema.
//
// Shared between EC and ESC validator tests; ESC tests need
// elasticClusterGVR so the HighRedundancy preflight can Get the parent
// EC the same way it does in production.
func dynClient(objs ...runtime.Object) dynamic.Interface {
	sch := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		blockDeviceGVR:    "BlockDeviceList",
		nodeGVR:           "NodeList",
		elasticClusterGVR: "ElasticClusterList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(sch, gvrToListKind, objs...)
}

func bdUnstructured(name, ownerEC, nodeName string, lbls map[string]string) *unstructured.Unstructured {
	out := &unstructured.Unstructured{}
	out.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "storage.deckhouse.io",
		Version: "v1alpha1",
		Kind:    "BlockDevice",
	})
	out.SetName(name)
	full := map[string]string{}
	for k, v := range lbls {
		full[k] = v
	}
	if ownerEC != "" {
		full[ecClusterLabel] = ownerEC
	}
	out.SetLabels(full)
	if nodeName != "" {
		_ = unstructured.SetNestedField(out.Object, nodeName, "status", "nodeName")
	}
	return out
}

func nodeUnstructured(name string, lbls map[string]string) *unstructured.Unstructured {
	out := &unstructured.Unstructured{}
	out.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Node",
	})
	out.SetName(name)
	out.SetLabels(lbls)
	return out
}

// matchLabels is a compact helper for `metav1.LabelSelector.matchLabels`
// shaped maps (the format unstructured objects round-trip to).
func matchLabels(kv map[string]string) map[string]interface{} {
	ml := map[string]interface{}{}
	for k, v := range kv {
		ml[k] = v
	}
	return map[string]interface{}{"matchLabels": ml}
}

// ecDemoName is the shared ElasticCluster name used by every test in
// this file. Centralising it keeps the dynamic-client lookups in the
// validator (which match by name) and the substring assertions on the
// returned admission messages in sync.
const ecDemoName = "ec-demo"

func newECUnstructured(spec map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("storage.deckhouse.io/v1alpha1")
	obj.SetKind("ElasticCluster")
	obj.SetName(ecDemoName)
	obj.Object["spec"] = spec
	return obj
}

func ecSpec(bdSel, nodeSel map[string]interface{}, network map[string]interface{}) map[string]interface{} {
	storage := map[string]interface{}{}
	if bdSel != nil {
		storage["blockDeviceSelector"] = bdSel
	}
	if nodeSel != nil {
		storage["nodeSelector"] = nodeSel
	}
	spec := map[string]interface{}{
		"storage": storage,
	}
	if network != nil {
		spec["network"] = network
	}
	return spec
}

var _ = Describe("ElasticClusterValidate", func() {
	var ctx = context.Background()

	defaultBd := matchLabels(map[string]string{"role": "elastic-osd"})
	defaultNode := matchLabels(map[string]string{"role": "storage"})
	defaultNet := map[string]interface{}{"public": "10.0.0.0/24", "cluster": "10.1.0.0/24"}

	validate := func(dyn dynamic.Interface, op model.AdmissionReviewOp, oldObj, newObj *unstructured.Unstructured) (bool, string, error) {
		fn := NewElasticClusterValidator(dyn)
		res, err := fn(ctx, admissionReview(op, oldObj), newObj)
		if err != nil {
			return false, "", err
		}
		return res.Valid, res.Message, nil
	}

	It("accepts CREATE unconditionally", func() {
		dyn := dynClient()
		ec := newECUnstructured(ecSpec(defaultBd, defaultNode, defaultNet))
		valid, _, err := validate(dyn, model.OperationCreate, nil, ec)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeTrue())
	})

	It("accepts UPDATE without changes", func() {
		dyn := dynClient(
			nodeUnstructured("node-a", map[string]string{"role": "storage"}),
		)
		spec := ecSpec(defaultBd, defaultNode, defaultNet)
		oldEC := newECUnstructured(spec)
		updated := newECUnstructured(spec)
		valid, msg, err := validate(dyn, model.OperationUpdate, oldEC, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeTrue(), "msg=%q", msg)
	})

	It("rejects UPDATE that mutates spec.network", func() {
		dyn := dynClient(
			nodeUnstructured("node-a", map[string]string{"role": "storage"}),
		)
		oldEC := newECUnstructured(ecSpec(defaultBd, defaultNode, defaultNet))
		newNet := map[string]interface{}{"public": "10.99.0.0/24", "cluster": "10.1.0.0/24"}
		updated := newECUnstructured(ecSpec(defaultBd, defaultNode, newNet))
		valid, msg, err := validate(dyn, model.OperationUpdate, oldEC, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("spec.network is immutable"))
	})

	It("accepts UPDATE that widens blockDeviceSelector with no conflicts", func() {
		dyn := dynClient(
			nodeUnstructured("node-a", map[string]string{"role": "storage"}),
			// Already-adopted BD: matches both selectors.
			bdUnstructured("bd-existing", ecDemoName, "node-a", map[string]string{"role": "elastic-osd"}),
			// Free BD eligible to be pulled in by the widened selector.
			bdUnstructured("bd-fresh", "", "node-a", map[string]string{"role": "expanded"}),
		)
		oldEC := newECUnstructured(ecSpec(defaultBd, defaultNode, defaultNet))
		widened := map[string]interface{}{
			"matchExpressions": []interface{}{
				map[string]interface{}{
					"key":      "role",
					"operator": "In",
					"values":   []interface{}{"elastic-osd", "expanded"},
				},
			},
		}
		updated := newECUnstructured(ecSpec(widened, defaultNode, defaultNet))
		valid, msg, err := validate(dyn, model.OperationUpdate, oldEC, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeTrue(), "msg=%q", msg)
	})

	It("rejects UPDATE that narrows blockDeviceSelector and orphans an adopted BD", func() {
		dyn := dynClient(
			nodeUnstructured("node-a", map[string]string{"role": "storage"}),
			bdUnstructured("bd-old", ecDemoName, "node-a", map[string]string{"role": "elastic-osd"}),
		)
		oldEC := newECUnstructured(ecSpec(defaultBd, defaultNode, defaultNet))
		narrowed := matchLabels(map[string]string{"role": "elastic-osd-future"})
		updated := newECUnstructured(ecSpec(narrowed, defaultNode, defaultNet))
		valid, msg, err := validate(dyn, model.OperationUpdate, oldEC, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("orphan adopted BlockDevices"))
		Expect(msg).To(ContainSubstring("bd-old"))
	})

	It("rejects UPDATE that narrows nodeSelector so an adopted BD lives on a non-matching Node", func() {
		dyn := dynClient(
			nodeUnstructured("node-a", map[string]string{"role": "storage"}),
			nodeUnstructured("node-b", map[string]string{"role": "compute"}),
			bdUnstructured("bd-on-b", ecDemoName, "node-b", map[string]string{"role": "elastic-osd"}),
		)
		oldEC := newECUnstructured(ecSpec(defaultBd, matchLabels(map[string]string{"role": "compute"}), defaultNet))
		updated := newECUnstructured(ecSpec(defaultBd, defaultNode, defaultNet))
		valid, msg, err := validate(dyn, model.OperationUpdate, oldEC, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("orphan adopted BlockDevices"))
		Expect(msg).To(ContainSubstring("bd-on-b"))
	})

	It("rejects UPDATE that widens blockDeviceSelector into a BD owned by another EC", func() {
		dyn := dynClient(
			nodeUnstructured("node-a", map[string]string{"role": "storage"}),
			bdUnstructured("bd-mine", ecDemoName, "node-a", map[string]string{"role": "elastic-osd"}),
			bdUnstructured("bd-claimed", "ec-other", "node-a", map[string]string{"role": "expanded"}),
		)
		oldEC := newECUnstructured(ecSpec(defaultBd, defaultNode, defaultNet))
		widened := map[string]interface{}{
			"matchExpressions": []interface{}{
				map[string]interface{}{
					"key":      "role",
					"operator": "In",
					"values":   []interface{}{"elastic-osd", "expanded"},
				},
			},
		}
		updated := newECUnstructured(ecSpec(widened, defaultNode, defaultNet))
		valid, msg, err := validate(dyn, model.OperationUpdate, oldEC, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("already owned by another ElasticCluster"))
		Expect(msg).To(ContainSubstring("bd-claimed"))
		Expect(msg).To(ContainSubstring("ec-other"))
	})

	It("rejects UPDATE with an invalid LabelSelector (In with no values)", func() {
		dyn := dynClient(
			nodeUnstructured("node-a", map[string]string{"role": "storage"}),
		)
		oldEC := newECUnstructured(ecSpec(defaultBd, defaultNode, defaultNet))
		bad := map[string]interface{}{
			"matchExpressions": []interface{}{
				map[string]interface{}{
					"key":      "role",
					"operator": "In",
					"values":   []interface{}{},
				},
			},
		}
		updated := newECUnstructured(ecSpec(bad, defaultNode, defaultNet))
		valid, msg, err := validate(dyn, model.OperationUpdate, oldEC, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("blockDeviceSelector"))
	})
})
