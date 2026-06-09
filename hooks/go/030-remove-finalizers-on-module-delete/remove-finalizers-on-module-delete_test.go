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

package hooks_common

import (
	"context"
	"testing"

	"github.com/deckhouse/deckhouse/pkg/log"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/deckhouse/sds-elastic/hooks/go/consts"
)

// TestRemoveFinalizersFromObjectStripsRookCR proves the generic
// finalizer-removal helper clears a Rook-vendored CR (CephCluster) so the
// API server can finish deleting it. On module disable the Rook operator
// is already stopped, so this patch never triggers Rook cleanup.
func TestRemoveFinalizersFromObjectStripsRookCR(t *testing.T) {
	gvk := schema.GroupVersionKind{
		Group:   "internal.sdselastic.deckhouse.io",
		Version: "v1",
		Kind:    "CephCluster",
	}

	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List"},
		&unstructured.UnstructuredList{},
	)

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(gvk)
	cr.SetNamespace(consts.ModuleNamespace)
	cr.SetName("ceph-prod")
	cr.SetFinalizers([]string{"ceph.rook.io/disaster-protection"})

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()

	if err := removeFinalizersFromObject(context.Background(), cl, cr, log.NewNop()); err != nil {
		t.Fatalf("removeFinalizersFromObject returned error: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(gvk)
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: consts.ModuleNamespace, Name: "ceph-prod"}, got); err != nil {
		t.Fatalf("get CephCluster: %v", err)
	}
	if len(got.GetFinalizers()) != 0 {
		t.Fatalf("expected no finalizers, got %v", got.GetFinalizers())
	}
}

// rookGVK is a convenience for the namespaced Rook-vendored kinds.
func rookGVK(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "internal.sdselastic.deckhouse.io", Version: "v1", Kind: kind}
}

func rookCR(kind, namespace, name string, finalizers ...string) *unstructured.Unstructured {
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(rookGVK(kind))
	cr.SetNamespace(namespace)
	cr.SetName(name)
	if len(finalizers) > 0 {
		cr.SetFinalizers(finalizers)
	}
	return cr
}

// TestRemoveCRFinalizersStripsNamespacedCRs exercises the handler glue: the
// per-GVK list-by-kind, the ModuleNamespace scoping, the empty-finalizer
// skip, and that CRs outside ModuleNamespace are left untouched.
func TestRemoveCRFinalizersStripsNamespacedCRs(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, kind := range []string{"CephCluster", "CephBlockPool", "CephFilesystem"} {
		scheme.AddKnownTypeWithName(rookGVK(kind), &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(rookGVK(kind+"List"), &unstructured.UnstructuredList{})
	}

	withFinalizer := rookCR("CephBlockPool", consts.ModuleNamespace, "pool-a", "cephblockpool.ceph.rook.io")
	noFinalizer := rookCR("CephFilesystem", consts.ModuleNamespace, "fs-a")
	foreignNS := rookCR("CephCluster", "other-ns", "ceph-foreign", "ceph.rook.io/disaster-protection")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(withFinalizer, noFinalizer, foreignNS).
		Build()

	gvks := []consts.CRGVK{
		{Group: "internal.sdselastic.deckhouse.io", Version: "v1", Kind: "CephCluster", Namespaced: true},
		{Group: "internal.sdselastic.deckhouse.io", Version: "v1", Kind: "CephBlockPool", Namespaced: true},
		{Group: "internal.sdselastic.deckhouse.io", Version: "v1", Kind: "CephFilesystem", Namespaced: true},
	}
	if err := removeCRFinalizers(context.Background(), cl, gvks, log.NewNop()); err != nil {
		t.Fatalf("removeCRFinalizers returned error: %v", err)
	}

	got := rookCR("CephBlockPool", "", "")
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: consts.ModuleNamespace, Name: "pool-a"}, got); err != nil {
		t.Fatalf("get CephBlockPool: %v", err)
	}
	if len(got.GetFinalizers()) != 0 {
		t.Errorf("in-namespace CR finalizers not stripped: %v", got.GetFinalizers())
	}

	// A CR in another namespace must NOT be touched (ModuleNamespace scope).
	foreign := rookCR("CephCluster", "", "")
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "other-ns", Name: "ceph-foreign"}, foreign); err != nil {
		t.Fatalf("get foreign CephCluster: %v", err)
	}
	if len(foreign.GetFinalizers()) == 0 {
		t.Errorf("CR outside ModuleNamespace should retain its finalizer")
	}
}

// TestRemoveCRFinalizersToleratesMissingCRD proves an absent CRD (no kind
// registered in the scheme) is skipped without surfacing an error, while a
// registered kind in the same batch is still processed.
func TestRemoveCRFinalizersToleratesMissingCRD(t *testing.T) {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(rookGVK("CephBlockPool"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(rookGVK("CephBlockPoolList"), &unstructured.UnstructuredList{})

	cr := rookCR("CephBlockPool", consts.ModuleNamespace, "pool-a", "cephblockpool.ceph.rook.io")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr).Build()

	gvks := []consts.CRGVK{
		// CephFilesystem is intentionally not registered → list fails as a
		// no-match and must be tolerated.
		{Group: "internal.sdselastic.deckhouse.io", Version: "v1", Kind: "CephFilesystem", Namespaced: true},
		{Group: "internal.sdselastic.deckhouse.io", Version: "v1", Kind: "CephBlockPool", Namespaced: true},
	}
	if err := removeCRFinalizers(context.Background(), cl, gvks, log.NewNop()); err != nil {
		t.Fatalf("removeCRFinalizers should tolerate a missing CRD, got: %v", err)
	}

	got := rookCR("CephBlockPool", "", "")
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: consts.ModuleNamespace, Name: "pool-a"}, got); err != nil {
		t.Fatalf("get CephBlockPool: %v", err)
	}
	if len(got.GetFinalizers()) != 0 {
		t.Errorf("registered CR should still be processed: %v", got.GetFinalizers())
	}
}

// TestCRGVKsForFinalizerRemovalIncludesRookCRs guards the consts list so a
// future edit cannot silently drop the Rook-vendored CRs whose finalizers
// would otherwise wedge the namespace in Terminating on module disable.
func TestCRGVKsForFinalizerRemovalIncludesRookCRs(t *testing.T) {
	want := map[string]bool{
		"CephCluster":    false,
		"CephBlockPool":  false,
		"CephFilesystem": false,
	}
	for _, g := range consts.CRGVKsForFinalizerRemoval {
		if _, ok := want[g.Kind]; !ok {
			continue
		}
		if g.Group != "internal.sdselastic.deckhouse.io" || g.Version != "v1" || !g.Namespaced {
			t.Errorf("Rook CR %s has unexpected GVK/scope: %+v", g.Kind, g)
		}
		want[g.Kind] = true
	}
	for kind, found := range want {
		if !found {
			t.Errorf("CRGVKsForFinalizerRemoval is missing Rook CR %q", kind)
		}
	}
}
