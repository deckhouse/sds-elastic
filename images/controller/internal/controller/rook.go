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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

const registrySecretName = "sds-elastic-registrysecret"

// ensureCephCluster brings up the Rook CephCluster CR and the rook-ceph-tools
// Deployment. Returns (done, fsid, message, error).
//
// "done" is true when both objects exist and the cluster FSID has been
// observed (status.ceph.fsid or rook-ceph-mon secret data.fsid). Until then
// the reconciler keeps requeuing.
func (r *SdsElasticClusterReconciler) ensureCephCluster(ctx context.Context, cluster *v1alpha1.SdsElasticCluster) (bool, string, string, error) {
	desired := builder.CephCluster(cluster, r.Cfg.ControllerNamespace, r.Cfg.OSDStorageClassName)
	if err := r.upsertUnstructured(ctx, desired); err != nil {
		return false, "", "", fmt.Errorf("upsert CephCluster: %w", err)
	}

	// rook-ceph-tools mounts the `rook-ceph-mon-endpoints` ConfigMap and
	// reads creds from the `rook-ceph-mon` Secret. Both are created by the
	// Rook operator only after the CephCluster MONs reach quorum, so
	// deploying the tools pod earlier produces FailedMount until then.
	// Skip the upsert until the operator has published both.
	ready, err := r.rookMonReady(ctx)
	if err != nil {
		return false, "", "", err
	}
	if !ready {
		return false, "", "waiting for rook-ceph-mon Secret and rook-ceph-mon-endpoints ConfigMap", nil
	}

	if err := r.upsertCephToolsDeployment(ctx, cluster); err != nil {
		return false, "", "", fmt.Errorf("upsert rook-ceph-tools Deployment: %w", err)
	}

	fsid, err := r.readCephFSID(ctx)
	if err != nil {
		return false, "", "", err
	}
	if fsid == "" {
		return false, "", "waiting for CephCluster fsid", nil
	}

	return true, fsid, "", nil
}

// rookMonReady returns true iff both the rook-ceph-mon Secret and the
// rook-ceph-mon-endpoints ConfigMap exist in the controller namespace.
// Either being absent means the Rook operator has not finished bringing
// up the MONs yet and we must not create the rook-ceph-tools Deployment.
func (r *SdsElasticClusterReconciler) rookMonReady(ctx context.Context) (bool, error) {
	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      external.RookCephMonSecretName,
	}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      external.RookCephMonEndpointsConfigMap,
	}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// readCephFSID returns the cluster FSID by checking, in order, the
// rook-ceph-mon secret (data.fsid is base64-encoded) and the CephCluster
// status.ceph.fsid. Either is acceptable: Rook populates them around the
// same time.
func (r *SdsElasticClusterReconciler) readCephFSID(ctx context.Context) (string, error) {
	mons, _, fsid, _, _ := r.readRookMon(ctx)
	_ = mons
	if fsid != "" {
		return fsid, nil
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephClusterGVK)
	if err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      builder.CephClusterName,
	}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	v, _, _ := unstructured.NestedString(obj.Object, "status", "ceph", "fsid")
	return v, nil
}

// upsertCephToolsDeployment creates or updates the rook-ceph-tools
// Deployment. Only the pod template is compared; immutable fields like
// .spec.selector are never touched.
func (r *SdsElasticClusterReconciler) upsertCephToolsDeployment(ctx context.Context, cluster *v1alpha1.SdsElasticCluster) error {
	desired := builder.CephToolsDeployment(cluster, r.Cfg.ControllerNamespace, registrySecretName)

	existing := &appsv1.Deployment{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: desired.Namespace,
		Name:      desired.Name,
	}, existing)
	if apierrors.IsNotFound(err) {
		return r.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	patched := false
	if !equality.Semantic.DeepEqual(existing.Spec.Template, desired.Spec.Template) {
		existing.Spec.Template = desired.Spec.Template
		patched = true
	}
	if !equality.Semantic.DeepEqual(existing.Spec.Replicas, desired.Spec.Replicas) {
		existing.Spec.Replicas = desired.Spec.Replicas
		patched = true
	}
	merged := mergeLabels(existing.Labels, desired.Labels)
	if !equality.Semantic.DeepEqual(existing.Labels, merged) {
		existing.Labels = merged
		patched = true
	}
	if !patched {
		return nil
	}
	return r.Client.Update(ctx, existing)
}
