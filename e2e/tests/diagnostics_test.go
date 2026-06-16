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
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgokube "k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

const (
	// diagControllerLogTail is the per-container log tail for the sds-elastic
	// module pods. 200 lines covers the most recent EC/ESC reconcile cycle.
	diagControllerLogTail = int64(200)
	// diagNodeConfiguratorLogTail is a smaller tail for the (chatty, per-node)
	// sds-node-configurator pods, which own LVMVolumeGroup creation.
	diagNodeConfiguratorLogTail = int64(100)
	// diagMaxEvents bounds how many (most recent) Warning events per namespace
	// are printed.
	diagMaxEvents = 30
)

// dumpFailedSpecDiagnostics emits a self-contained, best-effort dump describing
// why a spec failed: EC/ESC conditions, the storage layer the EC depends on
// (LVMVolumeGroup / BlockDevice / LVMLogicalVolume / PV), the renamed-group Rook
// CR statuses, the csi-ceph CRs, recent Warning events, and the
// controller/operator/node-configurator pod statuses and logs. Framed with
// markers so it can be sliced out of the run log. Every probe is best-effort;
// individual failures are logged inline and do not abort the rest of the dump.
//
// Callers invoke this only when CurrentSpecReport().Failed(); this function
// does NOT re-check that condition.
func dumpFailedSpecDiagnostics(ctx context.Context) {
	fmt.Fprintln(GinkgoWriter, "\n========== diagnostics on failure ==========")
	fmt.Fprintf(GinkgoWriter, "spec      : %s\n", CurrentSpecReport().FullText())
	fmt.Fprintf(GinkgoWriter, "ec        : %s\n", suiteCfg.ecName)
	fmt.Fprintf(GinkgoWriter, "rook ns   : %s\n", suiteCfg.rookNamespace)

	dumpElasticConditions(ctx)
	dumpStorageLayer(ctx)
	dumpRookStackStatus(ctx)
	dumpCsiCephCRs(ctx)

	cs, err := clientgokube.NewForConfig(suiteRestCfg)
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "\ndiag: build clientset: %v\n", err)
	} else {
		dumpWarningEvents(ctx, cs)
		dumpNamespaceDiagnostics(ctx, cs, d8ElasticNamespace, diagControllerLogTail)
		dumpNamespaceDiagnostics(ctx, cs, d8NodeConfiguratorNamespace, diagNodeConfiguratorLogTail)
		dumpNodeSummary(ctx, cs)
	}

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

// conditionByType returns the status/reason/message of a single condition type.
func conditionByType(obj *unstructured.Unstructured, condType string) (status, reason, msg string, found bool) {
	conds, ok, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !ok {
		return "", "", "", false
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(m, "type"); t != condType {
			continue
		}
		status, _, _ = unstructured.NestedString(m, "status")
		reason, _, _ = unstructured.NestedString(m, "reason")
		msg, _, _ = unstructured.NestedString(m, "message")
		return status, reason, msg, true
	}
	return "", "", "", false
}

// dumpStorageLayer prints the storage objects the ElasticCluster's StorageReady
// stage depends on: the EC-owned LVMVolumeGroups (with full status for any that
// are not Ready — the decisive signal for a stuck StorageReady), the
// OSD-labelled BlockDevices, and the EC-owned LVMLogicalVolumes / PVs.
func dumpStorageLayer(ctx context.Context) {
	ownerSel := clusterOwnerLabelKey + "=" + suiteCfg.ecName

	fmt.Fprintf(GinkgoWriter, "\n--- LVMVolumeGroups (label %s) ---\n", ownerSel)
	lvgs, err := suiteDyn.Resource(lvmVolumeGroupGVR).List(ctx, metav1.ListOptions{LabelSelector: ownerSel})
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "  list LVMVolumeGroup: %v\n", err)
	} else {
		if len(lvgs.Items) == 0 {
			fmt.Fprintln(GinkgoWriter, "  <none by owner label; listing all LVMVolumeGroups>")
			if all, allErr := suiteDyn.Resource(lvmVolumeGroupGVR).List(ctx, metav1.ListOptions{}); allErr != nil {
				fmt.Fprintf(GinkgoWriter, "  list all LVMVolumeGroup: %v\n", allErr)
			} else {
				lvgs = all
			}
		}
		for i := range lvgs.Items {
			dumpLVG(&lvgs.Items[i])
		}
		if len(lvgs.Items) == 0 {
			fmt.Fprintln(GinkgoWriter, "  <none>")
		}
	}

	fmt.Fprintf(GinkgoWriter, "\n--- BlockDevices (label %s) ---\n", suiteCfg.osdBDLabelKey)
	bds, err := suiteDyn.Resource(blockDeviceGVR).List(ctx, metav1.ListOptions{LabelSelector: suiteCfg.osdBDLabelKey})
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "  list BlockDevice: %v\n", err)
	} else {
		for i := range bds.Items {
			bd := &bds.Items[i]
			node, _, _ := unstructured.NestedString(bd.Object, "status", "nodeName")
			size, _, _ := unstructured.NestedString(bd.Object, "status", "size")
			consumable, _, _ := unstructured.NestedBool(bd.Object, "status", "consumable")
			path, _, _ := unstructured.NestedString(bd.Object, "status", "path")
			fmt.Fprintf(GinkgoWriter, "  %-50s node=%s size=%s consumable=%t path=%s\n",
				bd.GetName(), node, size, consumable, path)
		}
		if len(bds.Items) == 0 {
			fmt.Fprintln(GinkgoWriter, "  <none>")
		}
	}

	dumpBriefByOwner(ctx, "LVMLogicalVolumes", lvmLogicalVolumeGVR, ownerSel)
	dumpBriefByOwner(ctx, "PersistentVolumes", persistentVolumeGVR, ownerSel)
}

// dumpLVG prints one LVMVolumeGroup line (phase + Ready condition + nodes) and,
// when it is not Ready, its full status (exposes vgcreate / device errors).
func dumpLVG(lvg *unstructured.Unstructured) {
	phase, _, _ := unstructured.NestedString(lvg.Object, "status", "phase")
	status, reason, msg, _ := conditionByType(lvg, "Ready")
	fmt.Fprintf(GinkgoWriter, "  %-46s phase=%-10s Ready=%s reason=%s nodes=%v\n",
		lvg.GetName(), phase, status, reason, lvgNodeNames(lvg))
	if msg != "" {
		fmt.Fprintf(GinkgoWriter, "      msg: %s\n", msg)
	}
	if status != "True" {
		if st, ok, _ := unstructured.NestedMap(lvg.Object, "status"); ok {
			if y, err := yaml.Marshal(st); err == nil {
				fmt.Fprintln(GinkgoWriter, "      full status:")
				GinkgoWriter.Write(y)
			}
		}
	}
}

func lvgNodeNames(lvg *unstructured.Unstructured) []string {
	nodes, ok, _ := unstructured.NestedSlice(lvg.Object, "status", "nodes")
	if !ok {
		return nil
	}
	var names []string
	for _, n := range nodes {
		if m, ok := n.(map[string]interface{}); ok {
			if name, _, _ := unstructured.NestedString(m, "name"); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// dumpBriefByOwner lists a labelled resource and prints a one-line summary
// (phase + Ready condition) per object.
func dumpBriefByOwner(ctx context.Context, title string, gvr schema.GroupVersionResource, labelSelector string) {
	fmt.Fprintf(GinkgoWriter, "\n--- %s (label %s) ---\n", title, labelSelector)
	list, err := suiteDyn.Resource(gvr).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "  list %s: %v\n", gvr.Resource, err)
		return
	}
	if len(list.Items) == 0 {
		fmt.Fprintln(GinkgoWriter, "  <none>")
		return
	}
	for i := range list.Items {
		obj := &list.Items[i]
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		if st, reason, _, found := conditionByType(obj, "Ready"); found {
			fmt.Fprintf(GinkgoWriter, "  %-50s phase=%-10s Ready=%s reason=%s\n", obj.GetName(), phase, st, reason)
		} else {
			fmt.Fprintf(GinkgoWriter, "  %-50s phase=%s\n", obj.GetName(), phase)
		}
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

// dumpCsiCephCRs prints the csi-ceph CRs the EC's CsiCephReady stage produces:
// the per-cluster CephClusterConnection and every CephStorageClass.
func dumpCsiCephCRs(ctx context.Context) {
	fmt.Fprintf(GinkgoWriter, "\n--- CephClusterConnection %s ---\n", suiteCfg.ecName)
	if ccc, err := suiteDyn.Resource(cephClusterConnectionGVR).Get(ctx, suiteCfg.ecName, metav1.GetOptions{}); err != nil {
		fmt.Fprintf(GinkgoWriter, "  get CephClusterConnection: %v\n", err)
	} else {
		if phase, _, _ := unstructured.NestedString(ccc.Object, "status", "phase"); phase != "" {
			fmt.Fprintf(GinkgoWriter, "  phase=%s\n", phase)
		}
		dumpConditions(ccc)
	}

	fmt.Fprintln(GinkgoWriter, "\n--- CephStorageClasses ---")
	cscs, err := suiteDyn.Resource(cephStorageClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "  list CephStorageClass: %v\n", err)
		return
	}
	if len(cscs.Items) == 0 {
		fmt.Fprintln(GinkgoWriter, "  <none>")
		return
	}
	for i := range cscs.Items {
		csc := &cscs.Items[i]
		fmt.Fprintf(GinkgoWriter, "\n  CephStorageClass %s:\n", csc.GetName())
		if phase, _, _ := unstructured.NestedString(csc.Object, "status", "phase"); phase != "" {
			fmt.Fprintf(GinkgoWriter, "  phase=%s\n", phase)
		}
		dumpConditions(csc)
	}
}

// dumpWarningEvents prints the most recent Warning events in the sds-elastic and
// sds-node-configurator namespaces — surfaces scheduling / image-pull / mount
// failures that pod logs alone do not show.
func dumpWarningEvents(ctx context.Context, cs clientgokube.Interface) {
	for _, ns := range []string{d8ElasticNamespace, d8NodeConfiguratorNamespace} {
		fmt.Fprintf(GinkgoWriter, "\n--- Warning events in %s (last %d) ---\n", ns, diagMaxEvents)
		evs, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "  list events: %v\n", err)
			continue
		}
		warn := make([]corev1.Event, 0, len(evs.Items))
		for i := range evs.Items {
			if evs.Items[i].Type == corev1.EventTypeWarning {
				warn = append(warn, evs.Items[i])
			}
		}
		sort.Slice(warn, func(i, j int) bool { return eventTime(warn[i]).Before(eventTime(warn[j])) })
		if len(warn) > diagMaxEvents {
			warn = warn[len(warn)-diagMaxEvents:]
		}
		if len(warn) == 0 {
			fmt.Fprintln(GinkgoWriter, "  <none>")
			continue
		}
		for i := range warn {
			e := warn[i]
			fmt.Fprintf(GinkgoWriter, "  %s %dx %s/%s %s: %s\n",
				eventTime(e).Format(time.RFC3339), e.Count,
				e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, e.Message)
		}
	}
}

func eventTime(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

// dumpNamespaceDiagnostics lists every pod in a namespace, printing per-pod
// phase, per-container status (ready / restarts / waiting|terminated reason),
// and a tail of each container's logs.
func dumpNamespaceDiagnostics(ctx context.Context, cs clientgokube.Interface, namespace string, tail int64) {
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "\nlist pods in %s: %v\n", namespace, err)
		return
	}
	if len(pods.Items) == 0 {
		fmt.Fprintf(GinkgoWriter, "\n--- no pods in %s ---\n", namespace)
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		fmt.Fprintf(GinkgoWriter, "\n--- pod %s/%s on %s phase=%s ---\n",
			pod.Namespace, pod.Name, pod.Spec.NodeName, pod.Status.Phase)
		dumpContainerStatuses(pod)
		for _, c := range pod.Spec.Containers {
			dumpContainerLogs(ctx, cs, pod.Namespace, pod.Name, c.Name, tail)
		}
	}
}

// dumpContainerStatuses prints the runtime state of each (init) container so
// ImagePullBackOff / CrashLoopBackOff are visible without scrolling logs.
func dumpContainerStatuses(pod *corev1.Pod) {
	statuses := append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	for _, st := range statuses {
		state := "Running"
		switch {
		case st.State.Waiting != nil:
			state = fmt.Sprintf("Waiting(%s: %s)", st.State.Waiting.Reason, st.State.Waiting.Message)
		case st.State.Terminated != nil:
			state = fmt.Sprintf("Terminated(%s exit=%d)", st.State.Terminated.Reason, st.State.Terminated.ExitCode)
		}
		fmt.Fprintf(GinkgoWriter, "    container %-22s ready=%t restarts=%d state=%s\n",
			st.Name, st.Ready, st.RestartCount, state)
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

// dumpNodeSummary prints node Ready status and whether each node carries the
// EC storage-node label (so it is eligible to host OSDs).
func dumpNodeSummary(ctx context.Context, cs clientgokube.Interface) {
	fmt.Fprintf(GinkgoWriter, "\n--- Nodes (storage label %s=%s) ---\n",
		suiteCfg.storageNodeLabelKey, suiteCfg.storageNodeLabelValue)
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "  list nodes: %v\n", err)
		return
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		ready := "Unknown"
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				ready = string(c.Status)
				break
			}
		}
		_, hasStorage := n.Labels[suiteCfg.storageNodeLabelKey]
		fmt.Fprintf(GinkgoWriter, "  %-24s Ready=%s storageNode=%t\n", n.Name, ready, hasStorage)
	}
}
