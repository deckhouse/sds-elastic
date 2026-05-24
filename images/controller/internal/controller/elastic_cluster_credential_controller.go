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
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/config"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/logger"
)

// ElasticClusterCredentialReconciler keeps the ECC.spec mirror of the
// rook-ceph-mon Secret current for every ElasticCluster in the cluster.
//
// The reconciler implements TWO out of the three flows described in the
// MVP plan:
//
//   - Get-or-Create: when an ElasticCluster exists but no matching ECC
//     does (1:1 by metadata.name), the controller creates an empty ECC
//     so subsequent BACK-SYNC iterations have something to populate.
//   - BACK-SYNC: read Secret/rook-ceph-mon (fsid, admin-secret,
//     mon-secret) and patch ECC.spec.{fsid,monSecret,adminSecret} when
//     they differ. Status.phase becomes Populated only when all three
//     fields are non-empty; intermediate states report Pending.
//
// The third flow — RESTORE (re-create the rook-ceph-mon Secret from
// ECC.spec on namespace re-create) — is intentionally NOT implemented
// in the MVP and is tracked as part of B-N1 backlog item.
//
// Drift detection (verifying the live Rook Secret still matches ECC) is
// also out of scope: the BACK-SYNC direction always overwrites a stale
// ECC, so a drift between Rook and ECC self-heals on the next reconcile.
//
// Finalizers are not used: ECC ownership is bound to the parent
// ElasticCluster via OwnerReferences (added in B-N1). For the MVP
// deletion of an ECC is a no-op; deletion of the parent EC orphans the
// ECC until B-N1 lands.
type ElasticClusterCredentialReconciler struct {
	Client client.Client
	Log    *logger.Logger
	Cfg    *config.Options
}

// AddElasticClusterCredentialReconcilerToManager wires the ECC
// reconciler into the controller-runtime manager. Watches:
//
//   - For:    ElasticClusterCredential (own resource).
//   - Watch:  ElasticCluster — a new EC must trigger Get-or-Create of its
//     matching ECC. Mapped 1:1 by name.
//   - Watch:  Secret in the controller namespace, filtered to
//     rook-ceph-mon. A Rook Secret rotation must propagate to
//     every ECC's spec backup.
//
// No watches on third-party CRDs are needed here.
func AddElasticClusterCredentialReconcilerToManager(mgr manager.Manager, cfg *config.Options, log *logger.Logger) error {
	r := &ElasticClusterCredentialReconciler{
		Client: mgr.GetClient(),
		Log:    log,
		Cfg:    cfg,
	}

	rookSecretPredicate := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == external.RookCephMonSecretName &&
			o.GetNamespace() == cfg.ControllerNamespace
	})

	// GenerationChangedPredicate avoids self-triggered reconciles on
	// status updates the controller writes itself (every status patch
	// would otherwise re-enqueue the same key). EC events are filtered by
	// the same predicate because the ECC reconciler only cares about
	// EC.metadata, not the EC FSM status.
	return ctrl.NewControllerManagedBy(mgr).
		Named("elastic-cluster-credential").
		For(&v1alpha1.ElasticClusterCredential{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&v1alpha1.ElasticCluster{}, handler.EnqueueRequestsFromMapFunc(r.enqueueECCForEC), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.enqueueAllECC), builder.WithPredicates(rookSecretPredicate)).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: cfg.MaxConcurrentReconciles,
		}).
		Complete(r)
}

// enqueueECCForEC translates an ElasticCluster event into a request for
// the matching ECC (1:1 by name). The mapper does not check whether the
// ECC already exists — Reconcile handles Get-or-Create.
func (r *ElasticClusterCredentialReconciler) enqueueECCForEC(_ context.Context, o client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: o.GetName()}}}
}

// enqueueAllECC enqueues every ElasticClusterCredential in the cluster.
// Used for Secret/rook-ceph-mon events: a Rook Secret rotation has to
// trigger BACK-SYNC across every ECC, regardless of which EC it backs.
func (r *ElasticClusterCredentialReconciler) enqueueAllECC(ctx context.Context, _ client.Object) []reconcile.Request {
	list := &v1alpha1.ElasticClusterCredentialList{}
	if err := r.Client.List(ctx, list); err != nil {
		r.Log.Error(err, "[enqueueAllECC] list failed")
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: list.Items[i].Name},
		})
	}
	return out
}

// Reconcile is invoked for every ECC name (== EC name).
//
// Steps:
//
//  1. Locate the parent EC. If absent, the ECC is orphaned — the MVP
//     leaves it untouched (B-N1 will introduce finalizer cleanup).
//  2. Ensure an ECC object exists for this EC (Get-or-Create with empty
//     spec). The empty-spec window is intentional — the OpenAPI schema
//     does not require any of the three fields, and the validating
//     webhook enforces "fully populated" only on the eventual
//     Phase=Populated transition.
//  3. Read Secret/rook-ceph-mon. Missing or partial Secret → status
//     Phase=Pending, no spec mutation.
//  4. Compute desired spec from the Secret. If different from current,
//     patch ECC.spec in place. Status.LastSyncTime updates only on a
//     successful read; Phase becomes Populated when every field is set.
//  5. updateECCStatus writes Phase / LastSyncTime / ObservedGeneration
//     under retry.RetryOnConflict.
func (r *ElasticClusterCredentialReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("[Reconcile] ECC %q", req.Name))

	ec := &v1alpha1.ElasticCluster{}
	switch err := r.Client.Get(ctx, client.ObjectKey{Name: req.Name}, ec); {
	case apierrors.IsNotFound(err):
		// ECC orphaned (parent EC deleted). MVP leaves cleanup to B-N1.
		return ctrl.Result{}, nil
	case err != nil:
		return ctrl.Result{}, err
	}

	ecc, err := r.getOrCreateECC(ctx, ec)
	if err != nil {
		return ctrl.Result{}, err
	}

	secret := &corev1.Secret{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Namespace: r.Cfg.ControllerNamespace,
		Name:      external.RookCephMonSecretName,
	}, secret)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval},
			r.updateECCStatus(ctx, ecc, v1alpha1.ECCPhasePending, false)
	}
	if err != nil {
		r.bestEffortPhaseError(ctx, ecc)
		return ctrl.Result{}, err
	}

	desired := desiredECCSpec(secret)
	populated := desired.FSID != "" && desired.MonSecret != "" && desired.AdminSecret != ""

	specPatched := false
	if !reflect.DeepEqual(ecc.Spec, desired) {
		patch := client.MergeFrom(ecc.DeepCopy())
		ecc.Spec = desired
		if err := r.Client.Patch(ctx, ecc, patch); err != nil {
			r.bestEffortPhaseError(ctx, ecc)
			return ctrl.Result{}, fmt.Errorf("patch ECC.spec: %w", err)
		}
		specPatched = true
	}

	phase := v1alpha1.ECCPhasePending
	if populated {
		phase = v1alpha1.ECCPhasePopulated
	}
	// LastSyncTime is bumped only when the BACK-SYNC actually mutated the
	// spec (or transitions the ECC to Populated for the first time —
	// updateECCStatus enforces the "first-time" branch). On a fully
	// converged steady-state cluster every reconcile is a no-op for the
	// status subresource, which keeps the For watch quiet.
	if err := r.updateECCStatus(ctx, ecc, phase, specPatched); err != nil {
		return ctrl.Result{}, err
	}

	if !populated {
		return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
	}
	return ctrl.Result{}, nil
}

// bestEffortPhaseError flips ECC.status.phase to Error so operators see
// the controller is failing without having to inspect the manager log.
// Intentionally swallows write errors: the parent error is what we want
// to surface to controller-runtime; a status write loss is recoverable
// on the next reconcile.
func (r *ElasticClusterCredentialReconciler) bestEffortPhaseError(ctx context.Context, ecc *v1alpha1.ElasticClusterCredential) {
	if err := r.updateECCStatus(ctx, ecc, v1alpha1.ECCPhaseError, false); err != nil {
		r.Log.Error(err, fmt.Sprintf("[bestEffortPhaseError] unable to mark ECC %q as Error", ecc.Name))
	}
}

// getOrCreateECC returns the ECC for the given EC, creating an empty one
// when missing. Uses the EC name 1:1 (CRD-level CEL invariant).
func (r *ElasticClusterCredentialReconciler) getOrCreateECC(ctx context.Context, ec *v1alpha1.ElasticCluster) (*v1alpha1.ElasticClusterCredential, error) {
	ecc := &v1alpha1.ElasticClusterCredential{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: ec.Name}, ecc)
	if err == nil {
		return ecc, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get ECC %q: %w", ec.Name, err)
	}

	fresh := &v1alpha1.ElasticClusterCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name: ec.Name,
			Labels: map[string]string{
				external.ManagedByLabelKey: external.ManagedByLabelValue,
				external.ECClusterLabel:    ec.Name,
			},
		},
	}
	if err := r.Client.Create(ctx, fresh); err != nil {
		// Race: another reconcile created it between our Get and Create.
		if apierrors.IsAlreadyExists(err) {
			if getErr := r.Client.Get(ctx, client.ObjectKey{Name: ec.Name}, fresh); getErr != nil {
				return nil, fmt.Errorf("re-get ECC %q after AlreadyExists: %w", ec.Name, getErr)
			}
			return fresh, nil
		}
		return nil, fmt.Errorf("create ECC %q: %w", ec.Name, err)
	}
	r.Log.Info(fmt.Sprintf("[Reconcile] created ECC %q for ElasticCluster %q", ec.Name, ec.Name))
	return fresh, nil
}

// desiredECCSpec extracts the three back-up fields from a rook-ceph-mon
// Secret, trimming whitespace. Missing or empty data keys yield empty
// strings (caller decides whether that means Pending).
func desiredECCSpec(secret *corev1.Secret) v1alpha1.ElasticClusterCredentialSpec {
	return v1alpha1.ElasticClusterCredentialSpec{
		FSID:        strings.TrimSpace(string(secret.Data[external.RookCephMonSecretFSIDKey])),
		MonSecret:   strings.TrimSpace(string(secret.Data[external.RookCephMonSecretMonSecretKey])),
		AdminSecret: strings.TrimSpace(string(secret.Data[external.RookCephMonSecretAdminSecretKey])),
	}
}

// updateECCStatus publishes Phase and ObservedGeneration. LastSyncTime
// is bumped only when:
//
//   - bumpLastSync is true (the caller observed a real spec mutation), OR
//   - the ECC's status has never carried a LastSyncTime yet AND the new
//     phase is Populated (the first successful BACK-SYNC).
//
// Both branches preserve the status no-op short-circuit: a steady-state
// reconcile that finds spec already in sync and Phase already Populated
// produces no Status().Update call and therefore does not retrigger the
// `For` watch on this controller.
//
// retry.RetryOnConflict shields against the rare concurrent update
// (Reconcile racing against itself due to a watch flap).
func (r *ElasticClusterCredentialReconciler) updateECCStatus(ctx context.Context, ecc *v1alpha1.ElasticClusterCredential, phase string, bumpLastSync bool) error {
	now := metav1.NewTime(time.Now())
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.ElasticClusterCredential{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: ecc.Name}, latest); err != nil {
			return err
		}
		if latest.Status == nil {
			latest.Status = &v1alpha1.ElasticClusterCredentialStatus{}
		}
		before := latest.Status.DeepCopy()

		latest.Status.ObservedGeneration = latest.Generation
		latest.Status.Phase = phase
		if bumpLastSync || (latest.Status.LastSyncTime == nil && phase == v1alpha1.ECCPhasePopulated) {
			latest.Status.LastSyncTime = &now
		}

		if reflect.DeepEqual(before, latest.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, latest)
	})
}
