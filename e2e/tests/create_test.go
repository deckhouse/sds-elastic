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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// ElasticStorageClass / probe identifiers shared across the create, delete and
// module-disable specs (same package). All names are derived from the shared
// EC name so a custom E2E_EC_NAME keeps the whole fixture set consistent.
//
// The probe Pods created here on the RBD and CephFS StorageClasses deliberately
// outlive createSpecs(): the delete specs (C6) reuse the still-bound RBD probe
// to arm the BoundVolumesExist guard and tear down the CephFS probe first to
// reach FilesystemNotEmpty rather than BoundVolumesExist.
const (
	rbdProbePVC    = "probe-rbd"
	rbdProbePod    = "probe-rbd"
	rbdProbeMarker = "sds-elastic-e2e-rbd"

	fsProbePVC    = "probe-fs"
	fsProbePod    = "probe-fs"
	fsProbeMarker = "sds-elastic-e2e-cephfs"
)

func escRBDName() string    { return suiteCfg.ecName + "-rbd" }
func escCephFSName() string { return suiteCfg.ecName + "-fs" }
func escHRName() string     { return suiteCfg.ecName + "-hr" }

// createSpecs registers the happy-path creation + data round-trip specs on the
// shared ElasticCluster, mirroring docs/USAGE.md. Specs run in registration
// order inside the root Ordered container (see sds_elastic_suite_test.go).
func createSpecs() {
	It("creates the shared ElasticCluster and waits for Ready (staged conditions, credential, LVG/LLV/PV, Rook CephCluster Ready)", func() {
		ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.ecReadyTimeout+10*time.Minute)
		defer cancel()

		By("Applying the ElasticCluster and waiting for the aggregate Ready condition")
		_, err := testkit.EnsureElasticCluster(ctx, suiteRestCfg, testkit.ElasticClusterConfig{
			Name:                           suiteCfg.ecName,
			NodeSelectorMatchLabels:        ecNodeSelector(),
			BlockDeviceSelectorMatchLabels: ecBDSelector(),
			NetworkPublic:                  suiteCfg.networkPublic,
			NetworkCluster:                 suiteCfg.networkCluster,
			ReadyTimeout:                   suiteCfg.ecReadyTimeout,
		})
		Expect(err).NotTo(HaveOccurred(), "ElasticCluster %s did not reach Ready", suiteCfg.ecName)

		By("Asserting the staged conditions are all True")
		// EC is already Ready, so the staged conditions must already be set;
		// the short per-condition wait only absorbs status-write lag.
		for _, cond := range []string{
			storagekube.ElasticClusterConditionStorageReady,
			storagekube.ElasticClusterConditionCephClusterReady,
			storagekube.ElasticClusterConditionCredentialsReady,
			storagekube.ElasticClusterConditionCsiCephReady,
			storagekube.ElasticClusterConditionReady,
		} {
			Expect(waitECCondition(ctx, cond, "True", 2*time.Minute)).
				To(Succeed(), "EC %s condition %s should be True", suiteCfg.ecName, cond)
		}

		By("Asserting the ElasticClusterCredential mirror exists")
		_, err = suiteDyn.Resource(elasticClusterCredentialGVR).Get(ctx, suiteCfg.ecName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "ElasticClusterCredential %s should exist", suiteCfg.ecName)

		By("Asserting the EC-owned LVMVolumeGroup / LVMLogicalVolume / PV objects exist")
		sel := clusterOwnerLabelKey + "=" + suiteCfg.ecName
		Eventually(func() (int, error) { return countLabeled(ctx, lvmVolumeGroupGVR, "", sel) }, 2*time.Minute, pollInterval).
			Should(BeNumerically(">", 0), "expected LVMVolumeGroup(s) labelled %s", sel)
		Eventually(func() (int, error) { return countLabeled(ctx, lvmLogicalVolumeGVR, "", sel) }, 2*time.Minute, pollInterval).
			Should(BeNumerically(">", 0), "expected LVMLogicalVolume(s) labelled %s", sel)
		Eventually(func() (int, error) { return countLabeled(ctx, persistentVolumeGVR, "", sel) }, 2*time.Minute, pollInterval).
			Should(BeNumerically(">", 0), "expected local PV(s) labelled %s", sel)

		By("Asserting the vendored Rook CephCluster is Ready")
		Expect(verifyRookCephClusterReady(ctx)).To(Succeed())
	})

	It("declares an RBD ElasticStorageClass (ConsistencyAndAvailability) and materialises the CephStorageClass + core StorageClass", func() {
		ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.escReadyTimeout+5*time.Minute)
		defer cancel()

		_, err := testkit.EnsureElasticStorageClass(ctx, suiteRestCfg, testkit.ElasticStorageClassConfig{
			Name:         escRBDName(),
			ClusterRef:   suiteCfg.ecName,
			Type:         testkit.ElasticStorageClassTypeRBD,
			Replication:  testkit.ElasticReplicationConsistencyAndAvailability,
			ReadyTimeout: suiteCfg.escReadyTimeout,
		})
		Expect(err).NotTo(HaveOccurred(), "RBD ElasticStorageClass %s did not reach Ready", escRBDName())

		assertESCWired(ctx, escRBDName())

		By("Asserting the backing Rook CephBlockPool is Ready")
		Expect(storagekube.WaitForElasticRookCephBlockPoolReady(ctx, suiteRestCfg, suiteCfg.rookNamespace, escRBDName(), 5*time.Minute)).
			To(Succeed(), "Rook CephBlockPool %s/%s should be Ready", suiteCfg.rookNamespace, escRBDName())
	})

	It("declares a CephFS ElasticStorageClass (ConsistencyAndAvailability) and reaches Ready", func() {
		ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.escReadyTimeout+5*time.Minute)
		defer cancel()

		_, err := testkit.EnsureElasticStorageClass(ctx, suiteRestCfg, testkit.ElasticStorageClassConfig{
			Name:         escCephFSName(),
			ClusterRef:   suiteCfg.ecName,
			Type:         testkit.ElasticStorageClassTypeCephFS,
			Replication:  testkit.ElasticReplicationConsistencyAndAvailability,
			ReadyTimeout: suiteCfg.escReadyTimeout,
		})
		Expect(err).NotTo(HaveOccurred(), "CephFS ElasticStorageClass %s did not reach Ready", escCephFSName())

		assertESCWired(ctx, escCephFSName())

		By("Asserting the backing Rook CephFilesystem is Ready")
		Expect(storagekube.WaitForElasticRookCephFilesystemReady(ctx, suiteRestCfg, suiteCfg.rookNamespace, escCephFSName(), 5*time.Minute)).
			To(Succeed(), "Rook CephFilesystem %s/%s should be Ready", suiteCfg.rookNamespace, escCephFSName())
	})

	It("declares a HighRedundancy ElasticStorageClass and promotes the Ceph topology to mon=5/mgr=3", func() {
		ctx, cancel := context.WithTimeout(context.Background(), suiteCfg.escReadyTimeout+15*time.Minute)
		defer cancel()

		// HighRedundancy requires the EC Ready with adopted BDs on >=4 nodes;
		// the Ordered sequence (EC first) guarantees this before admission.
		_, err := testkit.EnsureElasticStorageClass(ctx, suiteRestCfg, testkit.ElasticStorageClassConfig{
			Name:         escHRName(),
			ClusterRef:   suiteCfg.ecName,
			Type:         testkit.ElasticStorageClassTypeRBD,
			Replication:  testkit.ElasticReplicationHighRedundancy,
			ReadyTimeout: suiteCfg.escReadyTimeout,
		})
		Expect(err).NotTo(HaveOccurred(), "HighRedundancy ElasticStorageClass %s did not reach Ready", escHRName())

		assertESCWired(ctx, escHRName())

		By("Asserting EC.status.cephTopology promoted to mon=5/mgr=3 (reason HighRedundancyESCPresent)")
		Eventually(func(g Gomega) {
			topo, err := getCephTopology(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(topo.Reason).To(Equal("HighRedundancyESCPresent"),
				"cephTopology.reason should reflect the HighRedundancy ESC")
			g.Expect(topo.MonCount).To(BeEquivalentTo(5), "monCount should promote to 5")
			g.Expect(topo.MgrCount).To(BeEquivalentTo(3), "mgrCount should promote to 3")
		}, 10*time.Minute, pollInterval).Should(Succeed())

		By("Asserting the Rook CephCluster spec reflects the promoted mon/mgr counts")
		Eventually(func(g Gomega) {
			mon, mgr, err := rookCephClusterMonMgr(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(mon).To(BeEquivalentTo(5), "Rook CephCluster spec.mon.count should be 5")
			g.Expect(mgr).To(BeEquivalentTo(3), "Rook CephCluster spec.mgr.count should be 3")
		}, 10*time.Minute, pollInterval).Should(Succeed())
	})

	It("round-trips data through an RBD PVC + Pod", func() {
		ctx, cancel := context.WithTimeout(context.Background(), pvcBindTimeout+podReadyTimeout+2*time.Minute)
		defer cancel()

		Expect(createProbePodWithPVC(ctx, escRBDName(), rbdProbePVC, rbdProbePod, rbdProbeMarker)).
			To(Succeed(), "RBD probe PVC+Pod should bind and become Ready")
		Expect(verifyProbeFile(ctx, rbdProbePod, rbdProbeMarker)).
			To(Succeed(), "RBD volume should return the written marker")
	})

	It("round-trips data through a CephFS PVC + Pod", func() {
		ctx, cancel := context.WithTimeout(context.Background(), pvcBindTimeout+podReadyTimeout+2*time.Minute)
		defer cancel()

		Expect(createProbePodWithPVC(ctx, escCephFSName(), fsProbePVC, fsProbePod, fsProbeMarker)).
			To(Succeed(), "CephFS probe PVC+Pod should bind and become Ready")
		Expect(verifyProbeFile(ctx, fsProbePod, fsProbeMarker)).
			To(Succeed(), "CephFS volume should return the written marker")
	})

	It("keeps the vendored Rook fully renamed (no ceph.rook.io leak)", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		Expect(verifyNoUpstreamRookNaming(ctx)).
			To(Succeed(), "no upstream ceph.rook.io group/CRs must be present")
	})
}

// assertESCWired asserts an ElasticStorageClass reached its backend + CSI
// stages and produced the csi-ceph objects the controller is responsible for:
// the per-cluster CephClusterConnection (1:1 with the EC) and the per-ESC
// CephStorageClass (1:1 with the ESC). The core k8s StorageClass is already
// awaited by EnsureElasticStorageClass.
func assertESCWired(ctx context.Context, escName string) {
	GinkgoHelper()

	Expect(waitESCCondition(ctx, escName, storagekube.ElasticStorageClassConditionPoolReady, "True", 2*time.Minute)).
		To(Succeed(), "ESC %s PoolReady should be True", escName)
	Expect(waitESCCondition(ctx, escName, storagekube.ElasticStorageClassConditionCsiStorageClassReady, "True", 2*time.Minute)).
		To(Succeed(), "ESC %s CsiStorageClassReady should be True", escName)

	_, err := suiteDyn.Resource(cephClusterConnectionGVR).Get(ctx, suiteCfg.ecName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "CephClusterConnection %s should exist", suiteCfg.ecName)

	_, err = suiteDyn.Resource(cephStorageClassGVR).Get(ctx, escName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "CephStorageClass %s should exist", escName)
}

// rookCephClusterMonMgr reads spec.mon.count / spec.mgr.count from the single
// renamed-group Rook CephCluster the EC manages in the Rook namespace.
func rookCephClusterMonMgr(ctx context.Context) (mon, mgr int64, err error) {
	names, err := storagekube.ListElasticRookCephClusterNames(ctx, suiteRestCfg, suiteCfg.rookNamespace)
	if err != nil {
		return 0, 0, err
	}
	if len(names) == 0 {
		return 0, 0, fmt.Errorf("no Rook CephCluster found in %s", suiteCfg.rookNamespace)
	}
	obj, err := suiteDyn.Resource(storagekube.ElasticRookCephClusterGVR).
		Namespace(suiteCfg.rookNamespace).Get(ctx, names[0], metav1.GetOptions{})
	if err != nil {
		return 0, 0, err
	}
	mon, _, _ = unstructured.NestedInt64(obj.Object, "spec", "mon", "count")
	mgr, _, _ = unstructured.NestedInt64(obj.Object, "spec", "mgr", "count")
	return mon, mgr, nil
}
