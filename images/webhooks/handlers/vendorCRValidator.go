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
	"context"
	"fmt"
	"slices"

	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
)

const (
	moduleNamespace = "d8-sds-elastic"

	allowedControllerUser = "system:serviceaccount:d8-sds-elastic:controller"
	allowedOperatorUser   = "system:serviceaccount:d8-sds-elastic:rook-ceph-system"
	// allowedObjectUser is the sds-object controller, which manages
	// CephObjectStore / CephObjectStoreUser for its Heavy profile (S3 / RGW)
	// on top of an sds-elastic cluster.
	allowedObjectUser = "system:serviceaccount:d8-sds-object:controller"

	vendorCRDenyMessage = "Direct modifications to Rook Ceph resources are not allowed. " +
		"Please use ElasticCluster / ElasticStorageClass (storage.deckhouse.io/v1alpha1) to manage the cluster."
)

// vendorAPIGroups lists the API groups whose CRs are managed exclusively by
// the sds-elastic controller and the vendored Rook operator. The Rook group
// is internal.sdselastic.deckhouse.io (renamed from upstream ceph.rook.io
// by the vendored operator build, see images/operator/patches/) so that
// sds-elastic does not collide with a user-installed upstream Rook on the
// same cluster.
//
// objectbucket.io is intentionally absent: this release ships with the Rook
// OBC controller disabled (ROOK_DISABLE_OBJECT_BUCKET_CLAIM=true) and does
// not vendor the objectbucket.io CRDs, so there is nothing to guard.
var vendorAPIGroups = []string{"internal.sdselastic.deckhouse.io"}

var allowedUsers = []string{allowedControllerUser, allowedOperatorUser, allowedObjectUser}

func VendorCRValidate(_ context.Context, arReview *model.AdmissionReview, obj metav1.Object) (*kwhvalidating.ValidatorResult, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return &kwhvalidating.ValidatorResult{}, nil
	}

	gv := u.GroupVersionKind()
	if !slices.Contains(vendorAPIGroups, gv.Group) {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	ns := u.GetNamespace()
	if ns == "" {
		ns = arReview.Namespace
	}
	if ns != "" && ns != moduleNamespace {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	username := arReview.UserInfo.Username
	if slices.Contains(allowedUsers, username) {
		klog.Infof("User %s is allowed to manage vendor CR %s/%s", username, gv.Kind, u.GetName())
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	klog.Infof("User %s is not allowed to manage vendor CR %s/%s", username, gv.Kind, u.GetName())
	return &kwhvalidating.ValidatorResult{
		Valid:   false,
		Message: fmt.Sprintf("%s (requested by %s)", vendorCRDenyMessage, username),
	}, nil
}
