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

package tests

import (
	"context"
	"fmt"
	"io"

	. "github.com/onsi/ginkgo/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgokube "k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

// diagControllerLogTail is the per-container log tail emitted on failure. 200
// lines covers the most recent EC/ESC reconcile cycle while staying readable.
const diagControllerLogTail = int64(200)

// dumpFailedSpecDiagnostics emits a self-contained, best-effort dump describing
// why a spec failed: EC/ESC conditions, the renamed-group Rook CR statuses, and
// the sds-elastic controller/webhook logs. Framed with markers so it can be
// sliced out of the run log. Every probe is best-effort; individual failures
// are logged inline and do not abort the rest of the dump.
//
// Callers invoke this only when CurrentSpecReport().Failed(); this function
// does NOT re-check that condition.
func dumpFailedSpecDiagnostics(ctx context.Context) {
	fmt.Fprintln(GinkgoWriter, "\n========== diagnostics on failure ==========")
	fmt.Fprintf(GinkgoWriter, "spec      : %s\n", CurrentSpecReport().FullText())
	fmt.Fprintf(GinkgoWriter, "ec        : %s\n", suiteCfg.ecName)
	fmt.Fprintf(GinkgoWriter, "rook ns   : %s\n", suiteCfg.rookNamespace)

	dumpElasticConditions(ctx)
	dumpRookStackStatus(ctx)
	dumpControllerLogs(ctx, diagControllerLogTail)

	fmt.Fprintln(GinkgoWriter, "========== /diagnostics ==========")
}

// dumpElasticConditions prints status.conditions of the shared ElasticCluster
// and every ElasticStorageClass currently present.
func dumpElasticConditions(ctx context.Context) {
	fmt.Fprintf(GinkgoWriter, "\n--- ElasticCluster %s conditions ---\n", suiteCfg.ecName)
	if ec, err := suiteDyn.Resource(storagekube.ElasticClusterGVR).Get(ctx, suiteCfg.ecName, metav1.GetOptions{}); err != nil {
		fmt.Fprintf(GinkgoWriter, "  get EC: %v\n", err)
	} else {
		dumpConditions(ec)
	}

	fmt.Fprintln(GinkgoWriter, "\n--- ElasticStorageClasses ---")
	escs, err := suiteDyn.Resource(storagekube.ElasticStorageClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "  list ESC: %v\n", err)
		return
	}
	for i := range escs.Items {
		esc := &escs.Items[i]
		fmt.Fprintf(GinkgoWriter, "\n  ESC %s:\n", esc.GetName())
		dumpConditions(esc)
	}
}

func dumpConditions(obj *unstructured.Unstructured) {
	conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		fmt.Fprintln(GinkgoWriter, "  <no status.conditions>")
		return
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(m, "type")
		s, _, _ := unstructured.NestedString(m, "status")
		r, _, _ := unstructured.NestedString(m, "reason")
		msg, _, _ := unstructured.NestedString(m, "message")
		fmt.Fprintf(GinkgoWriter, "  %-24s %-6s reason=%s msg=%s\n", t, s, r, msg)
	}
}

// dumpRookStackStatus emits status of the renamed-group (internal.sdselastic.
// deckhouse.io) CephCluster / CephBlockPool / CephFilesystem in the Rook
// namespace — the fastest signal for why a bootstrap or teardown hangs.
func dumpRookStackStatus(ctx context.Context) {
	gvrs := []schema.GroupVersionResource{
		storagekube.ElasticRookCephClusterGVR,
		storagekube.ElasticRookCephBlockPoolGVR,
		storagekube.ElasticRookCephFilesystemGVR,
	}
	for _, gvr := range gvrs {
		list, err := suiteDyn.Resource(gvr).Namespace(suiteCfg.rookNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "\nlist %s in %s: %v\n", gvr.Resource, suiteCfg.rookNamespace, err)
			continue
		}
		for i := range list.Items {
			obj := list.Items[i]
			status, _, _ := unstructured.NestedMap(obj.Object, "status")
			fmt.Fprintf(GinkgoWriter, "\n--- %s %s/%s status ---\n", gvr.Resource, obj.GetNamespace(), obj.GetName())
			if status == nil {
				fmt.Fprintln(GinkgoWriter, "  <empty>")
				continue
			}
			if y, err := yaml.Marshal(status); err == nil {
				GinkgoWriter.Write(y)
			} else {
				fmt.Fprintf(GinkgoWriter, "  marshal status: %v\n", err)
			}
		}
	}
}

// dumpControllerLogs tails logs of every pod in the sds-elastic module
// namespace (controller + webhook + Rook operator), per container.
func dumpControllerLogs(ctx context.Context, tail int64) {
	cs, err := clientgokube.NewForConfig(suiteRestCfg)
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "\ndiag: build clientset: %v\n", err)
		return
	}

	pods, err := cs.CoreV1().Pods(d8ElasticNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "\nlist pods in %s: %v\n", d8ElasticNamespace, err)
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		fmt.Fprintf(GinkgoWriter, "\n--- pod %s/%s on %s phase=%s ---\n",
			pod.Namespace, pod.Name, pod.Spec.NodeName, pod.Status.Phase)
		for _, c := range pod.Spec.Containers {
			dumpContainerLogs(ctx, cs, pod.Namespace, pod.Name, c.Name, tail)
		}
	}
}

func dumpContainerLogs(ctx context.Context, cs clientgokube.Interface, namespace, podName, container string, tail int64) {
	req := cs.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: container,
		TailLines: &tail,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "logs %s/%s [%s]: %v\n", namespace, podName, container, err)
		return
	}
	defer stream.Close()

	fmt.Fprintf(GinkgoWriter, "\n--- logs %s/%s container=%s tail=%d ---\n", namespace, podName, container, tail)
	if _, err := io.Copy(GinkgoWriter, stream); err != nil {
		fmt.Fprintf(GinkgoWriter, "\n  copy logs: %v\n", err)
	}
}
