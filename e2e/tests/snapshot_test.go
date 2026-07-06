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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

const (
	// snapBlockPVCSize is the size of Block-mode PVCs in the snapshot / RWX
	// specs. Kept small so a whole-device sha256 (read end to end) stays fast;
	// Filesystem-mode PVCs use suiteCfg.pvcSize instead.
	snapBlockPVCSize = "256Mi"

	// snapBlockWriteMiB is how many MiB of random data we write into a Block
	// volume before snapshotting. Must be < snapBlockPVCSize so dd never hits
	// ENOSPC; the rest of the device reads back as deterministic zeros, so a
	// whole-device checksum is still stable.
	snapBlockWriteMiB = 200

	// snapFileCount is how many files the Filesystem scenario writes.
	snapFileCount = 5

	// snapshotReadyTimeout bounds the wait for a VolumeSnapshot to report
	// status.readyToUse=true.
	snapshotReadyTimeout = 5 * time.Minute

	// snapScenarioTimeout bounds a single (ESC, mode) snapshot scenario.
	snapScenarioTimeout = 30 * time.Minute
)

// snapshotSpecs registers the VolumeSnapshot round-trip specs on the shared
// RBD / CephFS ElasticStorageClasses created by createSpecs(). Each spec runs
// the full write -> checksum -> snapshot -> restore -> verify -> isolate ->
// delete-source -> write-restored flow and then fully reclaims everything it
// created (PVC+PV, restore PVC+PV, VolumeSnapshot + content) so the later
// delete-guard specs see only the createSpecs() probes still bound.
//
// CephFS+Block is intentionally absent: cephfs.csi.ceph.com is Filesystem-only.
func snapshotSpecs() {
	It("snapshots an RBD Filesystem volume, restores it, and verifies integrity, isolation and writability", func() {
		runSnapshotScenario(escRBDName(), corev1.PersistentVolumeFilesystem, "snap-rbd-fs")
	})

	It("snapshots an RBD Block volume, restores it, and verifies integrity, isolation and writability", func() {
		runSnapshotScenario(escRBDName(), corev1.PersistentVolumeBlock, "snap-rbd-blk")
	})

	It("snapshots a CephFS Filesystem volume, restores it, and verifies integrity, isolation and writability", func() {
		runSnapshotScenario(escCephFSName(), corev1.PersistentVolumeFilesystem, "snap-fs-fs")
	})
}

// runSnapshotScenario implements the 7-step snapshot flow for one ESC +
// volumeMode combination.
func runSnapshotScenario(escName string, mode corev1.PersistentVolumeMode, key string) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(context.Background(), snapScenarioTimeout)
	defer cancel()

	size := suiteCfg.pvcSize
	if mode == corev1.PersistentVolumeBlock {
		size = snapBlockPVCSize
	}

	origPVC := key + "-orig"
	origPod := key + "-orig"
	restorePVC := key + "-restore"
	restorePod := key + "-restore"
	snapName := key

	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), resourceGoneTimeout+10*time.Minute)
		defer ccancel()
		_ = deleteProbe(cctx, restorePVC, restorePod)
		_ = deleteProbe(cctx, origPVC, origPod)
		_ = deleteVolumeSnapshot(cctx, snapName)
		_ = waitPVCGone(cctx, restorePVC, resourceGoneTimeout)
		_ = waitPVCGone(cctx, origPVC, resourceGoneTimeout)
		_ = waitSnapshotGone(cctx, snapName, resourceGoneTimeout)
		_ = waitSnapshotContentGone(cctx, snapName, resourceGoneTimeout)
	}()

	By("Creating the original PVC + Pod and writing data")
	Expect(createSleeperPodWithPVC(ctx, escName, origPVC, origPod, mode, size, "")).
		To(Succeed(), "original %s PVC+Pod should bind and become Ready", key)
	Expect(writeSnapshotData(ctx, origPod, mode)).To(Succeed(), "writing data to the source volume")
	sumOrig, err := checksumData(ctx, origPod, mode)
	Expect(err).NotTo(HaveOccurred())
	Expect(sumOrig).NotTo(BeEmpty(), "source checksum must be non-empty")

	By("Creating a VolumeSnapshot and waiting for readyToUse")
	Expect(createVolumeSnapshot(ctx, snapName, origPVC, escName)).To(Succeed())
	restoreSize, err := waitSnapshotReady(ctx, snapName, snapshotReadyTimeout)
	Expect(err).NotTo(HaveOccurred())
	if strings.TrimSpace(restoreSize) == "" {
		restoreSize = size
	}

	By("Restoring a new PVC from the snapshot and asserting the checksum matches the source")
	Expect(createSleeperPodWithPVC(ctx, escName, restorePVC, restorePod, mode, restoreSize, snapName)).
		To(Succeed(), "restored %s PVC+Pod should bind and become Ready", key)
	sumRestored, err := checksumData(ctx, restorePod, mode)
	Expect(err).NotTo(HaveOccurred())
	Expect(sumRestored).To(Equal(sumOrig), "restored volume must match the source at snapshot time")

	By("Writing new data to the ORIGINAL and asserting the RESTORED copy is unchanged (isolation)")
	Expect(mutateSnapshotData(ctx, origPod, mode)).To(Succeed())
	sumRestoredAfter, err := checksumData(ctx, restorePod, mode)
	Expect(err).NotTo(HaveOccurred())
	Expect(sumRestoredAfter).To(Equal(sumOrig), "restored volume must stay isolated from writes to the source")

	By("Deleting the original volume, then writing to the RESTORED volume to prove it is writable")
	Expect(deleteProbe(ctx, origPVC, origPod)).To(Succeed())
	Expect(waitPVCGone(ctx, origPVC, resourceGoneTimeout)).To(Succeed())
	Expect(verifyWritable(ctx, restorePod, mode)).To(Succeed(), "restored volume must remain writable after the source is gone")
}

// --- PVC / Pod builders (Filesystem + Block) -------------------------------

// createSleeperPodWithPVC provisions a PVC (optionally restored from a
// snapshot) in the requested volumeMode and a long-lived Pod that mounts it
// (Filesystem: at probeMountPath; Block: as a raw device at probeDevicePath),
// then waits for the PVC to Bind and the Pod to be Ready. Idempotent against
// AlreadyExists.
func createSleeperPodWithPVC(ctx context.Context, scName, pvcName, podName string, mode corev1.PersistentVolumeMode, size, snapshotSource string) error {
	pvc := buildModePVC(pvcName, scName, size, mode, corev1.ReadWriteOnce, snapshotSource)
	if err := suiteK8s.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create pvc %s/%s: %w", suiteCfg.namespace, pvcName, err)
	}

	pod := buildSleeperPod(podName, pvcName, mode)
	if err := suiteK8s.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create pod %s/%s: %w", suiteCfg.namespace, podName, err)
	}

	if err := waitPVCBound(ctx, pvcName, pvcBindTimeout); err != nil {
		return err
	}
	return waitPodReady(ctx, podName, podReadyTimeout)
}

// buildModePVC renders a PVC with an explicit volumeMode and accessMode. When
// snapshotSource is non-empty the PVC is provisioned from that VolumeSnapshot.
func buildModePVC(name, scName, size string, mode corev1.PersistentVolumeMode, accessMode corev1.PersistentVolumeAccessMode, snapshotSource string) *corev1.PersistentVolumeClaim {
	sc := scName
	vm := mode
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: suiteCfg.namespace,
			Name:      name,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{accessMode},
			StorageClassName: &sc,
			VolumeMode:       &vm,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}
	if snapshotSource != "" {
		apiGroup := "snapshot.storage.k8s.io"
		pvc.Spec.DataSource = &corev1.TypedLocalObjectReference{
			APIGroup: &apiGroup,
			Kind:     "VolumeSnapshot",
			Name:     snapshotSource,
		}
	}
	return pvc
}

// buildSleeperPod renders a Pod that just sleeps, exposing the PVC either as a
// mounted filesystem (probeMountPath) or a raw block device (probeDevicePath)
// depending on mode. Tests drive I/O by exec-ing into it.
func buildSleeperPod(name, pvcName string, mode corev1.PersistentVolumeMode) *corev1.Pod {
	container := corev1.Container{
		Name:    probeContainerName,
		Image:   suiteCfg.probeImage,
		Command: []string{"sh", "-c", "sleep 360000"},
	}
	if mode == corev1.PersistentVolumeBlock {
		container.VolumeDevices = []corev1.VolumeDevice{{Name: "data", DevicePath: probeDevicePath}}
	} else {
		container.VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: probeMountPath}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: suiteCfg.namespace,
			Name:      name,
			Labels:    map[string]string{"app": "sds-elastic-e2e-probe"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{container},
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
		},
	}
}

// --- data write / checksum helpers -----------------------------------------

// execSh runs `sh -c script` inside the probe container and returns stdout,
// folding a transport error or any stderr output into the returned error.
func execSh(ctx context.Context, podName, script string) (string, error) {
	stdout, stderr, err := storagekube.ExecInPod(ctx, suiteRestCfg, suiteCfg.namespace, podName, probeContainerName, []string{"sh", "-c", script})
	if err != nil {
		return stdout, fmt.Errorf("exec in %s/%s: %w", suiteCfg.namespace, podName, err)
	}
	if strings.TrimSpace(stderr) != "" {
		return stdout, fmt.Errorf("exec in %s/%s reported stderr: %s", suiteCfg.namespace, podName, stderr)
	}
	return stdout, nil
}

// writeSnapshotData populates the source volume: several random files for a
// filesystem, or a fixed prefix of random data for a block device. Data is
// fsync'd and a final sync issued so the subsequent snapshot is consistent.
func writeSnapshotData(ctx context.Context, podName string, mode corev1.PersistentVolumeMode) error {
	var script string
	if mode == corev1.PersistentVolumeBlock {
		script = fmt.Sprintf(`set -e; dd if=/dev/urandom of=%s bs=1M count=%d conv=fsync 2>/dev/null; sync`,
			probeDevicePath, snapBlockWriteMiB)
	} else {
		script = fmt.Sprintf(`set -e; for i in %s; do dd if=/dev/urandom of=%s/file$i bs=1024 count=1024 conv=fsync 2>/dev/null; done; sync`,
			fileIdxList(), probeMountPath)
	}
	_, err := execSh(ctx, podName, script)
	return err
}

// checksumData returns a stable checksum of the volume contents: a whole-device
// sha256 for Block, or an aggregate sha256 over the sorted file set for
// Filesystem.
func checksumData(ctx context.Context, podName string, mode corev1.PersistentVolumeMode) (string, error) {
	var script string
	if mode == corev1.PersistentVolumeBlock {
		script = fmt.Sprintf(`sha256sum %s | awk '{print $1}'`, probeDevicePath)
	} else {
		script = fmt.Sprintf(`find %s -type f | sort | xargs sha256sum | sha256sum | awk '{print $1}'`, probeMountPath)
	}
	out, err := execSh(ctx, podName, script)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// mutateSnapshotData writes NEW data into the source volume so we can prove the
// restored copy is isolated: an extra file (Filesystem) or an overwritten
// region (Block). It never touches the file set / device region a restored
// volume would carry from the snapshot.
func mutateSnapshotData(ctx context.Context, podName string, mode corev1.PersistentVolumeMode) error {
	var script string
	if mode == corev1.PersistentVolumeBlock {
		script = fmt.Sprintf(`set -e; dd if=/dev/urandom of=%s bs=1M count=10 conv=fsync,notrunc 2>/dev/null; sync`, probeDevicePath)
	} else {
		script = fmt.Sprintf(`set -e; dd if=/dev/urandom of=%s/mutation.bin bs=1024 count=256 conv=fsync 2>/dev/null; sync`, probeMountPath)
	}
	_, err := execSh(ctx, podName, script)
	return err
}

// verifyWritable writes a marker into the volume and reads it back, proving the
// restored volume accepts writes after the source is deleted.
func verifyWritable(ctx context.Context, podName string, mode corev1.PersistentVolumeMode) error {
	const marker = "sds-elastic-e2e-writable"
	var script string
	if mode == corev1.PersistentVolumeBlock {
		script = fmt.Sprintf(`set -e
marker='%s'
len=${#marker}
printf '%%s' "$marker" | dd of=%s bs=1 seek=512 conv=fsync,notrunc 2>/dev/null
sync
got=$(dd if=%s bs=1 skip=512 count=$len 2>/dev/null)
[ "$got" = "$marker" ] || { echo "block write verify mismatch got=[$got] want=[$marker]" >&2; exit 1; }`,
			marker, probeDevicePath, probeDevicePath)
	} else {
		script = fmt.Sprintf(`set -e
marker='%s'
printf '%%s' "$marker" > %s/afterdelete.txt
sync
got=$(cat %s/afterdelete.txt)
[ "$got" = "$marker" ] || { echo "fs write verify mismatch got=[$got] want=[$marker]" >&2; exit 1; }`,
			marker, probeMountPath, probeMountPath)
	}
	_, err := execSh(ctx, podName, script)
	return err
}

// fileIdxList returns "1 2 3 ... snapFileCount" for the write loop.
func fileIdxList() string {
	ids := make([]string, 0, snapFileCount)
	for i := 1; i <= snapFileCount; i++ {
		ids = append(ids, fmt.Sprintf("%d", i))
	}
	return strings.Join(ids, " ")
}

// --- VolumeSnapshot helpers ------------------------------------------------

// createVolumeSnapshot creates a VolumeSnapshot of pvcName using the
// csi-ceph-managed VolumeSnapshotClass (named identically to the ESC /
// CephStorageClass). Idempotent against AlreadyExists.
func createVolumeSnapshot(ctx context.Context, name, pvcName, vscName string) error {
	snap := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "snapshot.storage.k8s.io/v1",
			"kind":       "VolumeSnapshot",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": suiteCfg.namespace,
			},
			"spec": map[string]interface{}{
				"volumeSnapshotClassName": vscName,
				"source": map[string]interface{}{
					"persistentVolumeClaimName": pvcName,
				},
			},
		},
	}
	_, err := suiteDyn.Resource(volumeSnapshotGVR).Namespace(suiteCfg.namespace).Create(ctx, snap, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create VolumeSnapshot %s/%s: %w", suiteCfg.namespace, name, err)
	}
	return nil
}

// waitSnapshotReady blocks until the VolumeSnapshot reports
// status.readyToUse=true and returns its status.restoreSize (may be empty).
func waitSnapshotReady(ctx context.Context, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		obj, err := suiteDyn.Resource(volumeSnapshotGVR).Namespace(suiteCfg.namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			ready, found, _ := unstructured.NestedBool(obj.Object, "status", "readyToUse")
			if found && ready {
				restoreSize, _, _ := unstructured.NestedString(obj.Object, "status", "restoreSize")
				return restoreSize, nil
			}
			last = fmt.Sprintf("readyToUse found=%v value=%v", found, ready)
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout waiting for VolumeSnapshot %s/%s to be readyToUse; last: %s", suiteCfg.namespace, name, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return "", ctx.Err()
		}
	}
}

// deleteVolumeSnapshot removes a VolumeSnapshot. Idempotent (NotFound ignored).
func deleteVolumeSnapshot(ctx context.Context, name string) error {
	err := suiteDyn.Resource(volumeSnapshotGVR).Namespace(suiteCfg.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete VolumeSnapshot %s/%s: %w", suiteCfg.namespace, name, err)
	}
	return nil
}

// waitSnapshotGone polls until the VolumeSnapshot GET returns NotFound.
func waitSnapshotGone(ctx context.Context, name string, timeout time.Duration) error {
	return waitResourceGone(ctx, volumeSnapshotGVR, suiteCfg.namespace, name, timeout)
}

// waitSnapshotContentGone waits until no VolumeSnapshotContent references the
// named snapshot, proving the backing Ceph snapshot was reclaimed (so the pool
// / filesystem is empty again for the delete-guard specs).
func waitSnapshotContentGone(ctx context.Context, snapName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		list, err := suiteDyn.Resource(volumeSnapshotContentGVR).List(ctx, metav1.ListOptions{})
		if err == nil {
			found := false
			for i := range list.Items {
				refName, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "volumeSnapshotRef", "name")
				if refName == snapName {
					found = true
					break
				}
			}
			if !found {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for VolumeSnapshotContent of %s/%s to be gone", suiteCfg.namespace, snapName)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}
