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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

const (
	cephVerSquid    = "ceph version 19.2.3 (1ac1f3d) squid (stable)"
	cephVerReef     = "ceph version 18.2.0 (5dd24139) reef (stable)"
	cephVerQuincyA  = "ceph version 17.2.7 (b12291d) quincy (stable)"
	cephVerQuincyAA = "ceph version 17.2.6 (d7ff0d10) quincy (stable)"
)

var _ = Describe("populateObservability", func() {
	var (
		ctx = context.Background()
		ec  = newTestElasticCluster()
	)

	Context("CephCluster carries a fully-populated status.ceph", func() {
		It("lifts health, Quantity capacity, check details, and per-daemon byVersion onto the EC.status builder", func() {
			cc := newCephClusterUnstructured(ec, "Ready", "v19.2.3", "registry.example.com/ceph:v19.2.3")
			withCephClusterCephStatus(cc,
				"HEALTH_WARN",
				"1 mon down",
				"2026-05-25T10:00:00Z",
				600*1024*1024*1024, 200*1024*1024*1024, 400*1024*1024*1024,
				"2026-05-25T10:01:00Z",
				map[string]map[string]string{
					"MON_DOWN":     {"severity": "HEALTH_WARN", "message": "1/3 mons down"},
					"OSD_NEARFULL": {"severity": "HEALTH_ERR", "message": "OSD nearfull"},
				},
				map[string]map[string]int32{
					"osd": {cephVerSquid: 6, cephVerReef: 3},
					"mon": {cephVerSquid: 3},
					"mgr": {cephVerSquid: 1},
				},
			)

			cl := newFakeClient(ec, cc)
			r := newElasticClusterReconciler(cl)
			sb := newECStatusBuilder(ec)

			r.populateObservability(ctx, ec, sb, 9)

			By("health is parsed and HEALTH_ERR sorts before HEALTH_WARN")
			Expect(sb.health).NotTo(BeNil())
			Expect(sb.health.Status).To(Equal("HEALTH_WARN"))
			Expect(sb.health.Message).To(Equal("1 mon down"))
			Expect(sb.health.LastChecked).NotTo(BeNil())
			Expect(sb.health.Checks).To(HaveLen(2))
			Expect(sb.health.Checks[0].Name).To(Equal("OSD_NEARFULL"))
			Expect(sb.health.Checks[0].Severity).To(Equal("HEALTH_ERR"))
			Expect(sb.health.Checks[1].Name).To(Equal("MON_DOWN"))

			By("capacity is exposed as resource.Quantity (BinarySI)")
			Expect(sb.capacity).NotTo(BeNil())
			Expect(sb.capacity.Total.String()).To(Equal("600Gi"))
			Expect(sb.capacity.Used.String()).To(Equal("200Gi"))
			Expect(sb.capacity.Available.String()).To(Equal("400Gi"))
			Expect(sb.capacity.Total.Cmp(resource.MustParse("600Gi"))).To(BeZero())
			Expect(sb.capacity.UsedPercent).To(Equal("33.33"))
			Expect(sb.capacity.LastUpdated).NotTo(BeNil())

			By("osds publishes desired plus the byVersion histogram, sorted by count desc")
			Expect(sb.osds).NotTo(BeNil())
			Expect(sb.osds.Desired).To(Equal(int32(9)))
			Expect(sb.osds.KnownToCeph).To(Equal(int32(9)))
			Expect(sb.osds.ByVersion).To(HaveLen(2))
			Expect(sb.osds.ByVersion[0]).To(Equal(v1alpha1.DaemonVersionCount{Version: cephVerSquid, Count: 6}))
			Expect(sb.osds.ByVersion[1]).To(Equal(v1alpha1.DaemonVersionCount{Version: cephVerReef, Count: 3}))

			By("mons / mgrs publish the same shape, no Pod-level data")
			Expect(sb.mons).NotTo(BeNil())
			Expect(sb.mons.KnownToCeph).To(Equal(int32(3)))
			Expect(sb.mons.ByVersion).To(HaveLen(1))
			Expect(sb.mons.ByVersion[0].Count).To(Equal(int32(3)))

			Expect(sb.mgrs).NotTo(BeNil())
			Expect(sb.mgrs.KnownToCeph).To(Equal(int32(1)))
			Expect(sb.mgrs.ByVersion).To(HaveLen(1))
		})
	})

	Context("CephCluster does not yet exist", func() {
		It("leaves health/capacity/mons/mgrs nil but still surfaces the desired OSD count", func() {
			cl := newFakeClient(ec)
			r := newElasticClusterReconciler(cl)
			sb := newECStatusBuilder(ec)

			r.populateObservability(ctx, ec, sb, 5)

			Expect(sb.health).To(BeNil())
			Expect(sb.capacity).To(BeNil())
			Expect(sb.osds).NotTo(BeNil())
			Expect(sb.osds.Desired).To(Equal(int32(5)))
			Expect(sb.osds.KnownToCeph).To(BeZero())
			Expect(sb.osds.ByVersion).To(BeNil())
			Expect(sb.mons).To(BeNil())
			Expect(sb.mgrs).To(BeNil())
		})
	})

	Context("partially observed CephCluster (no capacity, no versions yet)", func() {
		It("publishes health-only without forcing a synthetic zero capacity or empty version blocks", func() {
			cc := newCephClusterUnstructured(ec, "Progressing", "", "registry.example.com/ceph:v19.2.3")
			withCephClusterCephStatus(cc, "HEALTH_OK", "", "", 0, 0, 0, "", nil, nil)
			cl := newFakeClient(ec, cc)
			r := newElasticClusterReconciler(cl)
			sb := newECStatusBuilder(ec)

			r.populateObservability(ctx, ec, sb, 0)

			Expect(sb.health).NotTo(BeNil())
			Expect(sb.health.Status).To(Equal("HEALTH_OK"))
			Expect(sb.capacity).To(BeNil(),
				"capacity must stay nil while bytesTotal == 0; otherwise the UI would render an empty 0/0 bar")
			Expect(sb.osds).To(BeNil(),
				"no versions and zero desired → osds stays nil so the UI does not render an empty card")
			Expect(sb.mons).To(BeNil())
			Expect(sb.mgrs).To(BeNil())
		})
	})

	Context("CephCluster.status.ceph.versions block missing for some daemon kinds", func() {
		It("publishes only the kinds Ceph actually reported", func() {
			cc := newCephClusterUnstructured(ec, "Ready", "v19.2.3", "registry.example.com/ceph:v19.2.3")
			withCephClusterCephStatus(cc,
				"HEALTH_OK", "", "2026-05-25T10:00:00Z",
				0, 0, 0, "", nil,
				map[string]map[string]int32{
					"osd": {cephVerSquid: 1},
					// mon and mgr intentionally missing
				},
			)
			cl := newFakeClient(ec, cc)
			r := newElasticClusterReconciler(cl)
			sb := newECStatusBuilder(ec)

			r.populateObservability(ctx, ec, sb, 1)

			Expect(sb.osds).NotTo(BeNil())
			Expect(sb.osds.KnownToCeph).To(Equal(int32(1)))
			Expect(sb.mons).To(BeNil(), "mon kind missing → DaemonStatus stays nil")
			Expect(sb.mgrs).To(BeNil(), "mgr kind missing → DaemonStatus stays nil")
		})
	})
})

var _ = Describe("parseCephDaemonsByVersion", func() {
	It("sorts byVersion by count desc, then version asc", func() {
		cephObj := map[string]interface{}{
			"versions": map[string]interface{}{
				"osd": map[string]interface{}{
					cephVerSquid:    float64(2),
					cephVerReef:     float64(2),
					cephVerQuincyA:  float64(5),
					cephVerQuincyAA: float64(1),
				},
			},
		}
		total, hist := parseCephDaemonsByVersion(cephObj, "osd")
		Expect(total).To(Equal(int32(10)))
		Expect(hist).To(HaveLen(4))
		// count desc
		Expect(hist[0].Version).To(Equal(cephVerQuincyA))
		Expect(hist[0].Count).To(Equal(int32(5)))
		// ties on count=2 break by version asc — Reef sorts before Squid
		// alphabetically (the leading "ceph version 18..." < "ceph version 19...").
		Expect(hist[1].Version).To(Equal(cephVerReef))
		Expect(hist[1].Count).To(Equal(int32(2)))
		Expect(hist[2].Version).To(Equal(cephVerSquid))
		Expect(hist[2].Count).To(Equal(int32(2)))
		// quincyAA tail
		Expect(hist[3].Version).To(Equal(cephVerQuincyAA))
		Expect(hist[3].Count).To(Equal(int32(1)))
	})

	It("returns (0, nil) when the kind is absent", func() {
		cephObj := map[string]interface{}{
			"versions": map[string]interface{}{
				"osd": map[string]interface{}{cephVerSquid: float64(3)},
			},
		}
		total, hist := parseCephDaemonsByVersion(cephObj, "mon")
		Expect(total).To(BeZero())
		Expect(hist).To(BeNil())
	})

	It("returns (0, nil) when the versions block is missing", func() {
		total, hist := parseCephDaemonsByVersion(map[string]interface{}{}, "osd")
		Expect(total).To(BeZero())
		Expect(hist).To(BeNil())
	})

	It("drops zero / negative counts", func() {
		cephObj := map[string]interface{}{
			"versions": map[string]interface{}{
				"osd": map[string]interface{}{
					cephVerSquid: float64(0),
					cephVerReef:  float64(-1),
				},
			},
		}
		total, hist := parseCephDaemonsByVersion(cephObj, "osd")
		Expect(total).To(BeZero())
		Expect(hist).To(BeNil(),
			"all entries dropped → block stays nil so the UI does not render an empty card")
	})

	It("accepts int64 / int counts (defensive — Rook publishes JSON numbers via float64)", func() {
		cephObj := map[string]interface{}{
			"versions": map[string]interface{}{
				"osd": map[string]interface{}{
					cephVerSquid: int64(4),
					cephVerReef:  int(2),
				},
			},
		}
		total, hist := parseCephDaemonsByVersion(cephObj, "osd")
		Expect(total).To(Equal(int32(6)))
		Expect(hist).To(HaveLen(2))
		Expect(hist[0].Version).To(Equal(cephVerSquid))
		Expect(hist[0].Count).To(Equal(int32(4)))
	})
})

var _ = Describe("parseCephHealth", func() {
	It("returns nil when status.ceph is missing", func() {
		cc := &unstructured.Unstructured{Object: map[string]interface{}{
			"status": map[string]interface{}{},
		}}
		Expect(parseCephHealth(cc)).To(BeNil())
	})

	It("caps the checks slice at maxHealthChecks", func() {
		details := map[string]interface{}{}
		for i := 0; i < maxHealthChecks*2; i++ {
			details[fmt.Sprintf("CHECK_%03d", i)] = map[string]interface{}{
				"severity": "HEALTH_WARN",
				"message":  "noise",
			}
		}
		cc := &unstructured.Unstructured{}
		cc.SetGroupVersionKind(external.CephClusterGVK)
		cc.Object["status"] = map[string]interface{}{
			"ceph": map[string]interface{}{
				"health":  "HEALTH_WARN",
				"details": details,
			},
		}
		out := parseCephHealth(cc)
		Expect(out).NotTo(BeNil())
		Expect(out.Checks).To(HaveLen(maxHealthChecks))
	})
})

var _ = Describe("checkSeverityRank", func() {
	It("ranks ERR before WARN before unknown", func() {
		Expect(checkSeverityRank("HEALTH_ERR")).To(BeNumerically("<", checkSeverityRank("HEALTH_WARN")))
		Expect(checkSeverityRank("HEALTH_WARN")).To(BeNumerically("<", checkSeverityRank("")))
	})
})

var _ = Describe("ElasticClusterReconciler.Reconcile observability surface", func() {
	It("publishes Quantity capacity and byVersion histograms alongside the FSM verdict", func() {
		ctx := context.Background()
		ec := newTestElasticCluster()

		cc := newCephClusterUnstructured(ec, "Progressing", "", "registry.example.com/ceph:v19.2.3")
		withCephClusterCephStatus(cc,
			"HEALTH_OK", "", "2026-05-25T10:00:00Z",
			900*1024*1024*1024, 100*1024*1024*1024, 800*1024*1024*1024,
			"2026-05-25T10:01:00Z", nil,
			map[string]map[string]int32{
				"osd": {cephVerSquid: 1},
				"mon": {cephVerSquid: 1},
				"mgr": {cephVerSquid: 1},
			},
		)

		cl := newFakeClient(
			ec,
			newTestNode("node-a"),
			newBlockDevice("bd-a", "node-a", "100Gi", true, nil),
			cc,
		)
		r := newElasticClusterReconciler(cl)

		// Drive a single reconcile — the FSM gates StorageReady on LVG
		// phase and bails out, but the observability fields must still
		// land on the published status.
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: testECName}})
		Expect(err).NotTo(HaveOccurred())

		latest := &v1alpha1.ElasticCluster{}
		Expect(cl.Get(ctx, types.NamespacedName{Name: testECName}, latest)).To(Succeed())
		Expect(latest.Status).NotTo(BeNil())
		Expect(latest.Status.Health).NotTo(BeNil())
		Expect(latest.Status.Health.Status).To(Equal("HEALTH_OK"))

		Expect(latest.Status.Capacity).NotTo(BeNil())
		Expect(latest.Status.Capacity.Total.String()).To(Equal("900Gi"))
		Expect(latest.Status.Capacity.Used.String()).To(Equal("100Gi"))
		Expect(latest.Status.Capacity.Available.String()).To(Equal("800Gi"))
		Expect(latest.Status.Capacity.UsedPercent).To(Equal("11.11"))

		Expect(latest.Status.OSDs).NotTo(BeNil())
		Expect(latest.Status.OSDs.Desired).To(Equal(int32(1)))
		Expect(latest.Status.OSDs.KnownToCeph).To(Equal(int32(1)))
		Expect(latest.Status.OSDs.ByVersion).To(HaveLen(1))
		Expect(latest.Status.OSDs.ByVersion[0].Version).To(Equal(cephVerSquid))

		Expect(latest.Status.Mons).NotTo(BeNil())
		Expect(latest.Status.Mons.KnownToCeph).To(Equal(int32(1)))
		Expect(latest.Status.Mgrs).NotTo(BeNil())
		Expect(latest.Status.Mgrs.KnownToCeph).To(Equal(int32(1)))
	})
})
