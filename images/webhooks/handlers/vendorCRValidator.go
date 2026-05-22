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

	vendorCRDenyMessage = "Direct modifications to Rook Ceph resources are not allowed. " +
		"Please use SdsElasticCluster (storage.deckhouse.io/v1alpha1) to manage the cluster."
)

var vendorAPIGroups = []string{"ceph.rook.io", "objectbucket.io"}

var allowedUsers = []string{allowedControllerUser, allowedOperatorUser}

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
