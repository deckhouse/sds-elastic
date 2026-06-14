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

// deleteSpecs registers the data-safety deletion-guard specs (destructive,
// hence ordered after creation and the disable-blocked check). Implementations
// land in commit C6.
func deleteSpecs() {
	PIt("blocks ElasticStorageClass deletion while a bound PV exists, and force-deletion does not bypass the bound-PV guard", func() {})
	PIt("purges a non-empty RBD pool only after force-deletion (DataPresentInPool guard)", func() {})
	PIt("blocks CephFS ElasticStorageClass deletion while the filesystem is non-empty (FilesystemNotEmpty guard)", func() {})
	PIt("blocks ElasticCluster deletion while StorageClasses exist, keeps volumes live, then tears down in order with credential/disks preserved", func() {})
}

// finalManualCleanupSpec registers the documented post-uninstall manual cleanup
// spec. It is the very last spec (runs after the module is force-disabled), so
// it must keep BD labels/disks intact until then. Implementation lands in C6.
func finalManualCleanupSpec() {
	PIt("performs the documented manual cleanup of leftover PV/LLV/LVG and strips OSD BlockDevice labels", func() {})
}
