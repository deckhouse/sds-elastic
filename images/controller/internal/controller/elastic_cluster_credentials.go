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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// ensureCredentials reads the Rook secret/CM in the module namespace and
// populates the corresponding EC.status fields:
//
//   - cephFSID       -> Secret/rook-ceph-mon.data.fsid.
//   - monEndpoints   -> ConfigMap/rook-ceph-mon-endpoints.data.data, parsed
//     from Rook's "<id>=<host>:<port>,..." format.
//   - MonMaxID       -> ConfigMap/rook-ceph-mon-endpoints.data.maxMonId.
//   - credentialsRef -> name=ec.Name (1:1 mapping, independent of secret/CM).
//
// All four fields are written to the status builder only after every Get
// + parse succeeds, so an in-progress reconcile never publishes a
// half-populated status (e.g. credentialsRef pointing at an ECC that does
// not exist yet).
//
// The CredentialsReady stage owns the population of these fields in the MVP.
// The ElasticClusterCredential reconciler (added in a follow-up commit)
// independently maintains the ECC.spec backup; both writers converge to the
// same source of truth so the duplication is safe.
func (r *ElasticClusterReconciler) ensureCredentials(ctx context.Context, ec *v1alpha1.ElasticCluster, status *ecStatusBuilder) (bool, string, error) {
	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      external.RookCephMonSecretName,
	}, secret)
	if apierrors.IsNotFound(err) {
		return false, "waiting for cluster credentials", nil
	}
	if err != nil {
		r.Log.Error(err, "[ensureCredentials] failed to get rook-ceph-mon secret")
		return false, "", errReadCredentials
	}

	cm := &corev1.ConfigMap{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      external.RookCephMonEndpointsConfigMap,
	}, cm)
	if apierrors.IsNotFound(err) {
		return false, "waiting for cluster connection details", nil
	}
	if err != nil {
		r.Log.Error(err, "[ensureCredentials] failed to get rook-ceph-mon-endpoints configmap")
		return false, "", errReadCredentials
	}

	fsid := strings.TrimSpace(string(secret.Data[external.RookCephMonSecretFSIDKey]))
	if fsid == "" {
		return false, "cluster credentials not yet populated", nil
	}
	monEndpoints := parseMonEndpoints(cm.Data[external.RookCephMonEndpointsDataKey])
	if len(monEndpoints) == 0 {
		return false, "cluster connection details not yet available", nil
	}

	status.cephFSID = fsid
	status.monEndpoints = monEndpoints
	status.monMaxID = strings.TrimSpace(cm.Data[external.RookCephMonEndpointsMaxMonIDKey])
	status.credentialsRef = &v1alpha1.ElasticClusterCredentialRef{Name: ec.Name}

	return true, fmt.Sprintf("captured cluster credentials (%d endpoints)", len(monEndpoints)), nil
}
