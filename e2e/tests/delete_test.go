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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

const (
	// guardSettleTimeout bounds how long a teardown guard may take to surface
	// its blocking condition.
	guardSettleTimeout = 5 * time.Minute
	// forceNoBypassWindow is how long we hold to confirm force-deletion does
	// NOT bypass the bound-PV guard.
	forceNoBypassWindow = 30 * time.Second
)

// deleteSpecs registers the data-safety deletion-guard specs (destructive,
// hence ordered after creation and the disable-blocked check). They reuse the
// probe Pods created in createSpecs and progressively dismantle the shared
// fixture; the surviving HighRedundancy ESC is removed last to exercise the
// ElasticCluster StorageClassesExist guard.
func deleteSpecs() {
	It("blocks ElasticStorageClass deletion while a bound PV exists, and force-deletion does not bypass the bound-PV guard", func() {
		ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+10*time.Minute)
		defer cancel()

		By("Confirming the RBD round-trip volume still holds its data")
		Expect(verifyProbeFile(ctx, rbdProbePod, rbdProbeMarker)).To(Succeed())

		By("Deleting the RBD ElasticStorageClass while its PVC is still bound")
		Expect(storagekube.DeleteElasticStorageClass(ctx, suiteRestCfg, escRBDName())).To(Succeed())

		By("Asserting it sticks in Terminating with reason BoundVolumesExist")
		expectESCStuck(ctx, escRBDName(), storagekube.ElasticStorageClassReasonBoundVolumesExist, guardSettleTimeout)

		By("Asserting the CephStorageClass + backing pool are preserved and data is still readable")
		_, err := suiteDyn.Resource(cephStorageClassGVR).Get(ctx, escRBDName(), metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "CephStorageClass %s must be preserved by the guard", escRBDName())
		_, err = suiteDyn.Resource(storagekube.ElasticRookCephBlockPoolGVR).
			Namespace(suiteCfg.rookNamespace).Get(ctx, escRBDName(), metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "Rook CephBlockPool %s must be preserved by the guard", escRBDName())
		Expect(verifyProbeFile(ctx, rbdProbePod, rbdProbeMarker)).To(Succeed())

		By("Setting force-deletion and confirming it does NOT bypass the bound-PV guard")
		Expect(annotateForceDeletion(ctx, escRBDName())).To(Succeed())
		consistentlyESCReason(ctx, escRBDName(), storagekube.ElasticStorageClassReasonBoundVolumesExist, forceNoBypassWindow)

		By("Removing the bound PVC+Pod and letting the teardown finish")
		Expect(deleteProbe(ctx, rbdProbePVC, rbdProbePod)).To(Succeed())
		Expect(storagekube.WaitForElasticStorageClassGone(ctx, suiteRestCfg, escRBDName(), resourceGoneTimeout)).To(Succeed())
		Expect(waitResourceGone(ctx, cephStorageClassGVR, "", escRBDName(), resourceGoneTimeout)).To(Succeed())
		Expect(waitResourceGone(ctx, storagekube.ElasticRookCephBlockPoolGVR, suiteCfg.rookNamespace, escRBDName(), resourceGoneTimeout)).To(Succeed())
	})

	It("purges a non-empty RBD pool only after force-deletion (DataPresentInPool guard)", func() {
		ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+15*time.Minute)
		defer cancel()

		const (
			poolPVC    = "probe-rbd-pool"
			poolPod    = "probe-rbd-pool"
			poolMarker = "sds-elastic-e2e-rbd-pool"
		)

		By("Recreating a fresh RBD ElasticStorageClass")
		_, err := testkit.EnsureElasticStorageClass(ctx, suiteRestCfg, testkit.ElasticStorageClassConfig{
			Name:         escRBDName(),
			ClusterRef:   suiteCfg.ecName,
			Type:         testkit.ElasticStorageClassTypeRBD,
			Replication:  testkit.ElasticReplicationConsistencyAndAvailability,
			ReadyTimeout: suiteCfg.escReadyTimeout,
		})
		Expect(err).NotTo(HaveOccurred(), "fresh RBD ESC %s did not reach Ready", escRBDName())

		By("Leaving an orphaned RBD image in the pool via a Retained, then deleted, PVC")
		Expect(createProbePodWithPVC(ctx, escRBDName(), poolPVC, poolPod, poolMarker)).To(Succeed())
		pvName, err := retainBoundPV(ctx, poolPVC)
		Expect(err).NotTo(HaveOccurred())
		Expect(deleteProbe(ctx, poolPVC, poolPod)).To(Succeed())

		By("Deleting the ESC and asserting it sticks with reason DataPresentInPool")
		Expect(storagekube.DeleteElasticStorageClass(ctx, suiteRestCfg, escRBDName())).To(Succeed())
		expectESCStuck(ctx, escRBDName(), storagekube.ElasticStorageClassReasonDataPresentInPool, guardSettleTimeout)
		_, err = suiteDyn.Resource(storagekube.ElasticRookCephBlockPoolGVR).
			Namespace(suiteCfg.rookNamespace).Get(ctx, escRBDName(), metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "pool must be preserved while data is present")

		By("Force-deleting to purge the non-empty pool")
		Expect(annotateForceDeletion(ctx, escRBDName())).To(Succeed())
		Expect(storagekube.WaitForElasticStorageClassGone(ctx, suiteRestCfg, escRBDName(), resourceGoneTimeout)).To(Succeed())
		Expect(waitResourceGone(ctx, storagekube.ElasticRookCephBlockPoolGVR, suiteCfg.rookNamespace, escRBDName(), resourceGoneTimeout)).To(Succeed())

		By("Cleaning up the orphaned Released PV")
		Expect(deleteReleasedPV(ctx, pvName)).To(Succeed())
	})

	It("blocks CephFS ElasticStorageClass deletion while the filesystem is non-empty (FilesystemNotEmpty guard)", func() {
		ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+15*time.Minute)
		defer cancel()

		const (
			fsOrphanPVC    = "probe-fs-orphan"
			fsOrphanPod    = "probe-fs-orphan"
			fsOrphanMarker = "sds-elastic-e2e-cephfs-orphan"
		)

		By("Removing the bound CephFS round-trip probe so the guard is FilesystemNotEmpty, not BoundVolumesExist")
		Expect(deleteProbe(ctx, fsProbePVC, fsProbePod)).To(Succeed())

		By("Leaving an orphaned subvolume via a Retained, then deleted, PVC")
		Expect(createProbePodWithPVC(ctx, escCephFSName(), fsOrphanPVC, fsOrphanPod, fsOrphanMarker)).To(Succeed())
		pvName, err := retainBoundPV(ctx, fsOrphanPVC)
		Expect(err).NotTo(HaveOccurred())
		Expect(deleteProbe(ctx, fsOrphanPVC, fsOrphanPod)).To(Succeed())

		By("Deleting the CephFS ESC and asserting it sticks with reason FilesystemNotEmpty")
		Expect(storagekube.DeleteElasticStorageClass(ctx, suiteRestCfg, escCephFSName())).To(Succeed())
		expectESCStuck(ctx, escCephFSName(), storagekube.ElasticStorageClassReasonFilesystemNotEmpty, guardSettleTimeout)

		By("There is no force path for CephFS: reclaiming the orphaned PV (reclaimPolicy=Delete) destroys the subvolume and lets teardown finish")
		Expect(reclaimReleasedPV(ctx, pvName, resourceGoneTimeout)).To(Succeed())
		Expect(storagekube.WaitForElasticStorageClassGone(ctx, suiteRestCfg, escCephFSName(), resourceGoneTimeout)).To(Succeed())
		Expect(waitResourceGone(ctx, cephStorageClassGVR, "", escCephFSName(), resourceGoneTimeout)).To(Succeed())
		Expect(waitResourceGone(ctx, storagekube.ElasticRookCephFilesystemGVR, suiteCfg.rookNamespace, escCephFSName(), resourceGoneTimeout)).To(Succeed())
	})

	It("blocks ElasticCluster deletion while StorageClasses exist, keeps volumes live, then tears down in order with credential/disks preserved", func() {
		ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+25*time.Minute)
		defer cancel()

		const (
			hrPVC        = "probe-hr"
			hrPod        = "probe-hr"
			hrMarker     = "sds-elastic-e2e-hr"
			hrMarkerLive = "sds-elastic-e2e-hr-live"
		)

		By("Writing data to a fresh PVC+Pod on the surviving HighRedundancy ESC")
		Expect(createProbePodWithPVC(ctx, escHRName(), hrPVC, hrPod, hrMarker)).To(Succeed())
		Expect(verifyProbeFile(ctx, hrPod, hrMarker)).To(Succeed())

		By("Deleting the ElasticCluster and asserting it sticks with reason StorageClassesExist")
		Expect(storagekube.DeleteElasticCluster(ctx, suiteRestCfg, suiteCfg.ecName)).To(Succeed())
		expectECStuck(ctx, storagekube.ElasticClusterReasonStorageClassesExist, guardSettleTimeout)

		By("Asserting the CephCluster and CephClusterConnection are preserved under the guard")
		names, err := storagekube.ListElasticRookCephClusterNames(ctx, suiteRestCfg, suiteCfg.rookNamespace)
		Expect(err).NotTo(HaveOccurred())
		Expect(names).NotTo(BeEmpty(), "Rook CephCluster must be preserved while EC teardown is blocked")
		_, err = suiteDyn.Resource(cephClusterConnectionGVR).Get(ctx, suiteCfg.ecName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "CephClusterConnection must be preserved under the guard")

		By("Proving volume I/O stays live while the EC is blocked in Terminating")
		Expect(verifyProbeFile(ctx, hrPod, hrMarker)).To(Succeed())
		Expect(writeProbeFile(ctx, hrPod, hrMarkerLive)).To(Succeed())
		Expect(verifyProbeFile(ctx, hrPod, hrMarkerLive)).To(Succeed())

		By("Removing the PVC+Pod and the last ESC to let the EC teardown proceed")
		Expect(deleteProbe(ctx, hrPVC, hrPod)).To(Succeed())
		Expect(testkit.TeardownElasticStorageClass(ctx, suiteRestCfg, escHRName(), false, resourceGoneTimeout)).To(Succeed())

		By("Asserting the EC and its Rook/csi-ceph wiring are gone")
		Expect(storagekube.WaitForElasticClusterGone(ctx, suiteRestCfg, suiteCfg.ecName, resourceGoneTimeout)).To(Succeed())
		Expect(waitResourceGone(ctx, cephClusterConnectionGVR, "", suiteCfg.ecName, resourceGoneTimeout)).To(Succeed())
		Eventually(func() ([]string, error) {
			return storagekube.ListElasticRookCephClusterNames(ctx, suiteRestCfg, suiteCfg.rookNamespace)
		}, resourceGoneTimeout, pollInterval).Should(BeEmpty(), "Rook CephCluster should be gone after EC teardown")

		By("Asserting reversibility: credential, labelled LVG/LLV/PV and BD owner labels survive")
		_, err = suiteDyn.Resource(elasticClusterCredentialGVR).Get(ctx, suiteCfg.ecName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "ElasticClusterCredential must survive EC teardown")
		ownerSel := clusterOwnerLabelKey + "=" + suiteCfg.ecName
		Expect(countLabeled(ctx, lvmVolumeGroupGVR, "", ownerSel)).To(BeNumerically(">", 0), "LVMVolumeGroups must survive")
		Expect(countLabeled(ctx, lvmLogicalVolumeGVR, "", ownerSel)).To(BeNumerically(">", 0), "LVMLogicalVolumes must survive")
		Expect(countLabeled(ctx, persistentVolumeGVR, "", ownerSel)).To(BeNumerically(">", 0), "local PVs must survive")
		Expect(countLabeled(ctx, blockDeviceGVR, "", ownerSel)).To(BeNumerically(">", 0), "BlockDevice owner labels must survive")
	})
}

// finalManualCleanupSpec registers the documented post-uninstall manual cleanup
// spec. It is the very last spec (runs after the module is force-disabled), so
// it must keep BD labels/disks intact until then.
func finalManualCleanupSpec() {
	It("performs the documented manual cleanup of leftover PV/LLV/LVG and strips OSD BlockDevice labels", func() {
		ctx, cancel := context.WithTimeout(context.Background(), resourceGoneTimeout+5*time.Minute)
		defer cancel()

		By("Running the documented manual reclaim cleanup")
		Expect(manualReclaimCleanup(ctx)).To(Succeed())

		By("Asserting the EC-owned PV/LLV/LVG are gone and the OSD label is stripped")
		ownerSel := clusterOwnerLabelKey + "=" + suiteCfg.ecName
		Eventually(func() (int, error) { return countLabeled(ctx, persistentVolumeGVR, "", ownerSel) }, resourceGoneTimeout, pollInterval).
			Should(BeZero(), "leftover local PVs should be cleaned up")
		Eventually(func() (int, error) { return countLabeled(ctx, lvmLogicalVolumeGVR, "", ownerSel) }, resourceGoneTimeout, pollInterval).
			Should(BeZero(), "leftover LVMLogicalVolumes should be cleaned up")
		Eventually(func() (int, error) { return countLabeled(ctx, lvmVolumeGroupGVR, "", ownerSel) }, resourceGoneTimeout, pollInterval).
			Should(BeZero(), "leftover LVMVolumeGroups should be cleaned up")
		Eventually(func() (int, error) { return countLabeled(ctx, blockDeviceGVR, "", suiteCfg.osdBDLabelKey) }, 2*time.Minute, pollInterval).
			Should(BeZero(), "the OSD selector label should be stripped from BlockDevices")
	})
}

// --- shared guard assertions ----------------------------------------------

// expectESCStuck waits until the ESC reports Ready=False with the given reason
// (the controller is holding a teardown guard).
func expectESCStuck(ctx context.Context, escName, reason string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		status, gotReason, _, found, err := storagekube.GetElasticStorageClassCondition(
			ctx, suiteRestCfg, escName, storagekube.ElasticStorageClassConditionReady)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue(), "ESC %s should still expose a Ready condition", escName)
		g.Expect(status).To(Equal("False"), "ESC %s Ready should be False while terminating", escName)
		g.Expect(gotReason).To(Equal(reason), "ESC %s Ready reason", escName)
	}, timeout, pollInterval).Should(Succeed())
}

// consistentlyESCReason asserts the ESC keeps reporting Ready=False with reason
// for the whole window (used to prove force-deletion does NOT bypass a guard).
func consistentlyESCReason(ctx context.Context, escName, reason string, window time.Duration) {
	GinkgoHelper()
	Consistently(func(g Gomega) {
		status, gotReason, _, found, err := storagekube.GetElasticStorageClassCondition(
			ctx, suiteRestCfg, escName, storagekube.ElasticStorageClassConditionReady)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(status).To(Equal("False"))
		g.Expect(gotReason).To(Equal(reason))
	}, window, pollInterval).Should(Succeed())
}

// expectECStuck waits until the ElasticCluster reports Ready=False with the
// given reason.
func expectECStuck(ctx context.Context, reason string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		status, gotReason, _, found, err := storagekube.GetElasticClusterCondition(
			ctx, suiteRestCfg, suiteCfg.ecName, storagekube.ElasticClusterConditionReady)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue(), "EC %s should still expose a Ready condition", suiteCfg.ecName)
		g.Expect(status).To(Equal("False"), "EC %s Ready should be False while terminating", suiteCfg.ecName)
		g.Expect(gotReason).To(Equal(reason), "EC %s Ready reason", suiteCfg.ecName)
	}, timeout, pollInterval).Should(Succeed())
}
