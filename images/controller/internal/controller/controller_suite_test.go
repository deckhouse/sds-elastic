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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/config"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/logger"
)

func TestController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

func newFakeClient(initObjs ...client.Object) client.Client {
	sch := apiruntime.NewScheme()
	utilruntime.Must(v1alpha1.AddToScheme(sch))
	utilruntime.Must(clientgoscheme.AddToScheme(sch))
	for _, gvk := range []schema.GroupVersionKind{
		external.BlockDeviceGVK,
		external.LVMVolumeGroupGVK,
		external.LVMLogicalVolumeGVK,
		external.CephClusterGVK,
		external.CephBlockPoolGVK,
		external.CephFilesystemGVK,
		external.CephClusterConnectionGVK,
		external.CephStorageClassGVK,
	} {
		registerUnstructuredGVK(sch, gvk)
	}

	return fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(
			&v1alpha1.ElasticCluster{},
			&v1alpha1.ElasticStorageClass{},
			&v1alpha1.ElasticClusterCredential{},
		).
		WithObjects(initObjs...).
		Build()
}

func newTestLogger() *logger.Logger {
	l, err := logger.NewLogger(logger.ErrorLevel)
	Expect(err).NotTo(HaveOccurred())
	return l
}

func newTestCfg() *config.Options {
	return &config.Options{
		ControllerNamespace:     "d8-sds-elastic",
		CephImages:              map[string]string{v1alpha1.DefaultCephVersion: "registry.example.com/ceph:v19.2.3"},
		MaxConcurrentReconciles: 1,
		RequeueInterval:         time.Second,
	}
}

func newElasticClusterReconciler(cl client.Client) *ElasticClusterReconciler {
	return &ElasticClusterReconciler{
		Client: cl,
		Log:    newTestLogger(),
		Cfg:    newTestCfg(),
	}
}

func newElasticClusterCredentialReconciler(cl client.Client) *ElasticClusterCredentialReconciler {
	return &ElasticClusterCredentialReconciler{
		Client: cl,
		Log:    newTestLogger(),
		Cfg:    newTestCfg(),
	}
}

func newElasticStorageClassReconciler(cl client.Client) *ElasticStorageClassReconciler {
	return &ElasticStorageClassReconciler{
		Client: cl,
		Log:    newTestLogger(),
		Cfg:    newTestCfg(),
	}
}

func registerUnstructuredGVK(scheme *apiruntime.Scheme, gvk schema.GroupVersionKind) {
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	listGVK := schema.GroupVersionKind{
		Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List",
	}
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
