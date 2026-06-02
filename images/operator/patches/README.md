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
lexical order via `git am` right after the clone step. `git am` preserves
the original authorship/commit message and fails loudly on a partial
apply, which is what we want in a reproducible build.

## Refreshing a patch after a Rook bump

When bumping the vendored Rook version, regenerate the patches against the
new tag so they keep applying cleanly:

```sh
git clone --branch v<new-rook-version> --depth 1 <source>/rook/rook.git
cd rook
git am --3way --keep-non-patch /path/to/images/operator/patches/*.patch
# resolve any conflicts, then `git am --continue`
git format-patch -<N> -o /path/to/images/operator/patches/   # N = number of patches
```

If `git am` cannot apply a hunk even with `--3way`, abort and fall back to
a manual re-apply:

```sh
git am --abort
git apply --3way /path/to/images/operator/patches/<file>.patch
# fix conflicts, re-commit, then `git format-patch` as above
```
