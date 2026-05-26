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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

const (
	// Full ceph version strings exactly as Rook publishes them under
	// status.ceph.versions.<kind>. Keys must include the major.minor.patch
	// segment that versionMatches looks for.
	cephVerString1923 = "ceph version 19.2.3 (c92aebb279828e9c3c1f5d24613efca272649e62) squid (stable)"
	cephVerString2021 = "ceph version 20.2.1 (6a49aff47758778a5f5951e731d437c317f72fb2) tentacle (stable)"
)

var _ = Describe("probeCephUpgradeState", func() {
	desiredVersion := v1alpha1.DefaultCephVersion // "v19.2.3"
	desiredImage := newTestCfg().CephImages[v1alpha1.DefaultCephVersion]
	otherImage := "registry.example.com/ceph:v20.2.1"

	It("returns Done when versions.overall has a single key matching desired", func() {
		cc := newCephClusterUnstructured(newTestElasticCluster(), "Ready", "v19.2.3", desiredImage)
		withCephClusterCephStatus(cc, "HEALTH_OK", "", "", 0, 0, 0, "", nil, map[string]map[string]int32{
			"overall": {cephVerString1923: 9},
		})

		probe := probeCephUpgradeState(cc, desiredImage, desiredVersion)

		Expect(probe.Done).To(BeTrue())
		Expect(probe.InProgress).To(BeFalse())
		Expect(probe.Running).To(Equal(cephVerString1923))
		Expect(probe.Msg).To(ContainSubstring("running ceph version"))
	})

	It("returns InProgress with mixed versions.overall (mid-roll)", func() {
		// Faithful reproduction of the user-reported snapshot: image
		// has been bumped to v20.2.1, mon/mgr already on the new
		// version, OSDs still on the old one — versions.overall is
		// multi-key. Probe must say InProgress=True even though
		// status.version.version Rook publishes is the new version.
		cc := newCephClusterUnstructured(newTestElasticCluster(), "Progressing", "20.2.1-0", otherImage)
		withCephClusterCephStatus(cc, "HEALTH_OK", "", "", 0, 0, 0, "", nil, map[string]map[string]int32{
			"mon":     {cephVerString2021: 3},
			"mgr":     {cephVerString2021: 2},
			"osd":     {cephVerString1923: 4},
			"overall": {cephVerString1923: 4, cephVerString2021: 5},
		})

		probe := probeCephUpgradeState(cc, otherImage, "v20.2.1")

		Expect(probe.Done).To(BeFalse())
		Expect(probe.InProgress).To(BeTrue())
		// Lagging version surfaced on the printcolumn so callers
		// see the still-rolling daemons' version, not Rook's marker.
		Expect(probe.Running).To(Equal(cephVerString1923))
		Expect(probe.Msg).To(ContainSubstring("Rook rolling pods"))
		Expect(probe.Msg).To(ContainSubstring("19.2.3"))
		Expect(probe.Msg).To(ContainSubstring("20.2.1"))
	})

	It("returns InProgress when versions.overall has a single key that does not match desired", func() {
		// Pre-bump steady state on the old version: image already
		// bumped (currentImage == desiredImage), but the cluster has
		// not yet started rolling, so versions.overall still carries
		// only the old release.
		cc := newCephClusterUnstructured(newTestElasticCluster(), "Ready", "20.2.1-0", otherImage)
		withCephClusterCephStatus(cc, "HEALTH_OK", "", "", 0, 0, 0, "", nil, map[string]map[string]int32{
			"overall": {cephVerString1923: 9},
		})

		probe := probeCephUpgradeState(cc, otherImage, "v20.2.1")

		Expect(probe.Done).To(BeFalse())
		Expect(probe.InProgress).To(BeTrue())
		Expect(probe.Msg).To(ContainSubstring("running="))
		Expect(probe.Msg).To(ContainSubstring("desired=v20.2.1"))
	})

	It("returns InProgress when image bump is queued and cluster is healthy", func() {
		cc := newCephClusterUnstructured(newTestElasticCluster(), "Ready", "v19.2.3", otherImage)
		withCephClusterCephStatus(cc, "HEALTH_OK", "", "", 0, 0, 0, "", nil, nil)

		probe := probeCephUpgradeState(cc, desiredImage, desiredVersion)

		Expect(probe.Done).To(BeFalse())
		Expect(probe.InProgress).To(BeTrue())
		Expect(probe.Msg).To(ContainSubstring("queued"))
	})

	It("blocks the bump when image differs and cluster is HEALTH_ERR (pre-upgrade gate)", func() {
		cc := newCephClusterUnstructured(newTestElasticCluster(), "Ready", "v19.2.3", otherImage)
		withCephClusterCephStatus(cc, "HEALTH_ERR", "", "", 0, 0, 0, "", nil, nil)

		probe := probeCephUpgradeState(cc, desiredImage, desiredVersion)

		Expect(probe.Done).To(BeFalse())
		Expect(probe.InProgress).To(BeFalse())
		Expect(probe.Msg).To(ContainSubstring("pre-upgrade gate"))
	})

	It("falls back to status.version.version when versions.overall is absent", func() {
		// Bootstrap edge case: Rook has not yet populated
		// status.ceph.versions, so we have to trust status.version.version.
		cc := newCephClusterUnstructured(newTestElasticCluster(), "Ready", "v19.2.3", desiredImage)

		probe := probeCephUpgradeState(cc, desiredImage, desiredVersion)

		Expect(probe.Done).To(BeTrue())
		Expect(probe.InProgress).To(BeFalse())
		Expect(probe.Running).To(Equal("v19.2.3"))
	})

	It("falls back to InProgress when versions.overall is absent and version field mismatches", func() {
		cc := newCephClusterUnstructured(newTestElasticCluster(), "Ready", "v18.2.0", desiredImage)

		probe := probeCephUpgradeState(cc, desiredImage, desiredVersion)

		Expect(probe.Done).To(BeFalse())
		Expect(probe.InProgress).To(BeTrue())
		Expect(probe.Running).To(Equal("v18.2.0"))
		Expect(probe.Msg).To(ContainSubstring("running=v18.2.0"))
		Expect(probe.Msg).To(ContainSubstring("desired=" + desiredVersion))
	})

	It("returns InProgress with empty Running when no version field nor versions.overall is published", func() {
		ec := newTestElasticCluster()
		cc := &unstructured.Unstructured{}
		cc.Object = map[string]interface{}{
			"spec":   map[string]interface{}{"cephVersion": map[string]interface{}{"image": desiredImage}},
			"status": map[string]interface{}{"phase": "Progressing"},
		}
		cc.SetName("ceph-" + ec.Name)

		probe := probeCephUpgradeState(cc, desiredImage, desiredVersion)

		Expect(probe.Done).To(BeFalse())
		Expect(probe.InProgress).To(BeTrue())
		Expect(probe.Running).To(BeEmpty())
		Expect(probe.Msg).To(ContainSubstring("no running ceph version yet"))
	})
})

var _ = Describe("ensureUpgrade", func() {
	var (
		ctx    = context.Background()
		ec     = newTestElasticCluster()
		status *ecStatusBuilder
		r      *ElasticClusterReconciler
	)

	BeforeEach(func() {
		status = newECStatusBuilder(ec)
	})

	It("returns done when versions.overall converges on desired", func() {
		cephImage := newTestCfg().CephImages[v1alpha1.DefaultCephVersion]
		cc := newCephClusterUnstructured(ec, "Ready", "v19.2.3", cephImage)
		withCephClusterCephStatus(cc, "HEALTH_OK", "", "", 0, 0, 0, "", nil, map[string]map[string]int32{
			"overall": {cephVerString1923: 9},
		})
		cl := newFakeClient(cc)
		r = newElasticClusterReconciler(cl)

		done, inProgress, msg, err := r.ensureUpgrade(ctx, ec, status)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
		Expect(inProgress).To(BeFalse())
		Expect(msg).To(ContainSubstring("running ceph version"))
		Expect(status.cephVersion.Running).To(Equal(cephVerString1923))
		Expect(status.cephVersion.Requested).To(Equal(v1alpha1.DefaultCephVersion))
	})

	It("returns in-progress when versions.overall is mixed", func() {
		cephImage := newTestCfg().CephImages[v1alpha1.DefaultCephVersion]
		cc := newCephClusterUnstructured(ec, "Progressing", "v19.2.3", cephImage)
		withCephClusterCephStatus(cc, "HEALTH_OK", "", "", 0, 0, 0, "", nil, map[string]map[string]int32{
			"overall": {cephVerString1923: 4, cephVerString2021: 5},
		})
		cl := newFakeClient(cc)
		r = newElasticClusterReconciler(cl)

		done, inProgress, msg, err := r.ensureUpgrade(ctx, ec, status)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(inProgress).To(BeTrue())
		Expect(msg).To(ContainSubstring("Rook rolling pods"))
		Expect(status.cephVersion.Running).To(Equal(cephVerString1923))
	})

	It("returns in-progress fallback when versions.overall is absent and running version differs", func() {
		cephImage := newTestCfg().CephImages[v1alpha1.DefaultCephVersion]
		cc := newCephClusterUnstructured(ec, "Ready", "v18.2.0", cephImage)
		cl := newFakeClient(cc)
		r = newElasticClusterReconciler(cl)

		done, inProgress, msg, err := r.ensureUpgrade(ctx, ec, status)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(inProgress).To(BeTrue())
		Expect(msg).To(ContainSubstring("running=v18.2.0"))
		Expect(msg).To(ContainSubstring("desired=" + v1alpha1.DefaultCephVersion))
	})

	It("returns in-progress when CephCluster is not yet visible", func() {
		cl := newFakeClient()
		r = newElasticClusterReconciler(cl)

		done, inProgress, msg, err := r.ensureUpgrade(ctx, ec, status)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse())
		Expect(inProgress).To(BeFalse())
		Expect(msg).To(ContainSubstring("not yet visible"))
	})
})
