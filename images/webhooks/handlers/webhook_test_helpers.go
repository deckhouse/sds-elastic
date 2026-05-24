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
	"encoding/json"

	"github.com/slok/kubewebhook/v2/pkg/model"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newESCUnstructured(name string, spec map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("storage.deckhouse.io/v1alpha1")
	obj.SetKind("ElasticStorageClass")
	obj.SetName(name)
	obj.Object["spec"] = spec
	return obj
}

func newECCUnstructured(spec map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("storage.deckhouse.io/v1alpha1")
	obj.SetKind("ElasticClusterCredential")
	obj.SetName("demo")
	obj.Object["spec"] = spec
	return obj
}

func admissionReview(op model.AdmissionReviewOp, oldObj *unstructured.Unstructured) *model.AdmissionReview {
	ar := &model.AdmissionReview{Operation: op}
	if oldObj != nil {
		raw, _ := json.Marshal(oldObj.Object)
		ar.OldObjectRaw = raw
	}
	return ar
}

func escSpec(clusterRef, scType, replication string) map[string]interface{} {
	spec := map[string]interface{}{
		"clusterRef": clusterRef,
		"type":       scType,
	}
	if replication != "" {
		spec["replication"] = replication
	}
	return spec
}

func eccSpec(fsid, monSecret, adminSecret string) map[string]interface{} {
	return map[string]interface{}{
		"fsid":        fsid,
		"monSecret":   monSecret,
		"adminSecret": adminSecret,
	}
}
