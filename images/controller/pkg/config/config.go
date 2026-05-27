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

package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
	"github.com/deckhouse/sds-elastic/images/controller/pkg/logger"
)

const (
	LogLevelEnv                = "LOG_LEVEL"
	ControllerNamespaceEnv     = "CONTROLLER_NAMESPACE"
	HealthProbeBindAddressEnv  = "HEALTH_PROBE_BIND_ADDRESS"
	CephImagesEnv              = "CEPH_IMAGES"
	MaxConcurrentReconcilesEnv = "MAX_CONCURRENT_RECONCILES"
	RequeueIntervalEnv         = "REQUEUE_INTERVAL_SECONDS"

	DefaultControllerNamespace     = "d8-sds-elastic"
	DefaultControllerName          = "sds-elastic-controller"
	DefaultHealthProbeBindAddress  = ":8081"
	DefaultRequeueIntervalSeconds  = 30
	DefaultMaxConcurrentReconciles = 1

	// ConfigSecretName is the name of the Secret rendered by
	// templates/controller/secret.yaml that carries
	// `.Values.sdsElastic.dataNodes.nodeSelector`. The data-node-watcher
	// reconciler filters incoming reconcile requests by this name so the
	// rest of the cluster's Secret traffic does not wake it up.
	ConfigSecretName = "d8-sds-elastic-controller-config"

	// DefaultRequeueSecretIntervalSeconds is the periodic requeue used by
	// the data-node-watcher reconciler after a successful pass. The
	// reconciler is also triggered immediately on Secret writes, so this
	// interval is the upper bound on convergence when the cluster state
	// drifts without a corresponding Secret event (e.g. a Node having
	// matching labels added or removed by another operator).
	DefaultRequeueSecretIntervalSeconds = 10
)

type Options struct {
	Loglevel                logger.Verbosity
	HealthProbeBindAddress  string
	ControllerNamespace     string
	CephImages              map[string]string
	MaxConcurrentReconciles int
	RequeueInterval         time.Duration

	// RequeueSecretInterval is the periodic requeue applied by the
	// data-node-watcher controller. Defaults to
	// DefaultRequeueSecretIntervalSeconds.
	RequeueSecretInterval time.Duration

	// ConfigSecretName is the name of the controller-config Secret in
	// ControllerNamespace. Exposed as a field (instead of a bare const)
	// so tests can override it.
	ConfigSecretName string
}

// SdsElasticConfig is the on-the-wire YAML payload carried by the
// controller-config Secret (key `config`). The Secret is rendered by
// templates/controller/secret.yaml from
// `.Values.sdsElastic.dataNodes.nodeSelector`.
type SdsElasticConfig struct {
	NodeSelector map[string]string `yaml:"nodeSelector"`
}

func NewConfig() *Options {
	var opts Options

	if v := os.Getenv(LogLevelEnv); v != "" {
		opts.Loglevel = logger.Verbosity(v)
	} else {
		opts.Loglevel = logger.DebugLevel
	}

	if v := os.Getenv(HealthProbeBindAddressEnv); v != "" {
		opts.HealthProbeBindAddress = v
	} else {
		opts.HealthProbeBindAddress = DefaultHealthProbeBindAddress
	}

	opts.ControllerNamespace = os.Getenv(ControllerNamespaceEnv)
	if opts.ControllerNamespace == "" {
		if ns, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			opts.ControllerNamespace = string(ns)
		} else {
			log.Printf("Failed to read namespace from filesystem: %v; falling back to %q", err, DefaultControllerNamespace)
			opts.ControllerNamespace = DefaultControllerNamespace
		}
	}

	opts.MaxConcurrentReconciles = DefaultMaxConcurrentReconciles
	if v := os.Getenv(MaxConcurrentReconcilesEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.MaxConcurrentReconciles = n
		}
	}

	opts.RequeueInterval = time.Duration(DefaultRequeueIntervalSeconds) * time.Second
	if v := os.Getenv(RequeueIntervalEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.RequeueInterval = time.Duration(n) * time.Second
		}
	}

	opts.RequeueSecretInterval = time.Duration(DefaultRequeueSecretIntervalSeconds) * time.Second
	opts.ConfigSecretName = ConfigSecretName

	cephImages, err := loadCephImages()
	if err != nil {
		log.Fatalf("invalid %s: %v", CephImagesEnv, err)
	}
	opts.CephImages = cephImages

	return &opts
}

func loadCephImages() (map[string]string, error) {
	raw := strings.TrimSpace(os.Getenv(CephImagesEnv))
	if raw == "" {
		return nil, fmt.Errorf("environment variable is empty")
	}

	var images map[string]string
	if err := json.Unmarshal([]byte(raw), &images); err != nil {
		return nil, fmt.Errorf("JSON decode: %w", err)
	}

	for _, ver := range v1alpha1.SupportedCephVersions {
		if images[ver] == "" {
			return nil, fmt.Errorf("missing image reference for version %q", ver)
		}
	}

	return images, nil
}
