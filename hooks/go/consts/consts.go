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

package consts

const (
	ModuleName       string = "sdsElastic"
	ModuleNamespace  string = "d8-sds-elastic"
	ModulePluralName string = "sds-elastic"
	// WebhookCertCn is the CN used for the self-signed TLS material that
	// the webhooks container consumes. It must match the Service name
	// (helm_lib_module_webhook_service defaults to "webhooks").
	WebhookCertCn string = "webhooks"
)

// AllowedProvisioners lists provisioners for which the module-delete hook
// should strip finalizers from StorageClass objects. sds-elastic itself
// does not own RBD/CephFS storage classes (csi-ceph does), so the list is
// empty by default; the manual OSD StorageClass uses no-provisioner.
var AllowedProvisioners = []string{}

var WebhookConfigurationsToDelete = []string{
	"d8-sds-elastic-vendor-cr-validation",
}

// CRGVKsForFinalizerRemoval lists CRs the module creates and which may
// carry a controller-managed finalizer. On module disable the controller
// and the Rook operator are already stopped, so stripping the finalizers
// here only unblocks API-server deletion — it does NOT trigger any Rook
// cleanup, so OSD disks and dataDirHostPath are preserved (the namespace
// is no longer wedged in Terminating).
//
// The Rook-vendored CRs (CephCluster/CephBlockPool/CephFilesystem) live
// in the module-internal group and are namespaced into ModuleNamespace.
var CRGVKsForFinalizerRemoval = []CRGVK{
	{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "ElasticCluster", Namespaced: false},
	{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "ElasticStorageClass", Namespaced: false},
	{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "ElasticClusterCredential", Namespaced: false},
	{Group: "internal.sdselastic.deckhouse.io", Version: "v1", Kind: "CephCluster", Namespaced: true},
	{Group: "internal.sdselastic.deckhouse.io", Version: "v1", Kind: "CephBlockPool", Namespaced: true},
	{Group: "internal.sdselastic.deckhouse.io", Version: "v1", Kind: "CephFilesystem", Namespaced: true},
}

type CRGVK struct {
	Group      string
	Version    string
	Kind       string
	Namespaced bool
}
