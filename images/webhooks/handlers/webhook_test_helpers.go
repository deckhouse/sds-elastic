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

// newMCUnstructured builds a Deckhouse ModuleConfig fixture for the
// mc-validation tests. `enabled` is omitted from spec when nil (mirrors a
// ModuleConfig that only carries settings), set otherwise. annotations is
// applied verbatim so tests can exercise the force-disable escape hatch.
func newMCUnstructured(name string, enabled *bool, annotations map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("deckhouse.io/v1alpha1")
	obj.SetKind("ModuleConfig")
	obj.SetName(name)
	if len(annotations) > 0 {
		obj.SetAnnotations(annotations)
	}
	spec := map[string]interface{}{}
	if enabled != nil {
		spec["enabled"] = *enabled
	}
	obj.Object["spec"] = spec
	return obj
}

// newECObject builds a minimal ElasticCluster runtime.Object suitable for
// seeding the fake dynamic client in mc-validation tests. The validator
// only counts/names ElasticClusters, so spec is intentionally empty.
func newECObject(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("storage.deckhouse.io/v1alpha1")
	obj.SetKind("ElasticCluster")
	obj.SetName(name)
	obj.Object["spec"] = map[string]interface{}{}
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

// newECStubUnstructured builds a minimal ElasticCluster fixture suitable
// for the ESC HighRedundancy preflight: the validator only reads
// metadata.name and spec.storage.nodeSelector, so spec.network and the
// blockDeviceSelector are intentionally omitted.
//
// The empty {} default for nodeSelector mirrors the CRD shape the
// validator gets from the API server: `unstructured.NestedMap` returns
// (empty, true, nil) for a present-but-empty selector, which
// labelSelectorFromMap turns into labels.Everything (matches every
// Node) — the same semantics the controller uses.
func newECStubUnstructured(name string, nodeSel map[string]interface{}) *unstructured.Unstructured {
	storage := map[string]interface{}{
		"blockDeviceSelector": map[string]interface{}{},
	}
	if nodeSel != nil {
		storage["nodeSelector"] = nodeSel
	} else {
		storage["nodeSelector"] = map[string]interface{}{}
	}
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("storage.deckhouse.io/v1alpha1")
	obj.SetKind("ElasticCluster")
	obj.SetName(name)
	obj.Object["spec"] = map[string]interface{}{"storage": storage}
	return obj
}
