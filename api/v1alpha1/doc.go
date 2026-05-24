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

// Package v1alpha1 contains API Schema definitions for the storage.deckhouse.io
// resources managed by the sds-elastic module:
//
//   - ElasticCluster — bootstraps a Rook CephCluster from BlockDevices.
//   - ElasticStorageClass — declares a Ceph pool plus a csi-ceph StorageClass.
//   - ElasticClusterCredential — internal backup of cluster identity.
//
// +groupName=storage.deckhouse.io
// +k8s:deepcopy-gen=package
package v1alpha1
