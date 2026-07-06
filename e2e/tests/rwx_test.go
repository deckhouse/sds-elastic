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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// rwxAppLabelKey is the pod label the RWX anti-affinity term selects on.
	rwxAppLabelKey = "app"

	// rwxBlockAlign is the offset/read granularity for the Block RWX marker.
	// 4096 keeps dd iflag=direct reads aligned on both 512- and 4096-byte
	// logical-sector devices.
	rwxBlockAlign = 4096

	// rwxScenarioTimeout bounds a single (ESC, mode) RWX scenario.
	rwxScenarioTimeout = 20 * time.Minute

	// rwxCrossNodeReadTimeout bounds how long the reader polls for data written
	// on the other node to become visible.
	rwxCrossNodeReadTimeout = 90 * time.Second
)

// rwxSpecs registers the ReadWriteMany (multi-attach) specs: two Pods forced
// onto different nodes must share a single volume. RBD only supports RWX in
// Block mode (a shared filesystem on one RBD image is unsafe), CephFS supports
// RWX in Filesystem mode. Both reclaim everything they create so the later
// delete-guard specs are unaffected.
func rwxSpecs() {
	It("shares an RBD Block ReadWriteMany volume across two Pods on different nodes", func() {
		runRWXScenario(escRBDName(), corev1.PersistentVolumeBlock, "rwx-rbd-blk")
	})

	It("shares a CephFS Filesystem ReadWriteMany volume across two Pods on different nodes", func() {
		runRWXScenario(escCephFSName(), corev1.PersistentVolumeFilesystem, "rwx-fs-fs")
	})
}

// runRWXScenario provisions one RWX PVC, schedules two anti-affine Pods on
// distinct nodes, and proves bidirectional shared access.
func runRWXScenario(escName string, mode corev1.PersistentVolumeMode, key string) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(context.Background(), rwxScenarioTimeout)
	defer cancel()

	size := suiteCfg.pvcSize
	if mode == corev1.PersistentVolumeBlock {
		size = snapBlockPVCSize
	}
	pvcName := key
	podA := key + "-a"
	podB := key + "-b"

	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), resourceGoneTimeout+5*time.Minute)
		defer ccancel()
		_ = deletePod(cctx, podA)
		_ = deletePod(cctx, podB)
		_ = deletePVCOnly(cctx, pvcName)
		_ = waitPVCGone(cctx, pvcName, resourceGoneTimeout)
	}()

	By("Creating a ReadWriteMany PVC")
	pvc := buildModePVC(pvcName, escName, size, mode, corev1.ReadWriteMany, "")
	err := suiteK8s.Create(ctx, pvc)
	Expect(err == nil || apierrors.IsAlreadyExists(err)).
		To(BeTrue(), "create RWX pvc %s/%s: %v", suiteCfg.namespace, pvcName, err)

	By("Scheduling two Pods with required anti-affinity so they land on different nodes")
	Expect(createRWXPod(ctx, podA, pvcName, mode, key)).To(Succeed())
	Expect(createRWXPod(ctx, podB, pvcName, mode, key)).To(Succeed())
	Expect(waitPVCBound(ctx, pvcName, pvcBindTimeout)).To(Succeed())
	Expect(waitPodReady(ctx, podA, podReadyTimeout)).To(Succeed())
	Expect(waitPodReady(ctx, podB, podReadyTimeout)).To(Succeed())

	By("Asserting the two consumers landed on different nodes")
	nodeA, err := podNodeName(ctx, podA)
	Expect(err).NotTo(HaveOccurred())
	nodeB, err := podNodeName(ctx, podB)
	Expect(err).NotTo(HaveOccurred())
	Expect(nodeA).NotTo(BeEmpty())
	Expect(nodeB).NotTo(BeEmpty())
	Expect(nodeA).NotTo(Equal(nodeB), "RWX consumers must be scheduled on different nodes")

	By("Writing from podA and reading it back from podB")
	markerAB := key + "-ab"
	Expect(rwxWrite(ctx, podA, mode, markerAB, 0)).To(Succeed())
	Eventually(func() (string, error) {
		return rwxRead(ctx, podB, mode, len(markerAB), 0)
	}, rwxCrossNodeReadTimeout, pollInterval).Should(Equal(markerAB), "podB should read data written by podA")

	By("Writing from podB and reading it back from podA (reverse direction)")
	markerBA := key + "-ba"
	Expect(rwxWrite(ctx, podB, mode, markerBA, 1)).To(Succeed())
	Eventually(func() (string, error) {
		return rwxRead(ctx, podA, mode, len(markerBA), 1)
	}, rwxCrossNodeReadTimeout, pollInterval).Should(Equal(markerBA), "podA should read data written by podB")
}

// --- RWX pod / IO helpers --------------------------------------------------

// createRWXPod creates a sleeper Pod that mounts the shared PVC and carries the
// required inter-pod anti-affinity (topology kubernetes.io/hostname) keyed on
// antiAffinityLabel so the two consumers are placed on different nodes.
func createRWXPod(ctx context.Context, podName, pvcName string, mode corev1.PersistentVolumeMode, antiAffinityLabel string) error {
	pod := buildRWXPod(podName, pvcName, mode, antiAffinityLabel)
	if err := suiteK8s.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create rwx pod %s/%s: %w", suiteCfg.namespace, podName, err)
	}
	return nil
}

func buildRWXPod(name, pvcName string, mode corev1.PersistentVolumeMode, antiAffinityLabel string) *corev1.Pod {
	pod := buildSleeperPod(name, pvcName, mode)
	pod.Labels = map[string]string{rwxAppLabelKey: antiAffinityLabel}
	pod.Spec.Affinity = &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{rwxAppLabelKey: antiAffinityLabel},
					},
					TopologyKey: "kubernetes.io/hostname",
				},
			},
		},
	}
	return pod
}

// rwxWrite writes marker into the shared volume from podName. slot selects a
// distinct file (Filesystem) or aligned device region (Block) so the two write
// directions do not collide.
func rwxWrite(ctx context.Context, podName string, mode corev1.PersistentVolumeMode, marker string, slot int) error {
	var script string
	if mode == corev1.PersistentVolumeBlock {
		script = fmt.Sprintf(`set -e; printf '%%s' '%s' | dd of=%s bs=%d seek=%d count=1 conv=fsync,notrunc 2>/dev/null; sync`,
			marker, probeDevicePath, rwxBlockAlign, slot)
	} else {
		script = fmt.Sprintf(`set -e; printf '%%s' '%s' > %s/shared%d.txt; sync`, marker, probeMountPath, slot)
	}
	_, err := execSh(ctx, podName, script)
	return err
}

// rwxRead reads back the marker from podName. Block reads use iflag=direct to
// bypass the reader node's page cache and go straight to the OSDs; the aligned
// block is captured via command substitution (which drops the trailing NUL
// padding) so no SIGPIPE is raised.
func rwxRead(ctx context.Context, podName string, mode corev1.PersistentVolumeMode, length, slot int) (string, error) {
	var script string
	if mode == corev1.PersistentVolumeBlock {
		script = fmt.Sprintf(`blk=$(dd if=%s bs=%d skip=%d count=1 iflag=direct 2>/dev/null); printf '%%s' "$blk" | head -c %d`,
			probeDevicePath, rwxBlockAlign, slot, length)
	} else {
		script = fmt.Sprintf(`cat %s/shared%d.txt 2>/dev/null`, probeMountPath, slot)
	}
	out, err := execSh(ctx, podName, script)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// --- generic delete helpers (shared PVC, so pods/PVC deleted separately) ----

func deletePod(ctx context.Context, podName string) error {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: suiteCfg.namespace, Name: podName}}
	if err := suiteK8s.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %s/%s: %w", suiteCfg.namespace, podName, err)
	}
	return waitPodGone(ctx, podName, podReadyTimeout)
}

func deletePVCOnly(ctx context.Context, pvcName string) error {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: suiteCfg.namespace, Name: pvcName}}
	if err := suiteK8s.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pvc %s/%s: %w", suiteCfg.namespace, pvcName, err)
	}
	return nil
}
