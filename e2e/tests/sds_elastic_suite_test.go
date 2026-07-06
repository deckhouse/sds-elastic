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
	"encoding/json"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/dynamic"

	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// fallbackMinOSDBlockDevices is the floor of consumable OSD BlockDevices the
// suite waits for when it did NOT attach the disks itself (existing cluster
// with pre-provisioned disks). HighRedundancy needs OSDs accepted on >=4
// distinct nodes, so 4 is the meaningful lower bound. When the suite attaches
// the disks, it instead waits for exactly the number it attached.
const fallbackMinOSDBlockDevices = 4

// e2eStateFile is where storage-e2e persists nested-cluster state (matches
// config.E2ETempDir, which is internal and not importable). Used only to surface
// access info in the keep-on-failure banner.
const e2eStateFile = "/tmp/e2e/cluster-state.json"

// anySpecFailed records whether any spec failed during the run. cleanupSuite
// consults it together with E2E_KEEP_CLUSTER_ON_FAILURE to decide whether to
// skip nested-cluster teardown.
var anySpecFailed bool

var _ = BeforeSuite(func() {
	prepareSuite()
})

var _ = AfterSuite(func() {
	cleanupSuite()
})

func TestSdsElastic(t *testing.T) {
	RegisterFailHandler(Fail)

	suiteConfig, reporterConfig := GinkgoConfiguration()
	if os.Getenv("CI") != "" {
		suiteConfig.FailFast = true
		suiteConfig.Timeout = 180 * time.Minute
	}
	// The suite shares one expensive ElasticCluster across dependency-ordered
	// specs, so randomization must stay OFF.
	suiteConfig.RandomizeAllSpecs = false
	reporterConfig.Verbose = true
	reporterConfig.ShowNodeEvents = false

	RunSpecs(t, "sds-elastic E2E Suite", suiteConfig, reporterConfig)
}

// The single root Ordered container. Spec registration goes through builder
// functions called in EXPLICIT dependency order (see README "Why one shared
// ElasticCluster"): per-file top-level Describes would order alphabetically and
// break the mcBlocked-before-teardown invariant.
var _ = Describe("sds-elastic e2e", Ordered, ContinueOnFailure, func() {
	BeforeAll(prepareSharedState)

	// Dump EC/ESC conditions, Rook status and controller logs on any failure.
	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		anySpecFailed = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		dumpFailedSpecDiagnostics(ctx)
	})

	createSpecs()              // create_test.go: EC + RBD/CephFS/HR ESC + data round-trip + no-leak
	snapshotSpecs()            // snapshot_test.go: VolumeSnapshot round-trip (RBD FS/Block, CephFS FS)
	rwxSpecs()                 // rwx_test.go: ReadWriteMany multi-attach across nodes (RBD Block, CephFS FS)
	moduleDisableBlockedSpec() // module_disable_test.go: disable denied while EC exists
	deleteSpecs()              // delete_test.go: ESC/EC deletion guards (data safety)
	moduleDisableForceSpec()   // module_disable_test.go: bare EC -> force-disable -> module uninstalled
	finalManualCleanupSpec()   // delete_test.go: documented manual PV/LLV/LVG/BD-label cleanup
})

func prepareSuite() {
	suiteCfg = loadConfig()

	GinkgoWriter.Printf("E2E config:\n")
	GinkgoWriter.Printf("  TEST_CLUSTER_CREATE_MODE:  %q\n", os.Getenv("TEST_CLUSTER_CREATE_MODE"))
	GinkgoWriter.Printf("  namespace (TEST_CLUSTER_NAMESPACE): %q\n", suiteCfg.namespace)
	GinkgoWriter.Printf("  E2E_EC_NAME:               %q\n", suiteCfg.ecName)
	GinkgoWriter.Printf("  E2E_ROOK_NAMESPACE:        %q\n", suiteCfg.rookNamespace)
	GinkgoWriter.Printf("  storage node label:        %s=%s\n", suiteCfg.storageNodeLabelKey, suiteCfg.storageNodeLabelValue)
	GinkgoWriter.Printf("  OSD BlockDevice label:     %s=%s\n", suiteCfg.osdBDLabelKey, suiteCfg.osdBDLabelValue)
	GinkgoWriter.Printf("  EC ready timeout:          %s\n", suiteCfg.ecReadyTimeout)
	GinkgoWriter.Printf("  ESC ready timeout:         %s\n", suiteCfg.escReadyTimeout)
	GinkgoWriter.Printf("  OSD disks per worker:      %d (%s each)\n", suiteCfg.osdDisksPerWorker, suiteCfg.osdDiskSize)
	GinkgoWriter.Printf("  probe image:               %q\n", suiteCfg.probeImage)

	ensureNestedTestCluster()

	var err error
	suiteRestCfg = suiteClusterResources.Kubeconfig
	suiteK8s, err = newRuntimeClient(suiteRestCfg)
	Expect(err).NotTo(HaveOccurred(), "build controller-runtime client")

	suiteDyn, err = dynamic.NewForConfig(suiteRestCfg)
	Expect(err).NotTo(HaveOccurred(), "build dynamic client")

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	By("Attaching raw OSD VirtualDisks to worker VMs")
	attached, err := attachOSDDisks(ctx)
	Expect(err).NotTo(HaveOccurred(), "attach raw OSD disks")

	// Wait for exactly the disks we attached; on the pre-provisioned path
	// (attached==0) fall back to the HR floor.
	minBD := attached
	if minBD <= 0 {
		minBD = fallbackMinOSDBlockDevices
	}

	By("Labelling storage nodes and OSD BlockDevices for the ElasticCluster")
	bds, err := testkit.EnsureElasticOSDBlockDevices(ctx, suiteRestCfg, testkit.ElasticOSDBlockDevicesConfig{
		NodeLabelKey:          suiteCfg.storageNodeLabelKey,
		NodeLabelValue:        suiteCfg.storageNodeLabelValue,
		BlockDeviceLabelKey:   suiteCfg.osdBDLabelKey,
		BlockDeviceLabelValue: suiteCfg.osdBDLabelValue,
		MinBlockDevices:       minBD,
	})
	Expect(err).NotTo(HaveOccurred(), "prepare OSD BlockDevices")
	suiteOSDBlockDevices = bds
	GinkgoWriter.Printf("  prepared %d OSD BlockDevice(s)\n", len(bds))

	By("Ensuring the in-cluster test namespace exists")
	Expect(ensureNamespace(ctx, suiteCfg.namespace)).To(Succeed())
}

// prepareSharedState runs once before the Ordered specs. Clients, node/BD
// labelling and the namespace are already set up in BeforeSuite; this is the
// hook where C5-C7 wire any additional shared fixtures.
func prepareSharedState() {
	GinkgoWriter.Printf("Shared ElasticCluster for this run: %s (namespace %s)\n", suiteCfg.ecName, suiteCfg.namespace)
}

func cleanupSuite() {
	// Keep the nested cluster alive for manual debugging when a spec failed and
	// the operator asked for it. Otherwise tear it down (the only mandatory
	// step; resource-level cleanup is driven by the specs themselves).
	if suiteCfg.keepClusterOnFailure && anySpecFailed {
		printKeepClusterBanner()
		return
	}
	cleanupNestedTestCluster()
}

// printKeepClusterBanner tells the operator how to reach the preserved cluster
// after a failure (E2E_KEEP_CLUSTER_ON_FAILURE). All lookups are best-effort.
func printKeepClusterBanner() {
	GinkgoWriter.Printf("\n========== E2E_KEEP_CLUSTER_ON_FAILURE: cluster preserved ==========\n")
	GinkgoWriter.Printf("A spec failed and nested-cluster teardown was SKIPPED for debugging.\n")
	GinkgoWriter.Printf("  namespace (PVC/Pod + base VM ns): %s\n", suiteCfg.namespace)
	GinkgoWriter.Printf("  ElasticCluster:                   %s\n", suiteCfg.ecName)
	GinkgoWriter.Printf("  Rook namespace:                   %s\n", suiteCfg.rookNamespace)
	if suiteClusterResources != nil && suiteClusterResources.KubeconfigPath != "" {
		GinkgoWriter.Printf("  kubeconfig (export KUBECONFIG):   %s\n", suiteClusterResources.KubeconfigPath)
	}
	if ip, vms := readClusterState(); ip != "" {
		GinkgoWriter.Printf("  first master IP:                  %s\n", ip)
		GinkgoWriter.Printf("  VMs:                              %v\n", vms)
		GinkgoWriter.Printf("  SSH (via jump host):              ssh -J $SSH_USER@$SSH_HOST $SSH_VM_USER@%s\n", ip)
	}
	GinkgoWriter.Printf("  state file:                       %s\n", e2eStateFile)
	GinkgoWriter.Printf("Remember to delete the VMs / nested cluster manually when finished.\n")
	GinkgoWriter.Printf("====================================================================\n")
}

// readClusterState parses the storage-e2e nested-cluster state file for access
// hints. Returns zero values on any error (file absent, partial run, etc.).
func readClusterState() (firstMasterIP string, vmNames []string) {
	data, err := os.ReadFile(e2eStateFile)
	if err != nil {
		return "", nil
	}
	var st struct {
		FirstMasterIP string   `json:"first_master_ip"`
		VMNames       []string `json:"vm_names"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return "", nil
	}
	return st.FirstMasterIP, st.VMNames
}
