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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/deckhouse/sds-common-lib/conditions"
	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/config"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/logger"
)

// ElasticClusterReconciler drives an ElasticCluster CR through the FSM:
//
//	StorageReady -> CephClusterReady -> CredentialsReady -> CsiCephReady ->
//	UpgradeReady -> Ready
//
// Each stage is implemented as a separate ensure*() method that returns
// (done, message, error). The reconciler funnels every stage outcome
// through advance(), which:
//
//   - on err  → condition False/Error  + gates downstream + aggregate Ready=False/WaitingForPrev
//   - on !done → condition False/InProgress + gates downstream + aggregate Ready=False/WaitingForPrev
//   - on done  → condition True/Ready  and the FSM is allowed to progress
//
// Both Error and InProgress paths share the same gating so the published
// status never carries a stale "Ready=True" on a downstream stage when an
// upstream stage is unhealthy. The aggregate Ready=True is set only after
// every stage passes.
type ElasticClusterReconciler struct {
	Client client.Client
	Log    *logger.Logger
	Cfg    *config.Options
}

// AddElasticClusterReconcilerToManager wires the reconciler into the
// controller-runtime manager.
//
// The controller watches every CR/object that can affect the FSM stages
// so user-visible "Pending → Ready" does not depend on the periodic
// RequeueAfter:
//
//   - For:    ElasticCluster (the primary reconciliation target).
//   - Watch:  Secret/rook-ceph-mon (filtered by name/namespace) and
//     ConfigMap/rook-ceph-mon-endpoints (filtered by name/namespace)
//     — both are the source of CredentialsReady status fields.
//   - Watch:  CephCluster, CephClusterConnection, BlockDevice (unstructured)
//     — Rook/csi-ceph/sds-node-configurator status changes drive
//     the corresponding stages.
//   - Watch:  ElasticClusterCredential — once the ECC reconciler (next
//     commit) populates spec.adminSecret, the EC reconciler must
//     pick it up immediately for CsiCephReady.
//
// Every external watch enqueues every ElasticCluster (low cardinality:
// one EC per cluster in MVP). Per-resource filtering through field
// selectors / label selectors is a backlog optimisation (B21).
//
// Watches on third-party CRDs that are not yet registered tolerate
// NoMatchError at runtime: stages probe IsNoMatchError on every Get/upsert
// and surface "waiting for CRD X" via the stage condition.
func AddElasticClusterReconcilerToManager(mgr manager.Manager, cfg *config.Options, log *logger.Logger) error {
	r := &ElasticClusterReconciler{
		Client: mgr.GetClient(),
		Log:    log,
		Cfg:    cfg,
	}

	rookSecretPredicate := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == external.RookCephMonSecretName &&
			o.GetNamespace() == cfg.ControllerNamespace
	})
	monCMPredicate := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetName() == external.RookCephMonEndpointsConfigMap &&
			o.GetNamespace() == cfg.ControllerNamespace
	})

	enqueueAll := handler.EnqueueRequestsFromMapFunc(r.enqueueAllElasticClusters)
	enqueueByESC := handler.EnqueueRequestsFromMapFunc(r.enqueueECByESC)

	// GenerationChangedPredicate alone would drop the Update event that
	// sets metadata.deletionTimestamp (deletion does not bump
	// generation), so a finalizer-held ElasticCluster would never reach
	// reconcileDelete. OR it with a predicate that always passes a
	// terminating object through.
	ecPredicate := predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				return e.ObjectNew != nil && !e.ObjectNew.GetDeletionTimestamp().IsZero()
			},
		},
	)

	b := ctrl.NewControllerManagedBy(mgr).
		Named("elastic-cluster").
		For(&v1alpha1.ElasticCluster{}, builder.WithPredicates(ecPredicate)).
		Watches(&corev1.Secret{}, enqueueAll, builder.WithPredicates(rookSecretPredicate)).
		Watches(&corev1.ConfigMap{}, enqueueAll, builder.WithPredicates(monCMPredicate)).
		Watches(&v1alpha1.ElasticClusterCredential{}, enqueueAll).
		// ESC create / spec change / delete must reach the EC reconciler:
		// presence of a HighRedundancy ESC is the trigger for the
		// auto-promotion in computeCephTopology, so a pure RequeueAfter
		// safety net would delay reaction up to one full requeue
		// interval. The mapper resolves to the single EC named by the
		// ESC's spec.clusterRef instead of fanning out to every EC.
		Watches(&v1alpha1.ElasticStorageClass{}, enqueueByESC).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: cfg.MaxConcurrentReconciles,
		})

	// Watches on third-party CRDs (Rook, csi-ceph, sds-node-configurator)
	// are registered ONLY if their CRDs are already known to the apiserver
	// at controller startup. Registering a Watch on an unknown GVK would
	// hang the manager's WaitForCacheSync indefinitely — Cache.Start would
	// retry the LIST forever. The runtime isNoMatchErr defenses inside
	// ensure*() stages do not cover this path: WaitForCacheSync runs before
	// the first Reconcile.
	//
	// Trade-off: if a dependency CRD is installed AFTER the sds-elastic
	// pod starts, the controller will not auto-watch it; the operator must
	// restart the pod (or wait for a leader-election cycle). Tracked as
	// part of B20 (the rebuild-on-CRD-install loop is small and shares
	// scope with the OwnerReferences/finalizer work).
	mapper := mgr.GetRESTMapper()
	for _, gvk := range []schema.GroupVersionKind{
		external.CephClusterGVK,
		external.CephClusterConnectionGVK,
		external.BlockDeviceGVK,
	} {
		if !gvkRegistered(mapper, gvk) {
			log.Warning(fmt.Sprintf("[setup] CRD %s not registered; skipping watch (controller will reconcile via RequeueAfter until pod restart)", gvk.Kind))
			continue
		}
		stub := &unstructured.Unstructured{}
		stub.SetGroupVersionKind(gvk)
		b = b.Watches(stub, enqueueAll)
	}

	return b.Complete(r)
}

// gvkRegistered probes the manager's RESTMapper for a GVK. Returns false
// when the corresponding CRD is not yet installed — used by the setup
// phase to avoid registering Watches that would block WaitForCacheSync.
func gvkRegistered(mapper apimeta.RESTMapper, gvk schema.GroupVersionKind) bool {
	_, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err == nil {
		return true
	}
	if apimeta.IsNoMatchError(err) {
		return false
	}
	// Other discovery errors are best-effort: try to register the watch
	// anyway and let controller-runtime surface the failure verbosely.
	return true
}

// enqueueAllElasticClusters returns a Reconcile request for every
// ElasticCluster in the cluster. Used as the mapper for every external
// watch — coarse-grained but cheap given MVP cardinality (1 EC).
func (r *ElasticClusterReconciler) enqueueAllElasticClusters(ctx context.Context, _ client.Object) []reconcile.Request {
	list := &v1alpha1.ElasticClusterList{}
	if err := r.Client.List(ctx, list); err != nil {
		r.Log.Error(err, "[enqueueAllElasticClusters] list failed")
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

// enqueueECByESC maps an ESC change to the single EC the ESC references
// via spec.clusterRef. Cluster-scoped resource, so no namespace; the
// returned request name is the EC name. Returns no requests when the
// ESC has an empty clusterRef — the ESC reconciler is responsible for
// rejecting such ESCs as Invalid, and the EC reconciler does not need
// to react to them.
func (r *ElasticClusterReconciler) enqueueECByESC(_ context.Context, obj client.Object) []reconcile.Request {
	esc, ok := obj.(*v1alpha1.ElasticStorageClass)
	if !ok || esc.Spec.ClusterRef == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: esc.Spec.ClusterRef},
	}}
}

// Reconcile is the entry point of the FSM.
func (r *ElasticClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("[Reconcile] start for ElasticCluster %q", req.Name))

	ec := &v1alpha1.ElasticCluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, ec); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Teardown path: reconcileDelete owns the ordered removal of the
	// Rook/csi-ceph resources the operator cannot delete by hand. PV /
	// LLV / LVG and the BlockDevice cluster label are left for manual
	// cleanup by label (documented procedure); OwnerReferences-driven
	// garbage collection of those remains backlog B20.
	if ec.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, ec)
	}

	// Ensure the teardown finalizer before reconciling the spec so a
	// delete issued mid-provisioning still runs reconcileDelete.
	if err := r.ensureECFinalizer(ctx, ec); err != nil {
		return ctrl.Result{}, err
	}

	return r.reconcileNormal(ctx, ec)
}

// stageOrder lists the FSM stage conditions in execution order. Used by
// gateRemainingStages and deriveECPhase to keep the two surfaces in sync.
var stageOrder = []string{
	v1alpha1.ECConditionStorageReady,
	v1alpha1.ECConditionCephClusterReady,
	v1alpha1.ECConditionCredentialsReady,
	v1alpha1.ECConditionCsiCephReady,
	v1alpha1.ECConditionUpgradeReady,
}

func (r *ElasticClusterReconciler) reconcileNormal(ctx context.Context, ec *v1alpha1.ElasticCluster) (ctrl.Result, error) {
	status := newECStatusBuilder(ec)

	storageDone, osdCount, pvcRequest, storageReason, msg, err := r.ensureStorage(ctx, ec)
	status.desiredOSDs = osdCount
	if !r.advance(status, v1alpha1.ECConditionStorageReady, storageDone, storageReason, msg, err) {
		return r.finishReconcile(ctx, ec, status, err)
	}

	cephDone, msg, cephTopology, upgradeProbe, err := r.ensureCephCluster(ctx, ec, osdCount, pvcRequest)
	if cephTopology != nil {
		status.cephTopology = cephTopology
	}
	// Publish the upgrade signal BEFORE the FSM gate. ensureCephCluster
	// returns done=false during a Rook rolling upgrade
	// (CephCluster.status.phase=Progressing for the entire mon → mgr →
	// osd → mds rollout window), so r.advance(...) below would otherwise
	// trip gateAfter and the UpgradeInProgress condition would flap to
	// False/WaitingForPrev exactly while the rollout is happening. Doing
	// the publish here keeps the signal True for the duration of the
	// rollout, regardless of the FSM gate.
	if upgradeProbe != nil {
		applyUpgradeProbeToStatus(status, v1alpha1.DefaultCephVersion, *upgradeProbe)
		setUpgradeInProgress(status, upgradeProbe.InProgress, upgradeProbe.Msg)
	}
	if !r.advance(status, v1alpha1.ECConditionCephClusterReady, cephDone, "", msg, err) {
		return r.finishReconcile(ctx, ec, status, err)
	}

	credsDone, msg, err := r.ensureCredentials(ctx, ec, status)
	if !r.advance(status, v1alpha1.ECConditionCredentialsReady, credsDone, "", msg, err) {
		return r.finishReconcile(ctx, ec, status, err)
	}

	csiDone, msg, err := r.ensureCsiCeph(ctx, ec, status)
	if !r.advance(status, v1alpha1.ECConditionCsiCephReady, csiDone, "", msg, err) {
		return r.finishReconcile(ctx, ec, status, err)
	}

	upgDone, inProgress, msg, err := r.ensureUpgrade(ctx, ec, status)
	if !r.advance(status, v1alpha1.ECConditionUpgradeReady, upgDone, "", msg, err) {
		// UpgradeInProgress is a signal alongside the gate; keep it accurate
		// even on the failed path so consumers (UI / metrics) can tell apart
		// a hung pre-upgrade health gate from an active rollout.
		setUpgradeInProgress(status, inProgress, msg)
		return r.finishReconcile(ctx, ec, status, err)
	}
	setUpgradeInProgress(status, inProgress, msg)

	status.setCondition(v1alpha1.ECConditionReady, metav1.ConditionTrue, "Ready", "All stages reconciled")
	return r.finishReconcile(ctx, ec, status, nil)
}

// advance records the outcome of a stage on the status builder and returns
// whether the FSM is allowed to progress to the next stage.
//
//   - err != nil  → condition False/Error, downstream gated, return false.
//   - !done       → condition False/<reason or InProgress>, downstream gated,
//     return false. `reason` lets a stage publish a richer machine-readable
//     cause (e.g. "WaitingForLVG", "WaitingForLLV") so a UI can dispatch on
//     reason without parsing the human-readable message. Empty `reason`
//     falls back to the generic "InProgress".
//   - done        → condition True/Ready, return true.
//
// The aggregate Ready condition is also pushed to False on every non-pass
// outcome so the printer column never lies about cluster readiness while
// any stage is unhealthy.
func (r *ElasticClusterReconciler) advance(status *ecStatusBuilder, condType string, done bool, reason, msg string, err error) bool {
	return ecStages().Advance(&status.conditions, status.source.Generation, condType, done, reason, msg, err)
}

// gateAfter marks every stage condition strictly downstream of `gateAfter`
// as False/WaitingForPrev, plus the aggregate Ready. Idempotent; safe to
// call from both the Error and InProgress paths.
//
// UpgradeInProgress is intentionally NOT touched here. It is a signal,
// not a stage, and is published by two authoritative sites:
//
//   - ensureCephCluster (via reconcileNormal) — runs every time the
//     CephCluster object is reachable, even when status.phase is not
//     Ready, so the True/False value reflects the current cluster state
//     during a Rook rolling upgrade (which keeps phase=Progressing for
//     the entire mon → mgr → osd rollout window).
//   - ensureUpgrade — runs at the UpgradeReady stage and gates that
//     condition; reuses the same probe.
//
// Forcing WaitingForPrev here used to clobber the True signal exactly
// while a rolling upgrade was rolling, surfacing as `UPGRADING=False
// REASON=WaitingForPrev` immediately after the upgrade started. Letting
// the explicit publishers own the condition keeps the signal accurate
// for the entire rollout. When upstream stages fail before the
// CephCluster has been fetched (e.g. StorageReady error), the previous
// UpgradeInProgress value stays sticky on the status — acceptable,
// since we have no fresh evidence either way.
func gateAfter(status *ecStatusBuilder, afterStage string) {
	ecStages().Gate(&status.conditions, status.source.Generation, afterStage)
}

// setUpgradeInProgress publishes the UpgradeInProgress signal condition.
// True when Rook is actively rolling pods; False otherwise.
func setUpgradeInProgress(status *ecStatusBuilder, inProgress bool, msg string) {
	if inProgress {
		status.setCondition(v1alpha1.ECConditionUpgradeInProgress, metav1.ConditionTrue, "Upgrading", msg)
		return
	}
	status.setCondition(v1alpha1.ECConditionUpgradeInProgress, metav1.ConditionFalse, "Stable", "no upgrade in progress")
}

// finishReconcile flushes the status builder and computes the requeue
// behaviour. The reconciler currently has no event sources for its
// dependencies (Watches added in this commit cover Secret/CM/Rook/csi-ceph
// CRs) so a periodic RequeueAfter is the safety net for missed events,
// not the primary convergence loop.
//
// When the cluster is fully Ready the reconciler omits the explicit
// RequeueAfter — the watches handle subsequent state changes — which
// avoids wasting reconcile slots on a steady-state cluster.
func (r *ElasticClusterReconciler) finishReconcile(
	ctx context.Context,
	ec *v1alpha1.ElasticCluster,
	status *ecStatusBuilder,
	reconcileErr error,
) (ctrl.Result, error) {
	// Populate observability fields on every exit path so the UI gets
	// the latest health/capacity/Pod-count snapshot whether or not the
	// FSM converged this reconcile. Best-effort: any error inside
	// populateObservability is logged and absorbed.
	r.populateObservability(ctx, ec, status, status.desiredOSDs)
	status.observed = true
	if err := r.updateECStatus(ctx, ec, status); err != nil {
		r.Log.Error(err, "[finishReconcile] unable to update status")
		if reconcileErr == nil {
			reconcileErr = err
		}
	}
	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}
	if isAggregateReady(status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
}

// isAggregateReady returns true when the status builder has set Ready=True
// during this reconcile (i.e. every stage passed). Reads the local builder
// rather than the API server to avoid a stale status race.
func isAggregateReady(status *ecStatusBuilder) bool {
	for i := len(status.conditions) - 1; i >= 0; i-- {
		c := status.conditions[i]
		if c.Type == v1alpha1.ECConditionReady {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// ecStatusBuilder accumulates conditions plus the populated EC.status fields
// across stages and writes them back via retry.RetryOnConflict.
type ecStatusBuilder struct {
	source     *v1alpha1.ElasticCluster
	conditions []metav1.Condition

	cephFSID       string
	monEndpoints   []string
	monMaxID       string
	credentialsRef *v1alpha1.ElasticClusterCredentialRef
	cephVersion    *v1alpha1.CephVersionStatus
	cephTopology   *v1alpha1.CephTopologyStatus

	// Observability fields populated by populateObservability after the
	// FSM stages have run. Pointer-typed: nil means "not observed yet";
	// updateECStatus only overwrites the corresponding latest.Status.*
	// when the pointer is non-nil so a stage that fails before
	// populateObservability runs does not destroy a previously-published
	// snapshot.
	health   *v1alpha1.CephHealthStatus
	capacity *v1alpha1.CephCapacityStatus
	osds     *v1alpha1.OSDStatus
	mons     *v1alpha1.DaemonStatus
	mgrs     *v1alpha1.DaemonStatus

	// observed marks whether populateObservability ran in this reconcile.
	// When true, updateECStatus also clears stale observability fields
	// (e.g. CephCluster removed → status.health goes back to nil) instead
	// of indefinitely keeping the previously-published snapshot.
	observed bool

	// desiredOSDs is the OSD count ensureStorage reported (== matched
	// BlockDevice count). Surfaced verbatim under EC.status.osds.desired
	// so the UI can render "Provisioning N OSDs" even before Rook has
	// scheduled any rook-ceph-osd Pod.
	desiredOSDs int32
}

func newECStatusBuilder(ec *v1alpha1.ElasticCluster) *ecStatusBuilder {
	sb := &ecStatusBuilder{source: ec}
	if ec.Status != nil {
		sb.cephFSID = ec.Status.CephFSID
		sb.monEndpoints = ec.Status.MonEndpoints
		sb.monMaxID = ec.Status.MonMaxID
		sb.credentialsRef = ec.Status.CredentialsRef.DeepCopy()
		sb.cephVersion = ec.Status.CephVersion.DeepCopy()
		sb.cephTopology = ec.Status.CephTopology.DeepCopy()
	}
	return sb
}

func (s *ecStatusBuilder) setCondition(condType string, condStatus metav1.ConditionStatus, reason, message string) {
	conditions.Set(&s.conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            conditions.TruncateMessage(message),
		ObservedGeneration: s.source.Generation,
	})
}

func (r *ElasticClusterReconciler) updateECStatus(ctx context.Context, ec *v1alpha1.ElasticCluster, sb *ecStatusBuilder) error {
	return conditions.UpdateStatus(ctx, r.Client, ec, func(latest *v1alpha1.ElasticCluster) {
		if latest.Status == nil {
			latest.Status = &v1alpha1.ElasticClusterStatus{}
		}

		for _, cond := range sb.conditions {
			conditions.Set(&latest.Status.Conditions, cond)
		}
		// ObservedGeneration intentionally tracks the generation we read
		// at the top of Reconcile (ec.Generation), not latest.Generation:
		// "we have processed the spec at this generation", regardless of
		// whether a newer spec landed during the reconcile.
		latest.Status.ObservedGeneration = ec.Generation
		if sb.cephFSID != "" {
			latest.Status.CephFSID = sb.cephFSID
		}
		if len(sb.monEndpoints) > 0 {
			latest.Status.MonEndpoints = sb.monEndpoints
		}
		if sb.monMaxID != "" {
			latest.Status.MonMaxID = sb.monMaxID
		}
		if sb.credentialsRef != nil {
			latest.Status.CredentialsRef = sb.credentialsRef
		}
		if sb.cephVersion != nil {
			latest.Status.CephVersion = sb.cephVersion
		}
		if sb.cephTopology != nil {
			latest.Status.CephTopology = sb.cephTopology
		}
		if sb.observed {
			// observed=true means populateObservability ran. Authoritatively
			// overwrite the observability fields — including back-to-nil
			// transitions (e.g. CephCluster deleted, mon Pods drained) so
			// the UI never lingers on a stale "HEALTH_OK" after a real
			// regression.
			latest.Status.Health = sb.health
			latest.Status.Capacity = sb.capacity
			latest.Status.OSDs = sb.osds
			latest.Status.Mons = sb.mons
			latest.Status.Mgrs = sb.mgrs
		}
		latest.Status.Phase = deriveECPhase(latest.Status.Conditions)
	})
}

// deriveECPhase converts the FSM stage conditions into the coarse Phase.
// Uses an explicit allow-list of stage condition types (stageOrder) so
// the function stays correct if anyone later adds new "signal" conditions
// (à la UpgradeInProgress) or if ECConditionReady is ever flipped to
// False with an Error reason directly. Aggregate Ready and the
// UpgradeInProgress signal are intentionally excluded.
//
//   - empty conditions slice                          → Pending
//   - any stage False with Reason=="Error"            → Error
//   - any stage False (other reasons, e.g. InProgress) → InProgress
//   - all stages True (or absent)                     → Ready
func deriveECPhase(conditions []metav1.Condition) string {
	return ecStages().Phase(conditions)
}
