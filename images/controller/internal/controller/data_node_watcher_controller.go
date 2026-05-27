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
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"sigs.k8s.io/yaml"

	"github.com/deckhouse/sds-elastic/images/controller/pkg/config"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/logger"
)

const (
	// DataNodeWatcherCtrl is the name registered with the controller-runtime
	// manager. Used as the leader-election sub-key and shows up in logs.
	DataNodeWatcherCtrl = "data-node-watcher-controller"

	// DataNodeSelectorLabel marks a Kubernetes Node as eligible to host
	// sds-elastic data. The label is set to empty string ("") so it can be
	// matched with `In: [""]` node-affinity terms by downstream consumers
	// (sds-node-configurator agent, ElasticCluster placement) without
	// committing to a particular value semantics.
	//
	// Pattern mirrors sds-replicated-volume
	// (`storage.deckhouse.io/sds-replicated-volume-node`) and
	// sds-local-volume (`storage.deckhouse.io/sds-local-volume-node`).
	DataNodeSelectorLabel = "storage.deckhouse.io/sds-elastic-node"
)

// DataNodeWatcherReconciler reconciles cluster Nodes against the operator-
// supplied `settings.dataNodes.nodeSelector` carried by the
// `d8-sds-elastic-controller-config` Secret. On every reconcile it:
//
//  1. Reads the latest selector from the Secret.
//  2. Lists Nodes matching the selector and adds DataNodeSelectorLabel to
//     each Node that does not have it yet.
//  3. Lists all Nodes and removes DataNodeSelectorLabel from every Node
//     that has the label but no longer matches the selector.
//
// The reconciler is watched-Secret-only (no `For()` primary resource): the
// data flow is selector-in / Node-out, and Node changes do not require a
// re-reconcile (any external label drift is corrected on the periodic
// requeue or on the next Secret write).
type DataNodeWatcherReconciler struct {
	Client client.Client
	Log    *logger.Logger
	Cfg    *config.Options
}

// AddDataNodeWatcherReconcilerToManager registers the reconciler with the
// manager. Only one Watch is wired (Secrets cluster-wide), filtered down to
// the single config Secret name by the reconciler itself; this is cheaper
// than a label predicate at controller-runtime level because the cluster
// already has very few Secrets in `d8-sds-elastic` and we avoid hot-path
// label lookups.
func AddDataNodeWatcherReconcilerToManager(mgr manager.Manager, cfg *config.Options, log *logger.Logger) error {
	r := &DataNodeWatcherReconciler{
		Client: mgr.GetClient(),
		Log:    log,
		Cfg:    cfg,
	}

	c, err := controller.New(DataNodeWatcherCtrl, mgr, controller.Options{
		MaxConcurrentReconciles: cfg.MaxConcurrentReconciles,
		Reconciler:              r,
	})
	if err != nil {
		return fmt.Errorf("create %s: %w", DataNodeWatcherCtrl, err)
	}

	if err := c.Watch(source.Kind(mgr.GetCache(), &corev1.Secret{}, &handler.TypedEnqueueRequestForObject[*corev1.Secret]{})); err != nil {
		return fmt.Errorf("watch Secrets for %s: %w", DataNodeWatcherCtrl, err)
	}

	return nil
}

// Reconcile filters out unrelated Secret events and routes the config
// Secret through reconcileDataNodes. The periodic RequeueAfter guards
// against external label drift (a Node operator manually removing the
// label, an admin re-labelling a Node back into the selector range, etc.).
func (r *DataNodeWatcherReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	if req.Name != r.Cfg.ConfigSecretName {
		return reconcile.Result{}, nil
	}
	if req.Namespace != r.Cfg.ControllerNamespace {
		return reconcile.Result{}, nil
	}

	r.Log.Debug(fmt.Sprintf("[DataNodeWatcher] reconcile triggered by Secret %s/%s", req.Namespace, req.Name))

	secret := &corev1.Secret{}
	if err := r.Client.Get(ctx, req.NamespacedName, secret); err != nil {
		if apierrors.IsNotFound(err) {
			// Secret is delivered by Helm and recreated on every install /
			// upgrade. If it is genuinely missing the operator has not
			// reconciled the module yet — skip without requeue so we do
			// not pin a busy loop on a missing dependency.
			r.Log.Warning(fmt.Sprintf("[DataNodeWatcher] config Secret %s/%s not found; skipping until next event", req.Namespace, req.Name))
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("get config Secret %s/%s: %w", req.Namespace, req.Name, err)
	}

	if err := r.reconcileDataNodes(ctx, secret); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: r.Cfg.RequeueSecretInterval}, nil
}

func (r *DataNodeWatcherReconciler) reconcileDataNodes(ctx context.Context, secret *corev1.Secret) error {
	selector, err := nodeSelectorFromSecret(secret)
	if err != nil {
		return fmt.Errorf("parse %s/%s: %w", secret.Namespace, secret.Name, err)
	}
	r.Log.Debug(fmt.Sprintf("[DataNodeWatcher] target nodeSelector=%v", selector))

	matching, err := r.listNodesBySelector(ctx, selector)
	if err != nil {
		return fmt.Errorf("list matching nodes: %w", err)
	}
	if err := r.addLabelOnMatchingNodes(ctx, matching); err != nil {
		return err
	}

	all, err := r.listAllNodes(ctx)
	if err != nil {
		return fmt.Errorf("list all nodes: %w", err)
	}
	return r.removeLabelFromStaleNodes(ctx, all, selector)
}

// nodeSelectorFromSecret extracts SdsElasticConfig.NodeSelector from the
// `config` data key. A nil map (returned for a malformed or empty payload
// with no `nodeSelector` key) is normalised to an empty map so callers can
// compose `labels.Set(selector).AsSelector().Matches(...)` without a nil
// check; the empty selector matches every Node.
func nodeSelectorFromSecret(secret *corev1.Secret) (map[string]string, error) {
	raw, ok := secret.Data["config"]
	if !ok || len(raw) == 0 {
		return map[string]string{}, nil
	}
	var sdsConfig config.SdsElasticConfig
	if err := yaml.Unmarshal(raw, &sdsConfig); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	if sdsConfig.NodeSelector == nil {
		return map[string]string{}, nil
	}
	return sdsConfig.NodeSelector, nil
}

func (r *DataNodeWatcherReconciler) listNodesBySelector(ctx context.Context, selector map[string]string) (*corev1.NodeList, error) {
	nodes := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodes, client.MatchingLabels(selector)); err != nil {
		return nil, err
	}
	return nodes, nil
}

func (r *DataNodeWatcherReconciler) listAllNodes(ctx context.Context) (*corev1.NodeList, error) {
	nodes := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

func (r *DataNodeWatcherReconciler) addLabelOnMatchingNodes(ctx context.Context, nodes *corev1.NodeList) error {
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if _, ok := node.Labels[DataNodeSelectorLabel]; ok {
			continue
		}
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels[DataNodeSelectorLabel] = ""
		if err := r.Client.Update(ctx, node); err != nil {
			r.Log.Error(err, fmt.Sprintf("[DataNodeWatcher] add label on Node %s", node.Name))
			continue
		}
		r.Log.Info(fmt.Sprintf("[DataNodeWatcher] labelled Node %s with %s", node.Name, DataNodeSelectorLabel))
	}
	return nil
}

func (r *DataNodeWatcherReconciler) removeLabelFromStaleNodes(ctx context.Context, nodes *corev1.NodeList, selector map[string]string) error {
	sel := labels.Set(selector).AsSelector()
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if _, hasLabel := node.Labels[DataNodeSelectorLabel]; !hasLabel {
			continue
		}
		if sel.Matches(labels.Set(node.Labels)) {
			continue
		}
		delete(node.Labels, DataNodeSelectorLabel)
		if err := r.Client.Update(ctx, node); err != nil {
			r.Log.Error(err, fmt.Sprintf("[DataNodeWatcher] remove label on Node %s", node.Name))
			continue
		}
		r.Log.Info(fmt.Sprintf("[DataNodeWatcher] removed %s from Node %s (no longer matches selector)", DataNodeSelectorLabel, node.Name))
	}
	return nil
}
