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

package builder

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
)

// MVPUserID is the Ceph user written to CephClusterConnection.spec.userID.
// We pin it to the cluster admin for the MVP because per-pool client.* users
// require csi-ceph CephClient hardening (B5 follow-up).
const MVPUserID = "admin"

// CephFSSubvolumeGroup is the CephFS subvolume group csi-ceph provisions
// CephFS volumes under. It matches Rook's default ("csi") and is pinned for
// the MVP.
const CephFSSubvolumeGroup = "csi"

// ECCephClusterConnectionName returns the CephClusterConnection name. It is
// 1:1 with the ElasticCluster's metadata.name so multiple clusters (future)
// produce distinct CRs.
func ECCephClusterConnectionName(ec *v1alpha1.ElasticCluster) string {
	return ec.Name
}

// ECCephClusterConnection builds an unstructured CephClusterConnection CR
// for csi-ceph. Inputs:
//   - clusterID — EC.status.cephFSID, copied verbatim from rook-ceph-mon Secret.
//   - monitors  — EC.status.monEndpoints, parsed from rook-ceph-mon-endpoints CM.
//   - userKey   — ECC.spec.adminSecret, copied from rook-ceph-mon Secret.
//
// All three inputs come from EC.status / ECC.spec, populated by the
// CredentialsReady stage of the controller. Calling this builder before
// CredentialsReady would yield an invalid CephClusterConnection (empty
// userKey), which is why the controller gates CsiCephReady on those fields.
//
// spec.cephFS is ALWAYS emitted, regardless of whether a CephFS ESC currently
// references the cluster. csi-ceph marks spec.cephFS immutable (it can neither
// be added, removed, nor changed after creation), so gating it on the current
// ESC inventory would wedge the connection the moment the first CephFS ESC is
// added to an EC that started RBD-only: the controller would try to add the
// field to an existing connection and every update would be rejected, freezing
// CsiCephReady (and the mon-endpoint refresh that rides the same update).
// Emitting it unconditionally keeps presence constant for the connection's
// whole life; csi-ceph treats it as harmless for RBD-only clusters (it only
// pins the CephFS subvolume group used once a CephFS volume is provisioned).
func ECCephClusterConnection(
	ec *v1alpha1.ElasticCluster,
	clusterID, userKey string,
	monitors []string,
) *unstructured.Unstructured {
	mons := make([]interface{}, 0, len(monitors))
	for _, m := range monitors {
		mons = append(mons, m)
	}
	spec := map[string]interface{}{
		"clusterID": clusterID,
		"monitors":  mons,
		"userID":    MVPUserID,
		"userKey":   userKey,
		"cephFS": map[string]interface{}{
			"subvolumeGroup": CephFSSubvolumeGroup,
		},
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(external.CephClusterConnectionGVK)
	obj.SetName(ECCephClusterConnectionName(ec))
	obj.SetLabels(ECManagedLabels(ec))
	obj.Object["spec"] = spec
	return obj
}
