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

package builder

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

var _ = Describe("builders", func() {
	ec := &v1alpha1.ElasticCluster{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	longEC := &v1alpha1.ElasticCluster{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 30)}}
	longBD := strings.Repeat("b", 40)

	Describe("OSDResourceShortHash", func() {
		It("is deterministic", func() {
			a := OSDResourceShortHash("demo", "bd-1")
			b := OSDResourceShortHash("demo", "bd-1")
			Expect(a).To(Equal(b))
			Expect(len(a)).To(Equal(8))
		})
	})

	Describe("resource name length", func() {
		It("keeps ECOSDResourceName within DNS label limits", func() {
			name := ECOSDResourceName(longEC, longBD)
			Expect(len(name)).To(BeNumerically("<=", 63))
		})

		It("keeps ECCephClusterName equal to EC name", func() {
			Expect(ECCephClusterName(ec)).To(Equal("demo"))
		})
	})

	Describe("ESCCephBlockPool", func() {
		It("maps AvailabilityWithoutConsistency to size=2", func() {
			esc := &v1alpha1.ElasticStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "sc"},
				Spec: v1alpha1.ElasticStorageClassSpec{
					Type:        v1alpha1.StorageClassTypeRBD,
					Replication: v1alpha1.ReplicationAvailabilityWithoutConsistency,
				},
			}
			pool, err := ESCCephBlockPool(esc, "ns")
			Expect(err).NotTo(HaveOccurred())
			repl, _, _ := unstructuredNestedInt64(pool, "spec", "replicated", "size")
			Expect(repl).To(Equal(int64(2)))
		})

		It("rejects ErasureCodedCompact for RBD", func() {
			esc := &v1alpha1.ElasticStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "sc"},
				Spec: v1alpha1.ElasticStorageClassSpec{
					Type:        v1alpha1.StorageClassTypeRBD,
					Replication: v1alpha1.ReplicationErasureCodedCompact,
				},
			}
			_, err := ESCCephBlockPool(esc, "ns")
			Expect(err).To(HaveOccurred())
		})

		It("maps HighRedundancy to size=4 with requireSafeReplicaSize=true for RBD", func() {
			esc := &v1alpha1.ElasticStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "sc"},
				Spec: v1alpha1.ElasticStorageClassSpec{
					Type:        v1alpha1.StorageClassTypeRBD,
					Replication: v1alpha1.ReplicationHighRedundancy,
				},
			}
			pool, err := ESCCephBlockPool(esc, "ns")
			Expect(err).NotTo(HaveOccurred())
			size, _, _ := unstructuredNestedInt64(pool, "spec", "replicated", "size")
			Expect(size).To(Equal(int64(4)))
			safe, found, _ := unstructured.NestedBool(pool.Object, "spec", "replicated", "requireSafeReplicaSize")
			Expect(found).To(BeTrue())
			Expect(safe).To(BeTrue())
		})
	})

	Describe("ESCCephFilesystem", func() {
		It("uses replicated metadata pool and EC data pool for CephFS", func() {
			esc := &v1alpha1.ElasticStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "fs"},
				Spec: v1alpha1.ElasticStorageClassSpec{
					Type:        v1alpha1.StorageClassTypeCephFS,
					Replication: v1alpha1.ReplicationErasureCodedCompact,
				},
			}
			fs, err := ESCCephFilesystem(esc, "ns")
			Expect(err).NotTo(HaveOccurred())
			metaSize, _, _ := unstructuredNestedInt64(fs, "spec", "metadataPool", "replicated", "size")
			Expect(metaSize).To(Equal(int64(ECMetadataPoolReplicatedSize)))

			pools, _, _ := unstructuredNestedSlice(fs, "spec", "dataPools")
			Expect(pools).To(HaveLen(1))
			ecMap, ok := pools[0].(map[string]interface{})
			Expect(ok).To(BeTrue())
			_, hasEC := ecMap["erasureCoded"]
			Expect(hasEC).To(BeTrue())
		})

		It("renders HighRedundancy CephFS data pool as replicated size=4 with requireSafeReplicaSize=true", func() {
			esc := &v1alpha1.ElasticStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "fs-hr"},
				Spec: v1alpha1.ElasticStorageClassSpec{
					Type:        v1alpha1.StorageClassTypeCephFS,
					Replication: v1alpha1.ReplicationHighRedundancy,
				},
			}
			fs, err := ESCCephFilesystem(esc, "ns")
			Expect(err).NotTo(HaveOccurred())

			pools, _, _ := unstructuredNestedSlice(fs, "spec", "dataPools")
			Expect(pools).To(HaveLen(1))
			pool0 := pools[0].(map[string]interface{})
			repl, ok := pool0["replicated"].(map[string]interface{})
			Expect(ok).To(BeTrue(), "HighRedundancy data pool must be replicated, not erasureCoded")
			Expect(repl["size"]).To(Equal(int64(4)))
			Expect(repl["requireSafeReplicaSize"]).To(Equal(true))
		})
	})

	Describe("ESCCephStorageClass", func() {
		It("builds RBD spec with pool name", func() {
			esc := &v1alpha1.ElasticStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "rbd-sc"},
				Spec: v1alpha1.ElasticStorageClassSpec{
					Type: v1alpha1.StorageClassTypeRBD,
				},
			}
			sc, err := ESCCephStorageClass(esc, "demo")
			Expect(err).NotTo(HaveOccurred())
			pool, _, _ := unstructuredNestedString(sc, "spec", "rbd", "pool")
			Expect(pool).To(Equal("rbd-sc"))
		})

		It("builds CephFS spec with fsName", func() {
			esc := &v1alpha1.ElasticStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "cephfs-sc"},
				Spec: v1alpha1.ElasticStorageClassSpec{
					Type: v1alpha1.StorageClassTypeCephFS,
				},
			}
			sc, err := ESCCephStorageClass(esc, "demo")
			Expect(err).NotTo(HaveOccurred())
			fsName, _, _ := unstructuredNestedString(sc, "spec", "cephFS", "fsName")
			Expect(fsName).To(Equal("cephfs-sc"))
		})

		It("errors on unsupported type", func() {
			esc := &v1alpha1.ElasticStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "bad"},
				Spec:       v1alpha1.ElasticStorageClassSpec{Type: v1alpha1.StorageClassType("Foo")},
			}
			_, err := ESCCephStorageClass(esc, "demo")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ECCephClusterConnection", func() {
		It("includes cephFS stanza when hasCephFS is true", func() {
			conn := ECCephClusterConnection(ec, "fsid", "key", []string{"10.0.0.1:6789"}, true)
			_, found, _ := unstructuredNestedMap(conn, "spec", "cephFS")
			Expect(found).To(BeTrue())
		})

		It("omits cephFS stanza when hasCephFS is false", func() {
			conn := ECCephClusterConnection(ec, "fsid", "key", []string{"10.0.0.1:6789"}, false)
			_, found, _ := unstructuredNestedMap(conn, "spec", "cephFS")
			Expect(found).To(BeFalse())
		})
	})

	Describe("ECCephCluster", func() {
		It("threads pvcStorageRequest into volumeClaimTemplates[0].spec.resources.requests.storage", func() {
			pvcReq := resource.MustParse("50Gi")
			cc := ECCephCluster(ec, "d8-sds-elastic", "registry.example.com/ceph:v19", int32(3), pvcReq, nil, 3, 2)

			sets, found, err := unstructuredNestedSlice(cc, "spec", "storage", "storageClassDeviceSets")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(sets).To(HaveLen(1))

			set0 := sets[0].(map[string]interface{})
			Expect(set0["count"]).To(Equal(int64(3)))

			templates := set0["volumeClaimTemplates"].([]interface{})
			Expect(templates).To(HaveLen(1))

			tpl0 := templates[0].(map[string]interface{})
			spec := tpl0["spec"].(map[string]interface{})
			res := spec["resources"].(map[string]interface{})
			req := res["requests"].(map[string]interface{})
			Expect(req["storage"]).To(Equal("50Gi"),
				"PVC request must equal the smallest local-PV's capacity (50Gi) so the K8s PV binder can satisfy PV.capacity >= PVC.requests.storage")
		})

		It("renders different pvcStorageRequest values verbatim (1Gi vs 2Ti)", func() {
			small := ECCephCluster(ec, "ns", "img", int32(1), resource.MustParse("1Gi"), nil, 3, 2)
			big := ECCephCluster(ec, "ns", "img", int32(1), resource.MustParse("2Ti"), nil, 3, 2)

			storageOf := func(o *unstructured.Unstructured) string {
				sets, _, _ := unstructuredNestedSlice(o, "spec", "storage", "storageClassDeviceSets")
				tpl0 := sets[0].(map[string]interface{})["volumeClaimTemplates"].([]interface{})[0].(map[string]interface{})
				spec := tpl0["spec"].(map[string]interface{})
				return spec["resources"].(map[string]interface{})["requests"].(map[string]interface{})["storage"].(string)
			}
			Expect(storageOf(small)).To(Equal("1Gi"))
			Expect(storageOf(big)).To(Equal("2Ti"))
		})

		It("threads monCount and mgrCount into spec.mon.count and spec.mgr.count", func() {
			cc := ECCephCluster(ec, "ns", "img", int32(1), resource.MustParse("1Gi"), nil, 5, 3)

			monCount, found, err := unstructured.NestedInt64(cc.Object, "spec", "mon", "count")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(monCount).To(Equal(int64(5)))

			mgrCount, found, err := unstructured.NestedInt64(cc.Object, "spec", "mgr", "count")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(mgrCount).To(Equal(int64(3)))
		})

		It("supports the standard (3, 2) topology unchanged", func() {
			cc := ECCephCluster(ec, "ns", "img", int32(1), resource.MustParse("1Gi"), nil, 3, 2)

			monCount, _, _ := unstructured.NestedInt64(cc.Object, "spec", "mon", "count")
			Expect(monCount).To(Equal(int64(3)))
			mgrCount, _, _ := unstructured.NestedInt64(cc.Object, "spec", "mgr", "count")
			Expect(mgrCount).To(Equal(int64(2)))
		})
	})
})

// unstructuredNested* helpers avoid importing controller package in builder tests.
func unstructuredNestedInt64(obj *unstructured.Unstructured, fields ...string) (int64, bool, error) {
	v, found, err := unstructured.NestedInt64(obj.Object, fields...)
	return v, found, err
}

func unstructuredNestedString(obj *unstructured.Unstructured, fields ...string) (string, bool, error) {
	return unstructured.NestedString(obj.Object, fields...)
}

func unstructuredNestedSlice(obj *unstructured.Unstructured, fields ...string) ([]interface{}, bool, error) {
	return unstructured.NestedSlice(obj.Object, fields...)
}

func unstructuredNestedMap(obj *unstructured.Unstructured, fields ...string) (map[string]interface{}, bool, error) {
	return unstructured.NestedMap(obj.Object, fields...)
}
