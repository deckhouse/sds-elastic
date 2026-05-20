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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/config"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/logger"
)

// SdsElasticClusterReconciler is the entry-point reconciler for the
// aggregate SdsElasticCluster CR.
type SdsElasticClusterReconciler struct {
	Client client.Client
	Log    *logger.Logger
	Cfg    *config.Options
}

// AddSdsElasticClusterReconcilerToManager wires the reconciler into the
// controller-runtime manager.
func AddSdsElasticClusterReconcilerToManager(mgr manager.Manager, cfg *config.Options, log *logger.Logger) error {
	r := &SdsElasticClusterReconciler{
		Client: mgr.GetClient(),
		Log:    log,
		Cfg:    cfg,
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("sds-elastic-cluster").
		For(&v1alpha1.SdsElasticCluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: cfg.MaxConcurrentReconciles,
		}).
		Complete(r)
}

// Reconcile is the main controller loop.
func (r *SdsElasticClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("[Reconcile] start for SdsElasticCluster %q", req.Name))

	cluster := &v1alpha1.SdsElasticCluster{}
	if err := r.Client.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if cluster.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, cluster)
	}

	if added, err := r.addFinalizer(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	} else if added {
		return ctrl.Result{Requeue: true}, nil
	}

	return r.reconcileNormal(ctx, cluster)
}

func (r *SdsElasticClusterReconciler) reconcileNormal(ctx context.Context, cluster *v1alpha1.SdsElasticCluster) (ctrl.Result, error) {
	status := newStatusBuilder(cluster)

	requeue := false

	storageDone, msg, err := r.ensureStorage(ctx, cluster)
	if err != nil {
		status.setCondition(v1alpha1.ConditionStorageReady, metav1.ConditionFalse, "Error", err.Error())
		return r.finishReconcile(ctx, cluster, status, false, err)
	}
	if !storageDone {
		status.setCondition(v1alpha1.ConditionStorageReady, metav1.ConditionFalse, "InProgress", msg)
		requeue = true
	} else {
		// Message is mode-specific (LVM vs raw devices), produced by
		// ensureStorage itself.
		status.setCondition(v1alpha1.ConditionStorageReady, metav1.ConditionTrue, "Ready", msg)
	}

	clusterDone, fsid, msg, err := r.ensureCephCluster(ctx, cluster)
	if err != nil {
		status.setCondition(v1alpha1.ConditionCephClusterReady, metav1.ConditionFalse, "Error", err.Error())
		return r.finishReconcile(ctx, cluster, status, false, err)
	}
	if !clusterDone {
		status.setCondition(v1alpha1.ConditionCephClusterReady, metav1.ConditionFalse, "InProgress", msg)
		requeue = true
	} else {
		status.setCondition(v1alpha1.ConditionCephClusterReady, metav1.ConditionTrue, "Ready", "CephCluster ready, rook-ceph-tools deployed")
		if fsid != "" {
			status.clusterID = fsid
		}
	}

	poolsDone, msg, err := r.ensureBlockPools(ctx, cluster)
	if err != nil {
		status.setCondition(v1alpha1.ConditionPoolsReady, metav1.ConditionFalse, "Error", err.Error())
		return r.finishReconcile(ctx, cluster, status, false, err)
	}
	if !poolsDone {
		status.setCondition(v1alpha1.ConditionPoolsReady, metav1.ConditionFalse, "InProgress", msg)
		requeue = true
	} else {
		status.setCondition(v1alpha1.ConditionPoolsReady, metav1.ConditionTrue, "Ready", "All CephBlockPools applied")
	}

	fsDone, msg, err := r.ensureFilesystems(ctx, cluster)
	if err != nil {
		status.setCondition(v1alpha1.ConditionFilesystemsReady, metav1.ConditionFalse, "Error", err.Error())
		return r.finishReconcile(ctx, cluster, status, false, err)
	}
	if !fsDone {
		status.setCondition(v1alpha1.ConditionFilesystemsReady, metav1.ConditionFalse, "InProgress", msg)
		requeue = true
	} else {
		status.setCondition(v1alpha1.ConditionFilesystemsReady, metav1.ConditionTrue, "Ready", "All CephFilesystems applied")
	}

	objDone, msg, err := r.ensureObjectStores(ctx, cluster)
	if err != nil {
		status.setCondition(v1alpha1.ConditionObjectStoresReady, metav1.ConditionFalse, "Error", err.Error())
		return r.finishReconcile(ctx, cluster, status, false, err)
	}
	if !objDone {
		status.setCondition(v1alpha1.ConditionObjectStoresReady, metav1.ConditionFalse, "InProgress", msg)
		requeue = true
	} else {
		status.setCondition(v1alpha1.ConditionObjectStoresReady, metav1.ConditionTrue, "Ready", "All CephObjectStores applied")
	}

	csiDone, msg, err := r.ensureCsiCephIntegration(ctx, cluster, status.clusterID)
	if err != nil {
		status.setCondition(v1alpha1.ConditionCsiCephReady, metav1.ConditionFalse, "Error", err.Error())
		return r.finishReconcile(ctx, cluster, status, false, err)
	}
	if !csiDone {
		status.setCondition(v1alpha1.ConditionCsiCephReady, metav1.ConditionFalse, "InProgress", msg)
		requeue = true
	} else {
		status.setCondition(v1alpha1.ConditionCsiCephReady, metav1.ConditionTrue, "Ready", "CephClusterConnection and CephStorageClasses applied")
	}

	allReady := storageDone && clusterDone && poolsDone && fsDone && objDone && csiDone
	if allReady {
		status.setCondition(v1alpha1.ConditionReady, metav1.ConditionTrue, "Ready", "All stages reconciled")
	} else {
		status.setCondition(v1alpha1.ConditionReady, metav1.ConditionFalse, "InProgress", "Reconcile in progress")
	}

	return r.finishReconcile(ctx, cluster, status, requeue, nil)
}

func (r *SdsElasticClusterReconciler) finishReconcile(ctx context.Context, cluster *v1alpha1.SdsElasticCluster, status *statusBuilder, requeue bool, reconcileErr error) (ctrl.Result, error) {
	if err := r.updateStatus(ctx, cluster, status); err != nil {
		r.Log.Error(err, "[finishReconcile] unable to update status")
		if reconcileErr == nil {
			reconcileErr = err
		}
	}
	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}
	if requeue {
		return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
	}
	return ctrl.Result{RequeueAfter: r.Cfg.RequeueInterval}, nil
}

// addFinalizer ensures our finalizer is on the CR. Returns true if the CR
// was updated.
func (r *SdsElasticClusterReconciler) addFinalizer(ctx context.Context, cluster *v1alpha1.SdsElasticCluster) (bool, error) {
	for _, f := range cluster.Finalizers {
		if f == v1alpha1.Finalizer {
			return false, nil
		}
	}
	cluster.Finalizers = append(cluster.Finalizers, v1alpha1.Finalizer)
	if err := r.Client.Update(ctx, cluster); err != nil {
		return false, fmt.Errorf("add finalizer: %w", err)
	}
	return true, nil
}

// removeFinalizer removes our finalizer from the CR.
func (r *SdsElasticClusterReconciler) removeFinalizer(ctx context.Context, cluster *v1alpha1.SdsElasticCluster) error {
	out := cluster.Finalizers[:0]
	changed := false
	for _, f := range cluster.Finalizers {
		if f == v1alpha1.Finalizer {
			changed = true
			continue
		}
		out = append(out, f)
	}
	if !changed {
		return nil
	}
	cluster.Finalizers = out
	return r.Client.Update(ctx, cluster)
}

// statusBuilder accumulates conditions / clusterID and writes them in one
// go via retry.RetryOnConflict.
type statusBuilder struct {
	source     *v1alpha1.SdsElasticCluster
	conditions []metav1.Condition
	clusterID  string
}

func newStatusBuilder(cluster *v1alpha1.SdsElasticCluster) *statusBuilder {
	sb := &statusBuilder{source: cluster}
	if cluster.Status != nil {
		sb.clusterID = cluster.Status.ClusterID
	}
	return sb
}

func (s *statusBuilder) setCondition(condType string, condStatus metav1.ConditionStatus, reason, message string) {
	s.conditions = append(s.conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: s.source.Generation,
		LastTransitionTime: metav1.NewTime(time.Now()),
	})
}

func (r *SdsElasticClusterReconciler) updateStatus(ctx context.Context, cluster *v1alpha1.SdsElasticCluster, sb *statusBuilder) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.SdsElasticCluster{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: cluster.Name}, latest); err != nil {
			return err
		}
		if latest.Status == nil {
			latest.Status = &v1alpha1.SdsElasticClusterStatus{}
		}
		before := latest.Status.DeepCopy()

		for _, cond := range sb.conditions {
			meta.SetStatusCondition(&latest.Status.Conditions, cond)
		}
		latest.Status.ObservedGeneration = cluster.Generation
		if sb.clusterID != "" {
			latest.Status.ClusterID = sb.clusterID
		}
		latest.Status.Phase = derivePhase(latest.Status.Conditions)

		if reflect.DeepEqual(before, latest.Status) {
			return nil
		}
		return r.Client.Status().Update(ctx, latest)
	})
}

// derivePhase converts the set of conditions into the coarse Phase.
func derivePhase(conditions []metav1.Condition) string {
	if len(conditions) == 0 {
		return v1alpha1.PhasePending
	}
	hasError := false
	hasFalse := false
	for _, c := range conditions {
		switch c.Status {
		case metav1.ConditionFalse:
			hasFalse = true
			if c.Reason == "Error" {
				hasError = true
			}
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
