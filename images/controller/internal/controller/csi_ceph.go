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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/builder"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// readRookMon returns (monitors, userID, fsid, userKey, found). It reads
// the rook-ceph-mon Secret (data.fsid, ceph-username, ceph-secret) and the
// rook-ceph-mon-endpoints ConfigMap (data.data, format
// "a=ip1:6789,b=ip2:6789,...") from the controller namespace.
// found=false means one of the sources is missing yet; caller should requeue.
func (r *SdsElasticClusterReconciler) readRookMon(ctx context.Context) (monitors []string, userID, fsid, userKey string, found bool) {
	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      external.RookCephMonSecretName,
	}, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Log.Error(err, "[readRookMon] unable to get rook-ceph-mon secret")
		}
		return nil, "", "", "", false
	}

	userID = strings.TrimPrefix(string(secret.Data[external.RookCephMonSecretUsernameKey]), "client.")
	userKey = string(secret.Data[external.RookCephMonSecretKeyKey])
	fsid = string(secret.Data[external.RookCephMonSecretFSIDKey])

	cm := &corev1.ConfigMap{}
	if err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      external.RookCephMonEndpointsConfigMap,
	}, cm); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Log.Error(err, "[readRookMon] unable to get rook-ceph-mon-endpoints configmap")
		}
		return nil, userID, fsid, userKey, false
	}

	monitors = parseMonEndpoints(cm.Data[external.RookCephMonEndpointsDataKey])
	if len(monitors) == 0 || userKey == "" || fsid == "" {
		return monitors, userID, fsid, userKey, false
	}
	return monitors, userID, fsid, userKey, true
}

// parseMonEndpoints turns "a=10.0.0.1:6789,b=10.0.0.2:6789" into
// ["10.0.0.1:6789", "10.0.0.2:6789"], sorted for stable output.
func parseMonEndpoints(data string) []string {
	if data == "" {
		return nil
	}
	parts := strings.Split(data, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if i := strings.Index(p, "="); i >= 0 {
			p = p[i+1:]
		}
		if p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// ensureCsiCephIntegration creates/updates the CephClusterConnection and
// CephStorageClass resources. The function is a no-op when the integration
// is disabled.
//
// fsidHint is the FSID already observed by ensureCephCluster; if empty we
// also try the rook-ceph-mon secret as a fallback.
func (r *SdsElasticClusterReconciler) ensureCsiCephIntegration(ctx context.Context, cluster *v1alpha1.SdsElasticCluster, fsidHint string) (bool, string, error) {
	if cluster.Spec.CsiCephIntegration == nil || !cluster.Spec.CsiCephIntegration.Enabled {
		return true, "csi-ceph integration disabled", nil
	}

	// csi-ceph is an optional dependency. If its module is not installed,
	// the CephClusterConnection / CephStorageClass CRDs are missing and
	// upsertUnstructured would crash the reconcile with NoKindMatchError.
	// Report a clear in-progress condition and keep polling — the user
	// may install csi-ceph after the SdsElasticCluster CR is created.
	for _, gvk := range []schema.GroupVersionKind{
		external.CephClusterConnectionGVK,
		external.CephStorageClassGVK,
	} {
		installed, err := r.crdRegistered(ctx, gvk)
		if err != nil {
			return false, "", fmt.Errorf("check CRD %s: %w", gvk.Kind, err)
		}
		if !installed {
			return false, fmt.Sprintf("waiting for csi-ceph module: CRD %s is not installed", gvk.Kind), nil
		}
	}

	monitors, userID, fsidFromMon, userKey, ok := r.readRookMon(ctx)
	if !ok {
		return false, "waiting for rook-ceph-mon Secret and rook-ceph-mon-endpoints ConfigMap", nil
	}

	clusterID := fsidHint
	if clusterID == "" {
		clusterID = fsidFromMon
	}
	if clusterID == "" {
		return false, "waiting for Ceph cluster FSID", nil
	}

	if cluster.Spec.CsiCephIntegration.UserID != "" {
		userID = cluster.Spec.CsiCephIntegration.UserID
	}
	if userID == "" {
		userID = "admin"
	}

	connectionName := cluster.Spec.CsiCephIntegration.ConnectionName
	if connectionName == "" {
		connectionName = builder.DefaultConnectionName
	}

	hasCephFS := len(cluster.Spec.Filesystems) > 0
	ccc := builder.CephClusterConnection(cluster, connectionName, clusterID, userID, userKey, monitors, hasCephFS)
	if err := r.upsertUnstructured(ctx, ccc); err != nil {
		return false, "", fmt.Errorf("upsert CephClusterConnection %s: %w", connectionName, err)
	}

	desiredSCNames := map[string]struct{}{}
	for i := range cluster.Spec.BlockPools {
		obj := builder.CephStorageClassRBD(cluster, connectionName, cluster.Spec.BlockPools[i].Name)
		if err := r.upsertUnstructured(ctx, obj); err != nil {
			return false, "", fmt.Errorf("upsert CephStorageClass %s: %w", obj.GetName(), err)
		}
		desiredSCNames[obj.GetName()] = struct{}{}
	}
	for i := range cluster.Spec.Filesystems {
		obj := builder.CephStorageClassCephFS(cluster, connectionName, cluster.Spec.Filesystems[i].Name)
		if err := r.upsertUnstructured(ctx, obj); err != nil {
			return false, "", fmt.Errorf("upsert CephStorageClass %s: %w", obj.GetName(), err)
		}
		desiredSCNames[obj.GetName()] = struct{}{}
	}

	if err := r.pruneOwnedByGVK(ctx, external.CephStorageClassGVK, cluster.Name, "", desiredSCNames); err != nil {
		return false, "", err
	}
	return true, "", nil
}
