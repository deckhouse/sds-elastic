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

package main

import (
	"context"
	"fmt"
	"os"
	goruntime "runtime"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	sv1 "k8s.io/api/storage/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/internal/controller"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/config"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/kubutils"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/logger"
)

var (
	buildDate = ""
	version   = ""
	commit    = ""
)

var resourcesSchemeFuncs = []func(*apiruntime.Scheme) error{
	v1alpha1.AddToScheme,
	clientgoscheme.AddToScheme,
	v1.AddToScheme,
	appsv1.AddToScheme,
	sv1.AddToScheme,
}

func main() {
	ctx := context.Background()
	cfgParams := config.NewConfig()

	log, err := logger.NewLogger(cfgParams.Loglevel)
	if err != nil {
		fmt.Printf("unable to create NewLogger, err: %v\n", err)
		os.Exit(1)
	}

	log.Info(fmt.Sprintf("[main] sds-elastic-controller version=%s commit=%s buildDate=%s", version, commit, buildDate))
	log.Info(fmt.Sprintf("[main] Go Version: %s", goruntime.Version()))
	log.Info(fmt.Sprintf("[main] OS/Arch: %s/%s", goruntime.GOOS, goruntime.GOARCH))
	log.Info(fmt.Sprintf("[main] ControllerNamespace=%s", cfgParams.ControllerNamespace))
	for _, ver := range v1alpha1.SupportedCephVersions {
		log.Info(fmt.Sprintf("[main] CephImages[%s]=%s", ver, cfgParams.CephImages[ver]))
	}
	log.Info(fmt.Sprintf("[main] MaxConcurrentReconciles=%d", cfgParams.MaxConcurrentReconciles))
	log.Info(fmt.Sprintf("[main] RequeueInterval=%s", cfgParams.RequeueInterval))

	kConfig, err := kubutils.KubernetesDefaultConfigCreate()
	if err != nil {
		log.Error(err, "[main] unable to create kubernetes config")
		os.Exit(1)
	}

	scheme := apiruntime.NewScheme()
	for _, f := range resourcesSchemeFuncs {
		if err := f(scheme); err != nil {
			log.Error(err, "[main] unable to register scheme")
			os.Exit(1)
		}
	}

	managerOpts := manager.Options{
		Scheme:                  scheme,
		HealthProbeBindAddress:  cfgParams.HealthProbeBindAddress,
		LeaderElection:          true,
		LeaderElectionNamespace: cfgParams.ControllerNamespace,
		LeaderElectionID:        config.DefaultControllerName,
		Logger:                  log.GetLogger(),
		// Scope the Secret/ConfigMap informers to the controller namespace.
		// Every Secret/ConfigMap this controller reads (the dataNodes config,
		// Rook's rook-ceph-mon Secret, rook-ceph-mon-endpoints ConfigMap)
		// lives in ControllerNamespace, so a cluster-wide cache for these
		// types would only inflate memory and API traffic. This is a
		// per-type scope (ByObject): cluster-wide objects (Node, BlockDevice,
		// ElasticCluster, CephCluster, PV, ...) keep their cluster-wide cache.
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&v1.Secret{}: {
					Namespaces: map[string]cache.Config{
						cfgParams.ControllerNamespace: {},
					},
				},
				&v1.ConfigMap{}: {
					Namespaces: map[string]cache.Config{
						cfgParams.ControllerNamespace: {},
					},
				},
			},
		},
	}

	mgr, err := manager.New(kConfig, managerOpts)
	if err != nil {
		log.Error(err, "[main] unable to create manager")
		os.Exit(1)
	}
	log.Info("[main] kubernetes manager created")

	if err := controller.AddElasticClusterReconcilerToManager(mgr, cfgParams, log); err != nil {
		log.Error(err, "[main] unable to register ElasticCluster reconciler")
		os.Exit(1)
	}

	if err := controller.AddElasticClusterCredentialReconcilerToManager(mgr, cfgParams, log); err != nil {
		log.Error(err, "[main] unable to register ElasticClusterCredential reconciler")
		os.Exit(1)
	}

	if err := controller.AddElasticStorageClassReconcilerToManager(mgr, cfgParams, log); err != nil {
		log.Error(err, "[main] unable to register ElasticStorageClass reconciler")
		os.Exit(1)
	}

	if err := controller.AddDataNodeWatcherReconcilerToManager(mgr, cfgParams, log); err != nil {
		log.Error(err, "[main] unable to register DataNodeWatcher reconciler")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "[main] unable to AddHealthzCheck")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "[main] unable to AddReadyzCheck")
		os.Exit(1)
	}

	log.Info("[main] starting manager")
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "[main] manager exited with error")
		os.Exit(1)
	}
}
