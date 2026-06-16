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

## 002-Mount-resource-cleanup-config-dir-at-var-lib-rook.patch

Fixes the operator's resource-cleanup job (force-deletion of
`CephBlockPool`, `CephBlockPoolRadosNamespace` and
`CephFilesystemSubVolumeGroup`) for any non-default `dataDirHostPath`.

`jobContainer()` in `pkg/operator/ceph/controller/cleanup.go` mounted the
host `dataDirHostPath` at its literal path, but the cleanup commands run
`rbd`/`ceph`, which read the cluster config and admin keyring from
`context.ConfigDir` = `k8sutil.DataDir` (`/var/lib/rook`) — the same
in-container path used by the operator and every Ceph daemon. With the
sds-elastic `dataDirHostPath` (`/opt/deckhouse/sds/elastic/<cluster>`)
the files are mounted at the literal path while `rbd` still looks under
`/var/lib/rook`, so it aborts immediately with
`unable to open config file` and the cleanup job CrashLoopBackOffs —
blocking `CephBlockPool` (and the owning `ElasticStorageClass`)
force-deletion. The patch mounts the host data dir at `k8sutil.DataDir`,
aligning the resource-cleanup container with the rest of Rook.

The host-disk wipe job (`pkg/operator/ceph/cluster/cleanup.go`,
`ceph clean host`) keeps its literal `dataDirHostPath` mount on purpose —
it physically deletes that host path — and is built by a different
function, so it is untouched.

Also includes a diagnostics fix in `pkg/daemon/ceph/client/image.go`:
`ListImagesInRadosNamespace` now folds `buf` (the `rbd` stderr captured
by the exec wrapper) into the wrapped error, so failures surface the real
cause instead of an opaque `exit status 1`.

Touches `pkg/operator/ceph/controller/cleanup.go` and
`pkg/daemon/ceph/client/image.go` only — disjoint from `001`, so apply
order is irrelevant. Generated against upstream Rook `v1.19.5`.

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
