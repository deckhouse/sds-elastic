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
)

// AllowedProvisioners lists provisioners for which the module-delete hook
// should strip finalizers from StorageClass objects. sds-elastic itself
// does not own RBD/CephFS storage classes (csi-ceph does), so the list is
// empty by default; the manual OSD StorageClass uses no-provisioner.
var AllowedProvisioners = []string{}

var WebhookConfigurationsToDelete = []string{}

// CRGVKsForFinalizerRemoval lists CRs the module creates and which carry
// our finalizer (the controller adds `storage.deckhouse.io/sds-elastic-cluster`
// on the SdsElasticCluster CR).
var CRGVKsForFinalizerRemoval = []CRGVK{
	{Group: "storage.deckhouse.io", Version: "v1alpha1", Kind: "SdsElasticCluster", Namespaced: false},
}

type CRGVK struct {
	Group      string
	Version    string
	Kind       string
	Namespaced bool
}
