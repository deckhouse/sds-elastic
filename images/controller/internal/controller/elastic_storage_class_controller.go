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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/external"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/config"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/logger"
)

// ElasticStorageClassReconciler drives every ElasticStorageClass through a
// short two-stage FSM:
//
//		PoolReady -> CsiStorageClassReady -> Ready
//
//	  - PoolReady covers both the EC dependency gate (parent EC must report
//	    CephClusterReady=True before any pool is created — otherwise the
//	    CephBlockPool/CephFilesystem CR has nothing to schedule against) and
//	    the actual Rook-side phase==Ready of the resulting pool.
//	  - CsiStorageClassReady creates the csi-ceph CephStorageClass and waits
//	    for status.phase==Created (csi-ceph then materialises the K8s
//	    StorageClass downstream).
//
// The reconciler reuses the same advance()/gateAfter() pattern from the
// EC reconciler so error/in-progress paths consistently gate the Ready
// aggregate and the printer column never lies about cluster readiness.
type ElasticStorageClassReconciler struct {
	Client client.Client
	Log    *logger.Logger
	Cfg    *config.Options
}

// escStageOrder lists the FSM stage condition types in execution order.
// Used by gateAfterESC and deriveESCPhase.
var escStageOrder = []string{
	v1alpha1.ESCConditionPoolReady,
	v1alpha1.ESCConditionCsiStorageClassReady,
}

// AddElasticStorageClassReconcilerToManager wires the ESC reconciler into
// the manager. Watches:
//
//   - For:    ElasticStorageClass.
//   - Watch:  ElasticCluster — when an EC's status flips to
//     CephClusterReady=True every ESC referencing it must be
//     re-reconciled to unblock the gate.
//   - Watch:  CephBlockPool / CephFilesystem (unstructured) — pool
//     status.phase changes drive PoolReady.
//   - Watch:  CephStorageClass (unstructured, csi-ceph) — drives
//     CsiStorageClassReady.
//
// Watches on third-party CRDs are conditionally registered against the
// manager's RESTMapper (same pattern as the EC reconciler) so a missing
// dependency CRD does not hang WaitForCacheSync.
func AddElasticStorageClassReconcilerToManager(mgr manager.Manager, cfg *config.Options, log *logger.Logger) error {
	r := &ElasticStorageClassReconciler{
		Client: mgr.GetClient(),
		Log:    log,
		Cfg:    cfg,
	}

	enqueueAll := handler.EnqueueRequestsFromMapFunc(r.enqueueAllESC)
	enqueueByEC := handler.EnqueueRequestsFromMapFunc(r.enqueueESCByCluster)

	// EC status updates do not bump generation, so
	// GenerationChangedPredicate would silently break the gate. Fire on
	// real CephClusterReady transitions only — that is the only EC field
	// the ESC FSM consults. Create / Delete events still propagate
	// (initial-creation enqueue, finalizer cleanup B20).
	ecCephClusterReadyPredicate := predicate.Funcs{
		CreateFunc:  func(_ event.CreateEvent) bool { return true },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return true },
		GenericFunc: func(_ event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldEC, _ := e.ObjectOld.(*v1alpha1.ElasticCluster)
			newEC, _ := e.ObjectNew.(*v1alpha1.ElasticCluster)
			return ecCephClusterReadyState(oldEC) != ecCephClusterReadyState(newEC)
		},
	}

	// GenerationChangedPredicate alone would miss two teardown signals:
	// setting deletionTimestamp does not bump generation, and the
	// force-deletion annotation is a metadata-only edit. OR-in a predicate
	// that passes any update to an object already terminating so deletion
	// and the force annotation both re-enqueue.
	escPredicate := predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				return e.ObjectNew != nil && !e.ObjectNew.GetDeletionTimestamp().IsZero()
			},
		},
	)

	b := ctrl.NewControllerManagedBy(mgr).
		Named("elastic-storage-class").
		For(&v1alpha1.ElasticStorageClass{}, builder.WithPredicates(escPredicate)).
		Watches(&v1alpha1.ElasticCluster{}, enqueueByEC, builder.WithPredicates(ecCephClusterReadyPredicate)).
		Watches(&corev1.PersistentVolume{}, handler.EnqueueRequestsFromMapFunc(r.enqueueDeletingESCByPV)).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: cfg.MaxConcurrentReconciles,
		})

	mapper := mgr.GetRESTMapper()
	for _, gvk := range []schema.GroupVersionKind{
		external.CephBlockPoolGVK,
		external.CephFilesystemGVK,
		external.CephStorageClassGVK,
	} {
		if !gvkRegistered(mapper, gvk) {
			log.Warning(fmt.Sprintf("[setup] CRD %s not registered; skipping watch", gvk.Kind))
			continue
		}
		stub := &unstructured.Unstructured{}
		stub.SetGroupVersionKind(gvk)
		b = b.Watches(stub, enqueueAll)
	}

	return b.Complete(r)
}

// ecCephClusterReadyState extracts the EC's CephClusterReady condition
// status as a comparable value (or "" if EC / status / condition is
// missing). Used by the watch predicate to fire only when this single
// condition transitions; all other EC status writes are ignored.
func ecCephClusterReadyState(ec *v1alpha1.ElasticCluster) string {
	if ec == nil || ec.Status == nil {
		return ""
	}
	cond := apimeta.FindStatusCondition(ec.Status.Conditions, v1alpha1.ECConditionCephClusterReady)
	if cond == nil {
		return ""
	}
	return string(cond.Status)
}

// enqueueAllESC returns a Reconcile request for every ElasticStorageClass.
// Coarse-grained but cheap given MVP cardinality. The per-ESC field
// indexer over spec.clusterRef is tracked in B21.
func (r *ElasticStorageClassReconciler) enqueueAllESC(ctx context.Context, _ client.Object) []reconcile.Request {
	list := &v1alpha1.ElasticStorageClassList{}
	if err := r.Client.List(ctx, list); err != nil {
		r.Log.Error(err, "[enqueueAllESC] list failed")
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

// enqueueESCByCluster maps an ElasticCluster event to every ESC that
// references it via spec.clusterRef. Without a field indexer the lookup
// is a linear scan over all ESCs (B21 to add the indexer).
func (r *ElasticStorageClassReconciler) enqueueESCByCluster(ctx context.Context, o client.Object) []reconcile.Request {
	ecName := o.GetName()
	list := &v1alpha1.ElasticStorageClassList{}
	if err := r.Client.List(ctx, list); err != nil {
		r.Log.Error(err, "[enqueueESCByCluster] list failed")
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.ClusterRef != ecName {
			continue
		}
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: list.Items[i].Name},
		})
	}
	return out
}

// enqueueDeletingESCByPV maps a PersistentVolume event to the single ESC
// whose StorageClass provisioned it, but only while that ESC is being
// deleted. This keeps the bound-PV teardown guard responsive (a PVC
// released its PV) without enqueuing every ESC on cluster-wide PV churn.
func (r *ElasticStorageClassReconciler) enqueueDeletingESCByPV(ctx context.Context, o client.Object) []reconcile.Request {
	pv, ok := o.(*corev1.PersistentVolume)
	if !ok || pv.Spec.StorageClassName == "" {
		return nil
	}
	esc := &v1alpha1.ElasticStorageClass{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: pv.Spec.StorageClassName}, esc); err != nil {
		return nil
	}
	if esc.DeletionTimestamp == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: esc.Name}}}
}

// Reconcile dispatches the ESC FSM, or runs the destructive teardown when
// the ElasticStorageClass is being deleted (see reconcileDeleteESC).
func (r *ElasticStorageClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("[Reconcile] start for ElasticStorageClass %q", req.Name))

	esc := &v1alpha1.ElasticStorageClass{}
	if err := r.Client.Get(ctx, req.NamespacedName, esc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if esc.DeletionTimestamp != nil {
		return r.reconcileDeleteESC(ctx, esc)
	}

	if err := r.ensureESCFinalizer(ctx, esc); err != nil {
		return ctrl.Result{}, err
	}
	return r.reconcileNormal(ctx, esc)
}

func (r *ElasticStorageClassReconciler) reconcileNormal(ctx context.Context, esc *v1alpha1.ElasticStorageClass) (ctrl.Result, error) {
	status := newESCStatusBuilder(esc)

	poolDone, msg, err := r.ensurePool(ctx, esc)
	if !r.advanceESC(status, v1alpha1.ESCConditionPoolReady, poolDone, msg, err) {
		return r.finishESCReconcile(ctx, esc, status, err)
	}

	csiDone, msg, err := r.ensureCsiStorageClass(ctx, esc)
	if !r.advanceESC(status, v1alpha1.ESCConditionCsiStorageClassReady, csiDone, msg, err) {
		return r.finishESCReconcile(ctx, esc, status, err)
	}

	status.setCondition(v1alpha1.ESCConditionReady, metav1.ConditionTrue, "Ready", "All stages reconciled")
	return r.finishESCReconcile(ctx, esc, status, nil)
}

// advanceESC mirrors the EC reconciler's advance(): records the stage
// outcome on the status builder, gates downstream conditions, and
// returns whether the FSM may progress.
func (r *ElasticStorageClassReconciler) advanceESC(status *escStatusBuilder, condType string, done bool, msg string, err error) bool {
	switch {
	case err != nil:
		status.setCondition(condType, metav1.ConditionFalse, "Error", err.Error())
		gateAfterESC(status, condType)
	case !done:
		status.setCondition(condType, metav1.ConditionFalse, "InProgress", msg)
		gateAfterESC(status, condType)
	default:
		status.setCondition(condType, metav1.ConditionTrue, "Ready", msg)
		return true
	}
	return false
}

// gateAfterESC marks every stage strictly downstream of `afterStage` and
// the aggregate Ready as False/WaitingForPrev.
func gateAfterESC(status *escStatusBuilder, afterStage string) {
	startIdx := -1
	for i, t := range escStageOrder {
		if t == afterStage {
			startIdx = i + 1
			break
		}
	}
	if startIdx >= 0 {
		for _, t := range escStageOrder[startIdx:] {
			status.setCondition(t, metav1.ConditionFalse, "WaitingForPrev",
				fmt.Sprintf("waiting for %s", afterStage))
		}
	}
	status.setCondition(v1alpha1.ESCConditionReady, metav1.ConditionFalse,
		"WaitingForPrev", fmt.Sprintf("waiting for %s", afterStage))
}

// escStatusBuilder is the ESC sibling of ecStatusBuilder.
type escStatusBuilder struct {
	source     *v1alpha1.ElasticStorageClass
	conditions []metav1.Condition
}

func newESCStatusBuilder(esc *v1alpha1.ElasticStorageClass) *escStatusBuilder {
	return &escStatusBuilder{source: esc}
}

func (s *escStatusBuilder) setCondition(condType string, condStatus metav1.ConditionStatus, reason, message string) {
	s.conditions = append(s.conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: s.source.Generation,
		LastTransitionTime: metav1.NewTime(time.Now()),
	})
}

func (r *ElasticStorageClassReconciler) finishESCReconcile(
	ctx context.Context,
	esc *v1alpha1.ElasticStorageClass,
	status *escStatusBuilder,
	reconcileErr error,
) (ctrl.Result, error) {
	if err := r.updateESCStatus(ctx, esc, status); err != nil {
		r.Log.Error(err, "[finishESCReconcile] unable to update status")
		if reconcileErr == nil {
			reconcileErr = err
		}
	}
	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}
	if isAggregateReadyESC(status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
}

func isAggregateReadyESC(status *escStatusBuilder) bool {
	for i := len(status.conditions) - 1; i >= 0; i-- {
		c := status.conditions[i]
		if c.Type == v1alpha1.ESCConditionReady {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

func (r *ElasticStorageClassReconciler) updateESCStatus(ctx context.Context, esc *v1alpha1.ElasticStorageClass, sb *escStatusBuilder) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.ElasticStorageClass{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: esc.Name}, latest); err != nil {
			return err
		}
		if latest.Status == nil {
			latest.Status = &v1alpha1.ElasticStorageClassStatus{}
		}
		before := latest.Status.DeepCopy()

		for _, cond := range sb.conditions {
			apimeta.SetStatusCondition(&latest.Status.Conditions, cond)
		}
		// ObservedGeneration tracks the spec generation we observed at
		// the top of Reconcile (esc.Generation), matching the EC
		// reconciler's semantics.
		latest.Status.ObservedGeneration = esc.Generation
		latest.Status.Phase = deriveESCPhase(latest.Status.Conditions)

		if reflect.DeepEqual(before, latest.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, latest)
	})
}

// deriveESCPhase converts the FSM stage conditions into the coarse Phase.
// Allow-list of stage condition types (escStageOrder) keeps the
// computation correct under future ESCConditionReady direct writes —
// matches deriveECPhase semantics.
func deriveESCPhase(conditions []metav1.Condition) string {
	if len(conditions) == 0 {
		return v1alpha1.PhasePending
	}
	hasError := false
	hasFalse := false
	for _, t := range escStageOrder {
		c := apimeta.FindStatusCondition(conditions, t)
		if c == nil || c.Status != metav1.ConditionFalse {
			continue
		}
		hasFalse = true
		if c.Reason == "Error" {
			hasError = true
		}
	}
	if hasError {
		return v1alpha1.PhaseError
	}
	if hasFalse {
		return v1alpha1.PhaseInProgress
	}
	return v1alpha1.PhaseReady
}
