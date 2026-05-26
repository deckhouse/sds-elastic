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
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// ensureCsiCeph applies the csi-ceph CephClusterConnection CR for this
// ElasticCluster. Preconditions (returns done=false if not satisfied):
//
//  1. EC.status.{cephFSID, monEndpoints, credentialsRef} populated.
//  2. ElasticClusterCredential resource exists with adminSecret set.
//
// userKey is taken from ECC.spec.adminSecret rather than directly from
// Secret/rook-ceph-mon: ECC is the cross-namespace backup of cluster
// identity and the canonical source of truth for csi-ceph credentials.
func (r *ElasticClusterReconciler) ensureCsiCeph(ctx context.Context, ec *v1alpha1.ElasticCluster, status *ecStatusBuilder) (bool, string, error) {
	if status.cephFSID == "" || len(status.monEndpoints) == 0 || status.credentialsRef == nil {
		return false, "waiting for credentials in EC.status", nil
	}

	ecc := &v1alpha1.ElasticClusterCredential{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: status.credentialsRef.Name}, ecc)
	if apierrors.IsNotFound(err) {
		return false, "waiting for ElasticClusterCredential", nil
	}
	if err != nil {
		return false, "", err
	}
	adminSecret := strings.TrimSpace(ecc.Spec.AdminSecret)
	if adminSecret == "" {
		return false, "waiting for ECC.spec.adminSecret", nil
	}

	hasCephFS, err := r.elasticClusterHasCephFS(ctx, ec)
	if err != nil {
		return false, "", err
	}

	desired := builder.ECCephClusterConnection(ec, status.cephFSID, adminSecret, status.monEndpoints, hasCephFS)
	if err := r.upsertECUnstructured(ctx, desired); err != nil {
		if isNoMatchErr(err) {
			return false, "waiting for CephClusterConnection CRD (csi-ceph)", nil
		}
		return false, "", fmt.Errorf("upsert CephClusterConnection: %w", err)
	}

	conn := &unstructured.Unstructured{}
	conn.SetGroupVersionKind(external.CephClusterConnectionGVK)
	err = r.Client.Get(ctx, types.NamespacedName{Name: desired.GetName()}, conn)
	if apierrors.IsNotFound(err) {
		return false, "CephClusterConnection not yet visible", nil
	}
	if err != nil {
		return false, "", err
	}
	phase, _, _ := unstructured.NestedString(conn.Object, "status", "phase")
	if phase != "Created" {
		return false, fmt.Sprintf("CephClusterConnection phase=%q", phase), nil
	}
	return true, "CephClusterConnection ready", nil
}

// elasticClusterHasCephFS returns true if any ElasticStorageClass referencing
// this EC has type=CephFS. The result drives the cephFS.subvolumeGroup
// section of CephClusterConnection.
func (r *ElasticClusterReconciler) elasticClusterHasCephFS(ctx context.Context, ec *v1alpha1.ElasticCluster) (bool, error) {
	list := &v1alpha1.ElasticStorageClassList{}
	if err := r.Client.List(ctx, list); err != nil {
		return false, err
	}
	for _, esc := range list.Items {
		if esc.Spec.ClusterRef == ec.Name && esc.Spec.Type == v1alpha1.StorageClassTypeCephFS {
			return true, nil
		}
	}
	return false, nil
}
