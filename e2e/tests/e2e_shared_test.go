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
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	elasticv1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/storage-e2e/pkg/cluster"
	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
	"github.com/deckhouse/storage-e2e/pkg/testkit"
)

// --- Suite env knobs (storage-e2e cluster knobs are read by storage-e2e itself) ---
const (
	envECName            = "E2E_EC_NAME"
	envPVCSize           = "E2E_PVC_SIZE"
	envRookNamespace     = "E2E_ROOK_NAMESPACE"
	envOSDBDLabel        = "E2E_OSD_BD_LABEL"
	envStorageNodeLabel  = "E2E_STORAGE_NODE_LABEL"
	envNetworkPublic     = "E2E_NETWORK_PUBLIC"
	envNetworkCluster    = "E2E_NETWORK_CLUSTER"
	envECReadyTimeout    = "E2E_EC_READY_TIMEOUT"
	envESCReadyTimeout   = "E2E_ESC_READY_TIMEOUT"
	envOSDDisksPerWorker = "E2E_OSD_DISKS_PER_WORKER"
	envOSDDiskSize       = "E2E_OSD_DISK_SIZE"
	envProbeImage        = "E2E_PROBE_IMAGE"
)

const (
	defaultECName       = "sds-elastic-e2e"
	defaultPVCSize      = "1Gi"
	defaultRookNS       = "d8-sds-elastic"
	defaultProbeImage   = "busybox:1.36"
	defaultOSDDiskSize  = "20Gi"
	defaultDisksPerNode = 2

	// moduleConfigName is the Deckhouse ModuleConfig the disable-guard webhook
	// protects; also the chart name.
	moduleConfigName = "sds-elastic"

	// forceDisableAnnotation is the escape hatch on the ModuleConfig that lets
	// an operator disable the module while ElasticClusters still exist
	// (mirrors the webhook in images/webhooks/handlers/mc_validator.go).
	forceDisableAnnotation = "sds-elastic.deckhouse.io/force-disable"

	// allowDisablingLabelKey is Deckhouse's own confirmation that a module may
	// be disabled; set so the suite isolates the sds-elastic webhook guard
	// rather than tripping the platform-level safeguard.
	allowDisablingLabelKey = "modules.deckhouse.io/allow-disabling"

	// clusterOwnerLabelKey marks the LVMVolumeGroup / LVMLogicalVolume / PV the
	// EC reconcile created for a given ElasticCluster.
	clusterOwnerLabelKey = "sds-elastic.deckhouse.io/cluster"

	d8ElasticNamespace      = "d8-sds-elastic"
	mcValidationWebhookName = "d8-sds-elastic-mc-validation"

	probeContainerName = "probe"
	probeMountPath     = "/data"
	probeFilePath      = "/data/probe.txt"
)

const (
	pollInterval        = 5 * time.Second
	pvcBindTimeout      = 5 * time.Minute
	podReadyTimeout     = 5 * time.Minute
	resourceGoneTimeout = 15 * time.Minute
	moduleGoneTimeout   = 15 * time.Minute
)

var (
	moduleConfigGVR = schema.GroupVersionResource{
		Group: "deckhouse.io", Version: "v1alpha1", Resource: "moduleconfigs",
	}
	blockDeviceGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "blockdevices",
	}
	lvmVolumeGroupGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "lvmvolumegroups",
	}
	lvmLogicalVolumeGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "lvmlogicalvolumes",
	}
	elasticClusterCredentialGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "elasticclustercredentials",
	}
	cephClusterConnectionGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "cephclusterconnections",
	}
	cephStorageClassGVR = schema.GroupVersionResource{
		Group: "storage.deckhouse.io", Version: "v1alpha1", Resource: "cephstorageclasses",
	}
	persistentVolumeGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "persistentvolumes",
	}

	// upstreamRookGVRs are the original ceph.rook.io resources that MUST NOT
	// appear on a cluster running sds-elastic (the module renames the group to
	// internal.sdselastic.deckhouse.io). verifyNoUpstreamRookNaming asserts none
	// exist.
	upstreamRookGVRs = []schema.GroupVersionResource{
		{Group: "ceph.rook.io", Version: "v1", Resource: "cephclusters"},
		{Group: "ceph.rook.io", Version: "v1", Resource: "cephblockpools"},
		{Group: "ceph.rook.io", Version: "v1", Resource: "cephfilesystems"},
		{Group: "ceph.rook.io", Version: "v1", Resource: "cephclients"},
	}
)

type e2eConfig struct {
	// namespace is the in-cluster namespace for PVCs/Pods. Single source of
	// truth: TEST_CLUSTER_NAMESPACE (also the base-cluster VM namespace).
	namespace string

	ecName        string
	pvcSize       string
	rookNamespace string

	storageNodeLabelKey   string
	storageNodeLabelValue string
	osdBDLabelKey         string
	osdBDLabelValue       string

	networkPublic  string
	networkCluster string

	ecReadyTimeout  time.Duration
	escReadyTimeout time.Duration

	osdDisksPerWorker int
	osdDiskSize       string
	probeImage        string

	// vmNamespace / baseStorageClass drive the runtime VirtualDisk attach for
	// raw OSD disks (base cluster). Both come from TEST_CLUSTER_*.
	vmNamespace      string
	baseStorageClass string
}

var (
	suiteCfg              e2eConfig
	suiteRestCfg          *rest.Config
	suiteK8s              client.Client
	suiteDyn              dynamic.Interface
	suiteClusterResources *cluster.TestClusterResources
	suiteOSDBlockDevices  []string
)

func loadConfig() e2eConfig {
	cfg := e2eConfig{
		namespace:        strings.TrimSpace(os.Getenv("TEST_CLUSTER_NAMESPACE")),
		ecName:           os.Getenv(envECName),
		pvcSize:          os.Getenv(envPVCSize),
		rookNamespace:    os.Getenv(envRookNamespace),
		networkPublic:    os.Getenv(envNetworkPublic),
		networkCluster:   os.Getenv(envNetworkCluster),
		osdDiskSize:      os.Getenv(envOSDDiskSize),
		probeImage:       os.Getenv(envProbeImage),
		vmNamespace:      strings.TrimSpace(os.Getenv("TEST_CLUSTER_NAMESPACE")),
		baseStorageClass: strings.TrimSpace(os.Getenv("TEST_CLUSTER_STORAGE_CLASS")),
	}

	if cfg.namespace == "" {
		cfg.namespace = "e2e-sds-elastic"
		cfg.vmNamespace = cfg.namespace
	}
	if cfg.ecName == "" {
		cfg.ecName = defaultECName
	}
	if cfg.pvcSize == "" {
		cfg.pvcSize = defaultPVCSize
	}
	if cfg.rookNamespace == "" {
		cfg.rookNamespace = defaultRookNS
	}
	if cfg.osdDiskSize == "" {
		cfg.osdDiskSize = defaultOSDDiskSize
	}
	if cfg.probeImage == "" {
		cfg.probeImage = defaultProbeImage
	}

	cfg.storageNodeLabelKey, cfg.storageNodeLabelValue = parseLabel(
		os.Getenv(envStorageNodeLabel),
		testkit.DefaultElasticStorageNodeLabelKey,
		testkit.DefaultElasticStorageNodeLabelValue,
	)
	cfg.osdBDLabelKey, cfg.osdBDLabelValue = parseLabel(
		os.Getenv(envOSDBDLabel),
		testkit.DefaultElasticOSDLabelKey,
		testkit.DefaultElasticOSDLabelValue,
	)

	cfg.ecReadyTimeout = parseDuration(os.Getenv(envECReadyTimeout), testkit.DefaultElasticClusterReadyTimeout)
	cfg.escReadyTimeout = parseDuration(os.Getenv(envESCReadyTimeout), testkit.DefaultElasticStorageClassReadyTimeout)

	cfg.osdDisksPerWorker = defaultDisksPerNode
	if raw := strings.TrimSpace(os.Getenv(envOSDDisksPerWorker)); raw != "" {
		if n, err := parsePositiveInt(raw); err == nil {
			cfg.osdDisksPerWorker = n
		}
	}

	return cfg
}

// parseLabel splits a "key=value" env value; "key" alone keeps defVal; empty
// keeps both defaults. storage-e2e's Elastic layer defaults to value "true",
// so a bare key still yields a usable selector.
func parseLabel(raw, defKey, defVal string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defKey, defVal
	}
	if i := strings.Index(raw, "="); i >= 0 {
		return raw[:i], raw[i+1:]
	}
	return raw, defVal
}

func parseDuration(raw string, def time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return def
}

func parsePositiveInt(raw string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("value %q must be positive", raw)
	}
	return n, nil
}

// newRuntimeClient builds a controller-runtime client with the sds-elastic
// v1alpha1 types registered on top of the built-in scheme (core/apps/storage/
// admissionregistration). Typed access is convenient for core-object asserts;
// CRDs are mostly handled through the dynamic client.
func newRuntimeClient(restCfg *rest.Config) (client.Client, error) {
	sch := scheme.Scheme
	if err := elasticv1alpha1.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("add sds-elastic scheme: %w", err)
	}
	return client.New(restCfg, client.Options{Scheme: sch})
}

func ensureNestedTestCluster() {
	if strings.TrimSpace(os.Getenv("TEST_CLUSTER_CREATE_MODE")) == "" {
		Fail("TEST_CLUSTER_CREATE_MODE must be set: this suite only supports storage-e2e nested clusters")
	}
	if suiteClusterResources != nil {
		return
	}
	suiteClusterResources = cluster.CreateOrConnectToTestCluster()
	if suiteClusterResources == nil || suiteClusterResources.Kubeconfig == nil {
		Fail("storage-e2e returned a nil cluster handle")
	}
}

func cleanupNestedTestCluster() {
	if suiteClusterResources == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := cluster.CleanupTestCluster(ctx, suiteClusterResources); err != nil {
		GinkgoWriter.Printf("  warning: nested cluster cleanup failed: %v\n", err)
	} else {
		GinkgoWriter.Printf("  nested cluster cleanup finished\n")
	}
	suiteClusterResources = nil
}

// attachOSDDisks attaches suiteCfg.osdDisksPerWorker raw VirtualDisks to every
// worker VM on the base cluster so sds-node-configurator surfaces them as
// consumable BlockDevices for the ElasticCluster to adopt as OSDs. Returns the
// total number of disks attached so the caller can wait for exactly that many
// consumable BlockDevices. No-op (returns 0) when BaseKubeconfig is nil
// (existing cluster with pre-provisioned disks).
func attachOSDDisks(ctx context.Context) (int, error) {
	if suiteClusterResources == nil || suiteClusterResources.BaseKubeconfig == nil {
		GinkgoWriter.Printf("  BaseKubeconfig is nil; skipping VirtualDisk attach (disks assumed pre-provisioned)\n")
		return 0, nil
	}
	workers, err := storagekube.GetWorkerNodes(ctx, suiteRestCfg)
	if err != nil {
		return 0, fmt.Errorf("list worker nodes: %w", err)
	}
	if len(workers) == 0 {
		return 0, fmt.Errorf("no worker nodes found to attach OSD disks to")
	}
	if suiteCfg.baseStorageClass == "" {
		return 0, fmt.Errorf("TEST_CLUSTER_STORAGE_CLASS must be set to attach raw OSD VirtualDisks")
	}

	attached := 0
	runSuffix := time.Now().Unix()
	for _, w := range workers {
		for d := 0; d < suiteCfg.osdDisksPerWorker; d++ {
			diskName := fmt.Sprintf("%s-elastic-osd-%d-%d", w.Name, runSuffix, d)
			res, err := storagekube.AttachVirtualDiskToVM(ctx, suiteClusterResources.BaseKubeconfig, storagekube.VirtualDiskAttachmentConfig{
				VMName:           w.Name,
				Namespace:        suiteCfg.vmNamespace,
				DiskName:         diskName,
				DiskSize:         suiteCfg.osdDiskSize,
				StorageClassName: suiteCfg.baseStorageClass,
			})
			if err != nil {
				return attached, fmt.Errorf("attach OSD disk %s to %s: %w", diskName, w.Name, err)
			}
			if err := storagekube.WaitForVirtualDiskAttached(ctx, suiteClusterResources.BaseKubeconfig, suiteCfg.vmNamespace, res.AttachmentName, 10*time.Second); err != nil {
				return attached, fmt.Errorf("wait OSD disk %s attach on %s: %w", diskName, w.Name, err)
			}
			attached++
		}
	}
	GinkgoWriter.Printf("  attached %d raw OSD disk(s) across %d worker(s)\n", attached, len(workers))
	return attached, nil
}

func ensureNamespace(ctx context.Context, name string) error {
	_, err := storagekube.CreateNamespaceIfNotExists(ctx, suiteRestCfg, name)
	return err
}

// ecNodeSelector / ecBDSelector are the selectors the shared ElasticCluster
// uses; they MUST match the labels EnsureElasticOSDBlockDevices applied.
func ecNodeSelector() map[string]string {
	return map[string]string{suiteCfg.storageNodeLabelKey: suiteCfg.storageNodeLabelValue}
}

func ecBDSelector() map[string]string {
	return map[string]string{suiteCfg.osdBDLabelKey: suiteCfg.osdBDLabelValue}
}

// --- condition waiters -----------------------------------------------------

func waitECCondition(ctx context.Context, condType, wantStatus string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		status, reason, _, found, err := storagekube.GetElasticClusterCondition(ctx, suiteRestCfg, suiteCfg.ecName, condType)
		if err == nil && found && status == wantStatus {
			return nil
		}
		last = fmt.Sprintf("found=%v status=%q reason=%q err=%v", found, status, reason, err)
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for EC %s condition %s=%s; last: %s", suiteCfg.ecName, condType, wantStatus, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

func waitESCCondition(ctx context.Context, escName, condType, wantStatus string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		status, reason, _, found, err := storagekube.GetElasticStorageClassCondition(ctx, suiteRestCfg, escName, condType)
		if err == nil && found && status == wantStatus {
			return nil
		}
		last = fmt.Sprintf("found=%v status=%q reason=%q err=%v", found, status, reason, err)
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for ESC %s condition %s=%s; last: %s", escName, condType, wantStatus, last)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

func getECReason(ctx context.Context, condType string) (string, error) {
	_, reason, _, found, err := storagekube.GetElasticClusterCondition(ctx, suiteRestCfg, suiteCfg.ecName, condType)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("EC %s has no %s condition", suiteCfg.ecName, condType)
	}
	return reason, nil
}

func getESCReason(ctx context.Context, escName, condType string) (string, error) {
	_, reason, _, found, err := storagekube.GetElasticStorageClassCondition(ctx, suiteRestCfg, escName, condType)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("ESC %s has no %s condition", escName, condType)
	}
	return reason, nil
}

// waitResourceGone blocks until a dynamic GET of the resource returns NotFound.
// ns="" addresses cluster-scoped resources.
func waitResourceGone(ctx context.Context, gvr schema.GroupVersionResource, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var err error
		if ns == "" {
			_, err = suiteDyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
		} else {
			_, err = suiteDyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		}
		if apierrors.IsNotFound(err) {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("timeout waiting for %s %s/%s to be gone; last get err: %w", gvr.Resource, ns, name, err)
			}
			return fmt.Errorf("timeout waiting for %s %s/%s to be gone (still present)", gvr.Resource, ns, name)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// --- Rook verifiers --------------------------------------------------------

func verifyRookCephClusterReady(ctx context.Context) error {
	names, err := storagekube.ListElasticRookCephClusterNames(ctx, suiteRestCfg, suiteCfg.rookNamespace)
	if err != nil {
		return fmt.Errorf("list Rook CephClusters in %s: %w", suiteCfg.rookNamespace, err)
	}
	if len(names) == 0 {
		return fmt.Errorf("no Rook CephCluster found in %s", suiteCfg.rookNamespace)
	}
	for _, n := range names {
		if err := storagekube.WaitForElasticRookCephClusterReady(ctx, suiteRestCfg, suiteCfg.rookNamespace, n, 15*time.Minute); err != nil {
			return fmt.Errorf("Rook CephCluster %s/%s not Ready: %w", suiteCfg.rookNamespace, n, err)
		}
	}
	return nil
}

// verifyNoUpstreamRookNaming asserts the vendored Rook is fully renamed: the
// apiserver does not serve ceph.rook.io and no ceph.rook.io CRs exist in the
// Rook namespace.
func verifyNoUpstreamRookNaming(ctx context.Context) error {
	has, err := storagekube.ServerHasAPIGroup(ctx, suiteRestCfg, storagekube.UpstreamRookGroup)
	if err != nil {
		return fmt.Errorf("discovery for %s: %w", storagekube.UpstreamRookGroup, err)
	}
	if has {
		return fmt.Errorf("upstream Rook API group %q is served by the apiserver but must be absent", storagekube.UpstreamRookGroup)
	}
	for _, gvr := range upstreamRookGVRs {
		list, err := suiteDyn.Resource(gvr).Namespace(suiteCfg.rookNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			// Group not served / kind unknown is exactly what we want.
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			}
			// Any other listing error against an absent group is also benign.
			continue
		}
		if len(list.Items) > 0 {
			return fmt.Errorf("found %d upstream %s in %s; expected none (Rook must use the renamed group)",
				len(list.Items), gvr.Resource, suiteCfg.rookNamespace)
		}
	}
	return nil
}

func getCephTopology(ctx context.Context) (storagekube.ElasticClusterCephTopology, error) {
	topo, found, err := storagekube.GetElasticClusterCephTopology(ctx, suiteRestCfg, suiteCfg.ecName)
	if err != nil {
		return topo, err
	}
	if !found {
		return topo, fmt.Errorf("EC %s has no status.cephTopology yet", suiteCfg.ecName)
	}
	return topo, nil
}

// --- PV reclaim helpers ----------------------------------------------------

// retainBoundPV flips the bound PV's reclaim policy to Retain so deleting the
// PVC leaves the PV (and its on-disk data) behind. Returns the PV name.
func retainBoundPV(ctx context.Context, pvcName string) (string, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := suiteK8s.Get(ctx, client.ObjectKey{Namespace: suiteCfg.namespace, Name: pvcName}, &pvc); err != nil {
		return "", fmt.Errorf("get pvc %s/%s: %w", suiteCfg.namespace, pvcName, err)
	}
	pvName := pvc.Spec.VolumeName
	if pvName == "" {
		return "", fmt.Errorf("pvc %s/%s has no bound PV yet", suiteCfg.namespace, pvcName)
	}
	var pv corev1.PersistentVolume
	if err := suiteK8s.Get(ctx, client.ObjectKey{Name: pvName}, &pv); err != nil {
		return "", fmt.Errorf("get pv %s: %w", pvName, err)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
		if err := suiteK8s.Update(ctx, &pv); err != nil {
			return "", fmt.Errorf("set pv %s reclaimPolicy=Retain: %w", pvName, err)
		}
	}
	return pvName, nil
}

func deleteReleasedPV(ctx context.Context, pvName string) error {
	var pv corev1.PersistentVolume
	err := suiteK8s.Get(ctx, client.ObjectKey{Name: pvName}, &pv)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get pv %s: %w", pvName, err)
	}
	return client.IgnoreNotFound(suiteK8s.Delete(ctx, &pv))
}

func annotateForceDeletion(ctx context.Context, escName string) error {
	return storagekube.AnnotateElasticStorageClassForceDeletion(ctx, suiteRestCfg, escName)
}

func countLabeled(ctx context.Context, gvr schema.GroupVersionResource, ns, labelSelector string) (int, error) {
	var (
		list *unstructured.UnstructuredList
		err  error
	)
	if ns == "" {
		list, err = suiteDyn.Resource(gvr).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	} else {
		list, err = suiteDyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	}
	if err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

// --- ModuleConfig / module disable helpers ---------------------------------

func patchModuleConfigEnabled(ctx context.Context, name string, enabled bool) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"enabled":%t}}`, enabled))
	_, err := suiteDyn.Resource(moduleConfigGVR).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// expectModuleConfigDenied attempts to set spec.enabled and asserts the
// attempt is rejected by an admission webhook (the disable guard). Returns an
// error if the patch unexpectedly succeeded or failed for an unrelated reason.
func expectModuleConfigDenied(ctx context.Context, name string, enabled bool) error {
	err := patchModuleConfigEnabled(ctx, name, enabled)
	if err == nil {
		return fmt.Errorf("expected admission denial patching ModuleConfig %s enabled=%t, but it succeeded", name, enabled)
	}
	msg := strings.ToLower(err.Error())
	if apierrors.IsForbidden(err) || strings.Contains(msg, "elasticcluster") || strings.Contains(msg, "cannot disable") {
		return nil
	}
	return fmt.Errorf("ModuleConfig %s patch was rejected but not by the expected disable guard: %w", name, err)
}

func setForceDisableAnnotation(ctx context.Context, name string) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:"true"}}}`, forceDisableAnnotation))
	_, err := suiteDyn.Resource(moduleConfigGVR).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func allowDeckhouseDisabling(ctx context.Context, name string) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"true"}}}`, allowDisablingLabelKey))
	_, err := suiteDyn.Resource(moduleConfigGVR).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// waitModuleUninstalled blocks until the module's footprint is gone: the
// d8-sds-elastic namespace and the mc-validation ValidatingWebhookConfiguration
// (removed by hook 030 + helm delete).
func waitModuleUninstalled(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		nsGone := false
		var ns corev1.Namespace
		errNs := suiteK8s.Get(ctx, client.ObjectKey{Name: d8ElasticNamespace}, &ns)
		if apierrors.IsNotFound(errNs) {
			nsGone = true
		}

		vwcGone := false
		var vwc admissionregistrationv1.ValidatingWebhookConfiguration
		errVwc := suiteK8s.Get(ctx, client.ObjectKey{Name: mcValidationWebhookName}, &vwc)
		if apierrors.IsNotFound(errVwc) {
			vwcGone = true
		}

		if nsGone && vwcGone {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for sds-elastic module uninstall: namespaceGone=%v webhookGone=%v", nsGone, vwcGone)
		}
		if !sleepCtx(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}

// applyElasticClusterNoWait applies a bare ElasticCluster without waiting for
// Ready — enough to arm the disable guard (which only counts EC existence).
func applyElasticClusterNoWait(ctx context.Context, name string) error {
	return storagekube.CreateElasticCluster(ctx, suiteRestCfg, storagekube.ElasticClusterParams{
		Name:                           name,
		NodeSelectorMatchLabels:        ecNodeSelector(),
		BlockDeviceSelectorMatchLabels: ecBDSelector(),
		NetworkPublic:                  suiteCfg.networkPublic,
		NetworkCluster:                 suiteCfg.networkCluster,
	})
}

// manualReclaimCleanup performs the documented post-uninstall cleanup: delete
// the PV/LLV/LVG the EC left behind (matched by the cluster-owner label) and
// strip the OSD label from the BlockDevices. These objects belong to
// sds-node-configurator / the core and outlive the sds-elastic module.
func manualReclaimCleanup(ctx context.Context) error {
	var errs []string

	for _, gvr := range []schema.GroupVersionResource{persistentVolumeGVR, lvmLogicalVolumeGVR, lvmVolumeGroupGVR} {
		list, err := suiteDyn.Resource(gvr).List(ctx, metav1.ListOptions{LabelSelector: clusterOwnerLabelKey})
		if err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			}
			errs = append(errs, fmt.Sprintf("list %s by %s: %v", gvr.Resource, clusterOwnerLabelKey, err))
			continue
		}
		for i := range list.Items {
			name := list.Items[i].GetName()
			if err := suiteDyn.Resource(gvr).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Sprintf("delete %s %s: %v", gvr.Resource, name, err))
			}
		}
	}

	// Strip the OSD label from BlockDevices (merge-patch the key to null).
	bds, err := suiteDyn.Resource(blockDeviceGVR).List(ctx, metav1.ListOptions{LabelSelector: suiteCfg.osdBDLabelKey})
	if err != nil {
		if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			errs = append(errs, fmt.Sprintf("list BlockDevices by %s: %v", suiteCfg.osdBDLabelKey, err))
		}
	} else {
		patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:null}}}`, suiteCfg.osdBDLabelKey))
		for i := range bds.Items {
			name := bds.Items[i].GetName()
			if _, err := suiteDyn.Resource(blockDeviceGVR).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Sprintf("unlabel BlockDevice %s: %v", name, err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("manual reclaim cleanup had errors:\n- %s", strings.Join(errs, "\n- "))
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
