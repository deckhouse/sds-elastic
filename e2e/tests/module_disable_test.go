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

// moduleDisableBlockedSpec registers the non-destructive disable-guard spec. It
// is built BEFORE deleteSpecs so it runs while the shared ElasticCluster is
// still alive. Implementation lands in commit C7 (requires the C3 webhook in
// the module image under test).
func moduleDisableBlockedSpec() {
	PIt("denies disabling the sds-elastic module without the force annotation while an ElasticCluster exists", func() {})
}

// moduleDisableForceSpec registers the terminal force-disable spec. It runs
// after deleteSpecs (the shared EC is gone), arms the guard with a bare EC, and
// then force-disables the module. Implementation lands in commit C7.
func moduleDisableForceSpec() {
	PIt("force-disables the module via the force annotation and uninstalls it (resources/VWC/namespace removed)", func() {})
}
