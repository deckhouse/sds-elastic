# Patches

## 001-Rename-CRD-API-group-to-internal-sdselastic-deckhouse-io.patch

Renames the Rook CRD API group from `ceph.rook.io` to
`internal.sdselastic.deckhouse.io`, so that the sds-elastic-managed Rook
operator can coexist with a user-installed upstream Rook on the same
Kubernetes cluster (no discovery / RBAC / etcd-key overlap).

Touches three production-code sites only:

- `pkg/apis/ceph.rook.io/register.go` — `CustomResourceGroupName`. All
  Rook clientsets / informers / listers, finalizer names and
  `SchemeGroupVersion` derive from this constant.
- `pkg/apis/ceph.rook.io/v1/register.go` — `CustomResourceGroup`
  (mirrors the above).
- `pkg/operator/ceph/csi/secrets.go` — `OwnerReference.APIVersion` on
  CSI Secrets owned by `CephCluster`. The Kubernetes garbage collector
  resolves owner GVK strictly through the discovery RESTMapper, so this
  literal must follow the renamed group; otherwise CSI Secrets leak on
  `CephCluster` deletion.

Generated against upstream Rook `v1.19.5` with `git format-patch`; the
build (`images/operator/werf.inc.yaml`) applies `*.patch` files in
lexical order via `git apply` right after the clone step.
