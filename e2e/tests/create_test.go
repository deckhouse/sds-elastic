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
	. "github.com/onsi/ginkgo/v2"
)

// createSpecs registers the happy-path creation + data round-trip specs on the
// shared ElasticCluster. The concrete implementations land in commit C5; the
// pending specs below pin the intended coverage and ordering.
func createSpecs() {
	PIt("creates the shared ElasticCluster and waits for Ready (staged conditions, credential, LVG/LLV/PV, Rook CephCluster Ready)", func() {})
	PIt("declares an RBD ElasticStorageClass (ConsistencyAndAvailability) and materialises the CephStorageClass + core StorageClass", func() {})
	PIt("declares a CephFS ElasticStorageClass (ErasureCodedCompact) and reaches Ready", func() {})
	PIt("declares a HighRedundancy ElasticStorageClass and promotes the Ceph topology to mon=5/mgr=3", func() {})
	PIt("round-trips data through an RBD PVC+Pod", func() {})
	PIt("round-trips data through a CephFS PVC+Pod", func() {})
	PIt("verifies the vendored Rook is fully renamed (no ceph.rook.io leak)", func() {})
}
