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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/deckhouse/sds-elastic/images/controller/pkg/config"
)

// newDataNodeWatcherReconciler wires a DataNodeWatcherReconciler around the
// shared fake client + test logger + test config helpers from
// controller_suite_test.go, matching the style of the other reconciler
// factories in this package.
func newDataNodeWatcherReconciler(cl client.Client) *DataNodeWatcherReconciler {
	cfg := newTestCfg()
	cfg.ConfigSecretName = config.ConfigSecretName
	cfg.RequeueSecretInterval = 0
	return &DataNodeWatcherReconciler{
		Client: cl,
		Log:    newTestLogger(),
		Cfg:    cfg,
	}
}

func nodeWithLabels(name string, lbls map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: lbls,
		},
	}
}

func configSecret(payload string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.ConfigSecretName,
			Namespace: "d8-sds-elastic",
		},
		Data: map[string][]byte{
			"config": []byte(payload),
		},
	}
}

func getNode(ctx context.Context, cl client.Client, name string) *corev1.Node {
	n := &corev1.Node{}
	Expect(cl.Get(ctx, types.NamespacedName{Name: name}, n)).To(Succeed())
	return n
}

var _ = Describe("DataNodeWatcherReconciler", func() {
	var ctx = context.Background()

	dataReq := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      config.ConfigSecretName,
			Namespace: "d8-sds-elastic",
		},
	}

	It("labels every Node when the selector is empty", func() {
		secret := configSecret("nodeSelector: {}")
		cl := newFakeClient(
			secret,
			nodeWithLabels("node-a", nil),
			nodeWithLabels("node-b", map[string]string{"role": "compute"}),
		)
		r := newDataNodeWatcherReconciler(cl)

		_, err := r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())

		Expect(getNode(ctx, cl, "node-a").Labels).To(HaveKeyWithValue(DataNodeSelectorLabel, ""))
		Expect(getNode(ctx, cl, "node-b").Labels).To(HaveKeyWithValue(DataNodeSelectorLabel, ""))
	})

	It("labels only Nodes matching nodeSelector and leaves others untouched", func() {
		secret := configSecret("nodeSelector:\n  role: storage")
		cl := newFakeClient(
			secret,
			nodeWithLabels("node-storage", map[string]string{"role": "storage"}),
			nodeWithLabels("node-compute", map[string]string{"role": "compute"}),
		)
		r := newDataNodeWatcherReconciler(cl)

		_, err := r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())

		Expect(getNode(ctx, cl, "node-storage").Labels).To(HaveKeyWithValue(DataNodeSelectorLabel, ""))
		Expect(getNode(ctx, cl, "node-compute").Labels).NotTo(HaveKey(DataNodeSelectorLabel))
	})

	It("removes the label from Nodes that no longer match the selector", func() {
		secret := configSecret("nodeSelector:\n  role: storage")
		// node-prev still carries the label from a previous broader
		// selector but no longer matches the new one.
		cl := newFakeClient(
			secret,
			nodeWithLabels("node-prev", map[string]string{
				"role":                "compute",
				DataNodeSelectorLabel: "",
			}),
			nodeWithLabels("node-now", map[string]string{"role": "storage"}),
		)
		r := newDataNodeWatcherReconciler(cl)

		_, err := r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())

		Expect(getNode(ctx, cl, "node-prev").Labels).NotTo(HaveKey(DataNodeSelectorLabel))
		Expect(getNode(ctx, cl, "node-now").Labels).To(HaveKeyWithValue(DataNodeSelectorLabel, ""))
	})

	It("does not touch Nodes that already carry the label and still match", func() {
		secret := configSecret("nodeSelector:\n  role: storage")
		cl := newFakeClient(
			secret,
			nodeWithLabels("node-stable", map[string]string{
				"role":                "storage",
				DataNodeSelectorLabel: "",
				"other":               "preserved",
			}),
		)
		r := newDataNodeWatcherReconciler(cl)

		_, err := r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())

		n := getNode(ctx, cl, "node-stable")
		Expect(n.Labels).To(HaveKeyWithValue(DataNodeSelectorLabel, ""))
		Expect(n.Labels).To(HaveKeyWithValue("role", "storage"))
		Expect(n.Labels).To(HaveKeyWithValue("other", "preserved"))
	})

	It("ignores Secret events outside the controller namespace / name", func() {
		// nodeSelector matches no labels; a buggy reconcile would clear
		// the label and fail this expectation. Reconcile must early-return
		// for unrelated Secret keys.
		cl := newFakeClient(
			nodeWithLabels("node-x", map[string]string{
				"role":                "storage",
				DataNodeSelectorLabel: "",
			}),
		)
		r := newDataNodeWatcherReconciler(cl)

		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "some-other-secret",
				Namespace: "d8-sds-elastic",
			},
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      config.ConfigSecretName,
				Namespace: "kube-system",
			},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(getNode(ctx, cl, "node-x").Labels).To(HaveKeyWithValue(DataNodeSelectorLabel, ""))
	})

	It("does not error when the config Secret is missing", func() {
		cl := newFakeClient(
			nodeWithLabels("node-only", map[string]string{"role": "storage"}),
		)
		r := newDataNodeWatcherReconciler(cl)

		_, err := r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(getNode(ctx, cl, "node-only").Labels).NotTo(HaveKey(DataNodeSelectorLabel))
	})

	It("treats an empty `config` payload as an empty selector (label everyone)", func() {
		secret := configSecret("")
		cl := newFakeClient(
			secret,
			nodeWithLabels("node-anywhere", nil),
		)
		r := newDataNodeWatcherReconciler(cl)

		_, err := r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())

		Expect(getNode(ctx, cl, "node-anywhere").Labels).To(HaveKeyWithValue(DataNodeSelectorLabel, ""))
	})
})

var _ = Describe("nodeSelectorFromSecret", func() {
	It("returns an empty selector for a missing data key", func() {
		sel, err := nodeSelectorFromSecret(&corev1.Secret{})
		Expect(err).NotTo(HaveOccurred())
		Expect(sel).To(BeEmpty())
	})

	It("returns an empty selector for an explicit empty map", func() {
		sel, err := nodeSelectorFromSecret(configSecret("nodeSelector: {}"))
		Expect(err).NotTo(HaveOccurred())
		Expect(sel).To(BeEmpty())
	})

	It("parses a non-trivial selector", func() {
		sel, err := nodeSelectorFromSecret(configSecret("nodeSelector:\n  role: storage\n  zone: a"))
		Expect(err).NotTo(HaveOccurred())
		Expect(sel).To(Equal(map[string]string{"role": "storage", "zone": "a"}))
	})

	It("rejects malformed YAML", func() {
		_, err := nodeSelectorFromSecret(configSecret("not-yaml: [unclosed"))
		Expect(err).To(HaveOccurred())
	})
})
