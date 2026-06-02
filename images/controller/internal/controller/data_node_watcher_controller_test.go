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

	It("returns an error and labels nothing when the config has an unknown key (strict)", func() {
		// `node_selector` (snake_case) is a typo for `nodeSelector`. Under
		// non-strict parsing it would leave the selector empty and label
		// every Node in the cluster; strict parsing turns it into a hard
		// error so the typo is surfaced instead of silently fanning out.
		secret := configSecret("node_selector:\n  role: storage")
		cl := newFakeClient(
			secret,
			nodeWithLabels("node-a", nil),
			nodeWithLabels("node-b", map[string]string{"role": "storage"}),
		)
		r := newDataNodeWatcherReconciler(cl)

		_, err := r.Reconcile(ctx, dataReq)
		Expect(err).To(HaveOccurred())

		Expect(getNode(ctx, cl, "node-a").Labels).NotTo(HaveKey(DataNodeSelectorLabel))
		Expect(getNode(ctx, cl, "node-b").Labels).NotTo(HaveKey(DataNodeSelectorLabel))
	})

	It("labels nothing and clears stale labels when the selector matches no Node", func() {
		secret := configSecret("nodeSelector:\n  role: storage")
		cl := newFakeClient(
			secret,
			// No Node matches role=storage; node-stale still carries the
			// label from a previous broader selector and must be cleared.
			nodeWithLabels("node-compute", map[string]string{"role": "compute"}),
			nodeWithLabels("node-stale", map[string]string{
				"role":                "compute",
				DataNodeSelectorLabel: "",
			}),
		)
		r := newDataNodeWatcherReconciler(cl)

		_, err := r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())

		Expect(getNode(ctx, cl, "node-compute").Labels).NotTo(HaveKey(DataNodeSelectorLabel))
		Expect(getNode(ctx, cl, "node-stale").Labels).NotTo(HaveKey(DataNodeSelectorLabel))
	})

	It("preserves labels written by a concurrent writer (merge patch)", func() {
		// Label node-a, then simulate another controller adding an
		// unrelated label, then rotate the selector so node-a is cleared.
		// The merge patch must drop only DataNodeSelectorLabel and leave
		// the concurrently-added label intact.
		cl := newFakeClient(
			configSecret("nodeSelector:\n  role: storage"),
			nodeWithLabels("node-a", map[string]string{"role": "storage"}),
		)
		r := newDataNodeWatcherReconciler(cl)

		_, err := r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(getNode(ctx, cl, "node-a").Labels).To(HaveKeyWithValue(DataNodeSelectorLabel, ""))

		// Concurrent writer adds an unrelated label.
		concurrent := getNode(ctx, cl, "node-a")
		concurrent.Labels["other-operator/managed"] = "yes"
		Expect(cl.Update(ctx, concurrent)).To(Succeed())

		// Rotate the selector so node-a no longer matches: the stale-label
		// path removes our label via merge patch.
		rotated := &corev1.Secret{}
		Expect(cl.Get(ctx, types.NamespacedName{Name: config.ConfigSecretName, Namespace: "d8-sds-elastic"}, rotated)).To(Succeed())
		rotated.Data["config"] = []byte("nodeSelector:\n  role: compute")
		Expect(cl.Update(ctx, rotated)).To(Succeed())

		_, err = r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())

		n := getNode(ctx, cl, "node-a")
		Expect(n.Labels).NotTo(HaveKey(DataNodeSelectorLabel))
		Expect(n.Labels).To(HaveKeyWithValue("other-operator/managed", "yes"))
		Expect(n.Labels).To(HaveKeyWithValue("role", "storage"))
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

	It("is idempotent: a second reconcile with the same payload is a no-op", func() {
		secret := configSecret("nodeSelector:\n  role: storage")
		cl := newFakeClient(
			secret,
			nodeWithLabels("node-storage", map[string]string{"role": "storage"}),
		)
		r := newDataNodeWatcherReconciler(cl)

		_, err := r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())

		// resourceVersion after the first (label-applying) reconcile.
		rvAfterFirst := getNode(ctx, cl, "node-storage").ResourceVersion
		Expect(getNode(ctx, cl, "node-storage").Labels).To(HaveKeyWithValue(DataNodeSelectorLabel, ""))

		_, err = r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())

		// No write happened on the second pass: the already-labelled Node
		// is short-circuited and the stale-label sweep skips it too, so
		// resourceVersion is unchanged.
		Expect(getNode(ctx, cl, "node-storage").ResourceVersion).To(Equal(rvAfterFirst))
	})

	It("recovers from external label drift on the next reconcile", func() {
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

		// Drift 1: an admin manually strips the label off a matching Node.
		stripped := getNode(ctx, cl, "node-storage")
		delete(stripped.Labels, DataNodeSelectorLabel)
		Expect(cl.Update(ctx, stripped)).To(Succeed())

		// Drift 2: an admin manually adds the label to a non-matching Node.
		bogus := getNode(ctx, cl, "node-compute")
		bogus.Labels[DataNodeSelectorLabel] = ""
		Expect(cl.Update(ctx, bogus)).To(Succeed())

		_, err = r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())

		// Both drifts are corrected: matching Node re-labelled, the
		// non-matching one cleared.
		Expect(getNode(ctx, cl, "node-storage").Labels).To(HaveKeyWithValue(DataNodeSelectorLabel, ""))
		Expect(getNode(ctx, cl, "node-compute").Labels).NotTo(HaveKey(DataNodeSelectorLabel))
	})

	It("re-balances labels when the selector changes on a Secret write", func() {
		cl := newFakeClient(
			configSecret("nodeSelector:\n  role: storage"),
			nodeWithLabels("node-storage", map[string]string{"role": "storage"}),
			nodeWithLabels("node-compute", map[string]string{"role": "compute"}),
		)
		r := newDataNodeWatcherReconciler(cl)

		_, err := r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(getNode(ctx, cl, "node-storage").Labels).To(HaveKeyWithValue(DataNodeSelectorLabel, ""))
		Expect(getNode(ctx, cl, "node-compute").Labels).NotTo(HaveKey(DataNodeSelectorLabel))

		// Operator rotates the selector to role=compute.
		rotated := &corev1.Secret{}
		Expect(cl.Get(ctx, types.NamespacedName{Name: config.ConfigSecretName, Namespace: "d8-sds-elastic"}, rotated)).To(Succeed())
		rotated.Data["config"] = []byte("nodeSelector:\n  role: compute")
		Expect(cl.Update(ctx, rotated)).To(Succeed())

		_, err = r.Reconcile(ctx, dataReq)
		Expect(err).NotTo(HaveOccurred())

		Expect(getNode(ctx, cl, "node-storage").Labels).NotTo(HaveKey(DataNodeSelectorLabel))
		Expect(getNode(ctx, cl, "node-compute").Labels).To(HaveKeyWithValue(DataNodeSelectorLabel, ""))
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

	It("rejects an unknown key (strict parsing)", func() {
		// A misspelled `node_selector` is an unknown field: strict parsing
		// must reject it instead of returning an empty (match-everything)
		// selector.
		_, err := nodeSelectorFromSecret(configSecret("node_selector:\n  role: storage"))
		Expect(err).To(HaveOccurred())
	})
})
