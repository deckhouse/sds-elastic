---
title: "sds-elastic module"
description: "sds-elastic module: distributed block storage on top of the Rook Ceph operator."
weight: 1
---

{{< alert level="warning" >}}
The module is in `Experimental` stage. The API, configuration and Custom
Resources may change without notice; do not rely on it for production
workloads.
{{< /alert >}}

The `sds-elastic` module deploys and manages the
[Rook Ceph operator](https://rook.io) in a Deckhouse Kubernetes cluster,
turning a set of nodes into a distributed block storage backend backed by
Ceph.

The module is currently a thin wrapper around the upstream operator: it
ships the operator Deployment, the rook-ceph-operator-config ConfigMap
and the full set of Ceph CRDs. Cluster provisioning, monitoring,
documentation of Custom Resources and end-to-end tests are out of scope
for v0.0.x and will be added in subsequent releases.
