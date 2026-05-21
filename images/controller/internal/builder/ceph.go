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
	"fmt"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

// CephImage returns the module registry image reference for the requested
// Ceph version (spec.cephVersion). Images are injected via the CEPH_IMAGES
// environment variable from Helm (helm_lib_module_image per ceph-v* variant).
func CephImage(images map[string]string, cephVersion string) (string, error) {
	version := defaultIfEmpty(cephVersion, v1alpha1.DefaultCephVersion)
	image, ok := images[version]
	if !ok {
		return "", fmt.Errorf("module ceph image not configured for version %q", version)
	}
	return image, nil
}
