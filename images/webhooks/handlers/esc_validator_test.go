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

package handlers

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/slok/kubewebhook/v2/pkg/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

var _ = Describe("ElasticStorageClassValidate", func() {
	var ctx = context.Background()

	// validate runs the ESC validator with no preloaded dynamic objects.
	// All non-HighRedundancy paths take this branch — the validator does
	// not call the dynamic client unless `replication=HighRedundancy` on
	// CREATE, so a bare fake client suffices.
	validate := func(op model.AdmissionReviewOp, oldObj, newObj *unstructured.Unstructured) (bool, string, error) {
		validator := NewElasticStorageClassValidator(dynClient())
		res, err := validator(ctx, admissionReview(op, oldObj), newObj)
		if err != nil {
			return false, "", err
		}
		return res.Valid, res.Message, nil
	}

	// validateWith mirrors validate but lets a test seed the dynamic
	// client with an EC, BlockDevices, and Nodes for the HighRedundancy
	// preflight to consume.
	validateWith := func(objs []runtime.Object, op model.AdmissionReviewOp, oldObj, newObj *unstructured.Unstructured) (bool, string, error) {
		validator := NewElasticStorageClassValidator(dynClient(objs...))
		res, err := validator(ctx, admissionReview(op, oldObj), newObj)
		if err != nil {
			return false, "", err
		}
		return res.Valid, res.Message, nil
	}

	It("rejects reserved OSD StorageClass name on CREATE", func() {
		obj := newESCUnstructured(reservedOSDStorageClassName, escSpec("demo", storageClassTypeRBD, ""))
		valid, msg, err := validate(model.OperationCreate, nil, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("reserved"))
	})

	It("rejects RBD + ErasureCodedCompact on CREATE", func() {
		obj := newESCUnstructured("pool", escSpec("demo", storageClassTypeRBD, replicationErasureCodedCompact))
		valid, msg, err := validate(model.OperationCreate, nil, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("ErasureCodedCompact"))
	})

	It("rejects CephFS + ErasureCodedCompact on CREATE", func() {
		obj := newESCUnstructured("pool", escSpec("demo", "CephFS", replicationErasureCodedCompact))
		valid, msg, err := validate(model.OperationCreate, nil, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("ErasureCodedCompact"))
	})

	It("accepts valid CREATE", func() {
		obj := newESCUnstructured("pool", escSpec("demo", storageClassTypeRBD, "ConsistencyAndAvailability"))
		valid, _, err := validate(model.OperationCreate, nil, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeTrue())
	})

	It("rejects clusterRef mutation on UPDATE", func() {
		oldESC := newESCUnstructured("pool", escSpec("ec-a", storageClassTypeRBD, ""))
		updated := newESCUnstructured("pool", escSpec("ec-b", storageClassTypeRBD, ""))
		valid, msg, err := validate(model.OperationUpdate, oldESC, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("clusterRef"))
	})

	It("accepts unchanged clusterRef on UPDATE", func() {
		spec := escSpec("demo", storageClassTypeRBD, "")
		oldESC := newESCUnstructured("pool", spec)
		updated := newESCUnstructured("pool", spec)
		valid, _, err := validate(model.OperationUpdate, oldESC, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeTrue())
	})

	It("rejects type mutation on UPDATE", func() {
		oldESC := newESCUnstructured("pool", escSpec("demo", storageClassTypeRBD, ""))
		updated := newESCUnstructured("pool", escSpec("demo", "CephFS", ""))
		valid, _, err := validate(model.OperationUpdate, oldESC, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
	})

	It("rejects replication mutation on UPDATE", func() {
		oldESC := newESCUnstructured("pool", escSpec("demo", storageClassTypeRBD, "ConsistencyAndAvailability"))
		updated := newESCUnstructured("pool", escSpec("demo", storageClassTypeRBD, "AvailabilityWithoutConsistency"))
		valid, msg, err := validate(model.OperationUpdate, oldESC, updated)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid).To(BeFalse())
		Expect(msg).To(ContainSubstring("replication"))
	})

	It("fail-closes on unexpected object type", func() {
		validator := NewElasticStorageClassValidator(dynClient())
		_, err := validator(ctx, admissionReview(model.OperationCreate, nil), &corev1.Pod{})
		Expect(err).To(HaveOccurred())
	})

	Describe("HighRedundancy preflight", func() {
		// helper: produce N nodes labelled role=storage and M
		// BlockDevices owned by `ec` spread across the supplied
		// nodeNames (one BD per nodeName by default — pass duplicates
		// to simulate multiple BDs on the same host).
		nodeLabels := map[string]string{"role": "storage"}
		ecName := "ec-prod"

		makeNodes := func(n int) []runtime.Object {
			out := make([]runtime.Object, 0, n)
			for i := 0; i < n; i++ {
				out = append(out, nodeUnstructured(nodeNameOf(i), nodeLabels))
			}
			return out
		}
		makeBDsOnNodes := func(nodeNames ...string) []runtime.Object {
			out := make([]runtime.Object, 0, len(nodeNames))
			for i, n := range nodeNames {
				out = append(out, bdUnstructured(bdNameOf(i), ecName, n, nil))
			}
			return out
		}
		newHRESC := func() *unstructured.Unstructured {
			return newESCUnstructured("rbd-hr", escSpec(ecName, storageClassTypeRBD, replicationHighRedundancy))
		}
		nodeSel := matchLabels(nodeLabels)

		It("rejects CREATE when parent EC is missing", func() {
			esc := newHRESC()
			valid, msg, err := validateWith(nil, model.OperationCreate, nil, esc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeFalse())
			Expect(msg).To(ContainSubstring(`"ec-prod"`))
			Expect(msg).To(ContainSubstring("does not exist"))
			Expect(msg).To(ContainSubstring("HighRedundancy"))
		})

		It("accepts CREATE when EC + 5 nodeSelector-matched nodes + 4 distinct OSD-host nodes are present", func() {
			seed := append(makeNodes(5),
				newECStubUnstructured(ecName, nodeSel),
			)
			seed = append(seed, makeBDsOnNodes("node-0", "node-1", "node-2", "node-3")...)
			esc := newHRESC()
			valid, msg, err := validateWith(seed, model.OperationCreate, nil, esc)
			Expect(err).NotTo(HaveOccurred(), "msg=%s", msg)
			Expect(valid).To(BeTrue(), "msg=%s", msg)
		})

		It("rejects CREATE when fewer than 5 nodes match nodeSelector", func() {
			seed := append(makeNodes(4),
				newECStubUnstructured(ecName, nodeSel),
			)
			seed = append(seed, makeBDsOnNodes("node-0", "node-1", "node-2", "node-3")...)
			esc := newHRESC()
			valid, msg, err := validateWith(seed, model.OperationCreate, nil, esc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeFalse())
			Expect(msg).To(ContainSubstring("at least 5"))
			Expect(msg).To(ContainSubstring("have 4"))
			Expect(msg).To(ContainSubstring(ecName))
		})

		It("rejects CREATE when fewer than 4 distinct nodes host adopted BlockDevices", func() {
			seed := append(makeNodes(5),
				newECStubUnstructured(ecName, nodeSel),
			)
			seed = append(seed, makeBDsOnNodes("node-0", "node-1", "node-2")...)
			esc := newHRESC()
			valid, msg, err := validateWith(seed, model.OperationCreate, nil, esc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeFalse())
			Expect(msg).To(ContainSubstring("at least 4 distinct nodes"))
			Expect(msg).To(ContainSubstring("have 3"))
		})

		It("rejects CREATE when 4 BDs are clustered on only 3 nodes (distinct count, not BD count)", func() {
			seed := append(makeNodes(5),
				newECStubUnstructured(ecName, nodeSel),
			)
			// node-0 hosts two BDs; the distinct-node count is 3, not 4.
			seed = append(seed, makeBDsOnNodes("node-0", "node-0", "node-1", "node-2")...)
			esc := newHRESC()
			valid, msg, err := validateWith(seed, model.OperationCreate, nil, esc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeFalse())
			Expect(msg).To(ContainSubstring("have 3"))
		})

		It("accepts CREATE when 4 BDs lie on exactly 4 distinct nodes (boundary case)", func() {
			seed := append(makeNodes(5),
				newECStubUnstructured(ecName, nodeSel),
			)
			seed = append(seed, makeBDsOnNodes("node-0", "node-1", "node-2", "node-3")...)
			esc := newHRESC()
			valid, msg, err := validateWith(seed, model.OperationCreate, nil, esc)
			Expect(err).NotTo(HaveOccurred(), "msg=%s", msg)
			Expect(valid).To(BeTrue(), "msg=%s", msg)
		})

		It("does NOT trigger preflight for non-HighRedundancy ESCs even on a tiny cluster", func() {
			// No EC, no nodes, no BDs — the validator must skip the
			// preflight entirely when replication is anything other
			// than HighRedundancy. This guards against accidentally
			// gating ConsistencyAndAvailability on the HR thresholds.
			esc := newESCUnstructured("rbd-prod", escSpec(ecName, storageClassTypeRBD, "ConsistencyAndAvailability"))
			valid, _, err := validateWith(nil, model.OperationCreate, nil, esc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeTrue())
		})
	})

	Describe("PG budget preflight", func() {
		ecName := "ec-pg"

		// makeBDs adopts n BlockDevices for ecName (one OSD per BD).
		makeBDs := func(n int) []runtime.Object {
			out := make([]runtime.Object, 0, n)
			for i := 0; i < n; i++ {
				out = append(out, bdUnstructured(bdNameOf(i), ecName, nodeNameOf(i), nil))
			}
			return out
		}

		It("accepts CREATE when pgNum stays within the PGs-per-OSD budget", func() {
			// 128 x size 3 / 3 OSD = 128 PG/OSD (<= 200).
			seed := makeBDs(3)
			esc := newESCUnstructured("pool", escSpecWithPG(ecName, "ConsistencyAndAvailability", 128))
			valid, msg, err := validateWith(seed, model.OperationCreate, nil, esc)
			Expect(err).NotTo(HaveOccurred(), "msg=%s", msg)
			Expect(valid).To(BeTrue(), "msg=%s", msg)
		})

		It("rejects CREATE when a single pool's pgNum blows the budget", func() {
			// 512 x size 3 / 3 OSD = 512 PG/OSD (> 200).
			seed := makeBDs(3)
			esc := newESCUnstructured("pool", escSpecWithPG(ecName, "ConsistencyAndAvailability", 512))
			valid, msg, err := validateWith(seed, model.OperationCreate, nil, esc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeFalse())
			Expect(msg).To(ContainSubstring("per OSD"))
			Expect(msg).To(ContainSubstring("512"))
			Expect(msg).To(ContainSubstring(ecName))
		})

		It("uses the replication factor: size-2 pool fits where size-3 would not", func() {
			// AvailabilityWithoutConsistency is size 2: 256 x 2 / 3 = 171
			// PG/OSD (<= 200). The same pgNum at size 3 would be 256 (> 200),
			// so this proves the replica factor feeds the projection.
			seed := makeBDs(3)
			esc := newESCUnstructured("pool", escSpecWithPG(ecName, "AvailabilityWithoutConsistency", 256))
			valid, msg, err := validateWith(seed, model.OperationCreate, nil, esc)
			Expect(err).NotTo(HaveOccurred(), "msg=%s", msg)
			Expect(valid).To(BeTrue(), "msg=%s", msg)
		})

		It("rejects CREATE when the aggregate across sibling pinned pools blows the budget", func() {
			// Existing sibling pins 128 (x3), the new pool pins 128 (x3):
			// (128+128) x 3 / 3 OSD = 256 PG/OSD (> 200).
			seed := makeBDs(3)
			seed = append(seed, newESCUnstructured("sibling", escSpecWithPG(ecName, "ConsistencyAndAvailability", 128)))
			esc := newESCUnstructured("pool", escSpecWithPG(ecName, "ConsistencyAndAvailability", 128))
			valid, msg, err := validateWith(seed, model.OperationCreate, nil, esc)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeFalse())
			Expect(msg).To(ContainSubstring("per OSD"))
		})

		It("ignores pinned pools of a different cluster", func() {
			// Sibling on another cluster pins 512, but it must not count
			// against ec-pg's budget: 128 x 3 / 3 OSD = 128 (<= 200).
			seed := makeBDs(3)
			seed = append(seed, newESCUnstructured("other-pool", escSpecWithPG("other-cluster", "ConsistencyAndAvailability", 512)))
			esc := newESCUnstructured("pool", escSpecWithPG(ecName, "ConsistencyAndAvailability", 128))
			valid, msg, err := validateWith(seed, model.OperationCreate, nil, esc)
			Expect(err).NotTo(HaveOccurred(), "msg=%s", msg)
			Expect(valid).To(BeTrue(), "msg=%s", msg)
		})

		It("skips the budget check when the cluster has no adopted OSDs yet", func() {
			// No BlockDevices seeded: OSD count is unknown, so even an
			// oversized pgNum is deferred to Ceph rather than blocked.
			esc := newESCUnstructured("pool", escSpecWithPG(ecName, "ConsistencyAndAvailability", 512))
			valid, msg, err := validateWith(nil, model.OperationCreate, nil, esc)
			Expect(err).NotTo(HaveOccurred(), "msg=%s", msg)
			Expect(valid).To(BeTrue(), "msg=%s", msg)
		})

		It("excludes the ESC's own stored pgNum on UPDATE (uses the new value)", func() {
			// Stored self pins 256; the UPDATE lowers it to 128. Counting
			// the stored 256 too would yield (128+256) x 3 / 3 = 384 (> 200)
			// and wrongly reject; excluding self gives 128 (<= 200).
			spec := func(pg int64) map[string]interface{} {
				return escSpecWithPG(ecName, "ConsistencyAndAvailability", pg)
			}
			seed := makeBDs(3)
			seed = append(seed, newESCUnstructured("pool", spec(256)))
			oldObj := newESCUnstructured("pool", spec(256))
			updated := newESCUnstructured("pool", spec(128))
			valid, msg, err := validateWith(seed, model.OperationUpdate, oldObj, updated)
			Expect(err).NotTo(HaveOccurred(), "msg=%s", msg)
			Expect(valid).To(BeTrue(), "msg=%s", msg)
		})

		It("rejects an UPDATE that bumps pgNum past the budget", func() {
			spec := func(pg int64) map[string]interface{} {
				return escSpecWithPG(ecName, "ConsistencyAndAvailability", pg)
			}
			seed := makeBDs(3)
			seed = append(seed, newESCUnstructured("pool", spec(128)))
			oldObj := newESCUnstructured("pool", spec(128))
			updated := newESCUnstructured("pool", spec(512))
			valid, msg, err := validateWith(seed, model.OperationUpdate, oldObj, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(valid).To(BeFalse())
			Expect(msg).To(ContainSubstring("per OSD"))
		})
	})
})

// nodeNameOf and bdNameOf produce stable, ordering-friendly names for
// HR-preflight test fixtures: makeNodes(5) yields node-0..node-4 and
// makeBDsOnNodes("node-0", "node-1") yields bd-0/bd-1.
func nodeNameOf(i int) string { return "node-" + itoa(i) }
func bdNameOf(i int) string   { return "bd-" + itoa(i) }

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = digits[i%10]
		i /= 10
	}
	return string(buf[n:])
}
