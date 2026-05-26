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
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/sirupsen/logrus"
	kwhlogrus "github.com/slok/kubewebhook/v2/pkg/log/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/deckhouse/sds-elastic/images/webhooks/handlers"
)

type config struct {
	certFile string
	keyFile  string
}

func httpHandlerHealthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = fmt.Fprint(w, "Ok.")
}

func initFlags() (config, error) {
	cfg := config{}

	fl := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fl.StringVar(&cfg.certFile, "tls-cert-file", "", "TLS certificate file")
	fl.StringVar(&cfg.keyFile, "tls-key-file", "", "TLS key file")

	err := fl.Parse(os.Args[1:])
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}

const (
	port                = ":8443"
	VendorCRValidatorID = "VendorCRValidator"

	ElasticStorageClassValidatorID      = "ElasticStorageClassValidator"
	ElasticClusterCredentialValidatorID = "ElasticClusterCredentialValidator"
	ElasticClusterValidatorID           = "ElasticClusterValidator"
)

func main() {
	logrusLogEntry := logrus.NewEntry(logrus.New())
	logrusLogEntry.Logger.SetLevel(logrus.DebugLevel)
	logger := kwhlogrus.NewLogrus(logrusLogEntry)

	cfg, err := initFlags()
	if err != nil {
		fmt.Printf("unable to parse config: err: %s", err.Error())
		os.Exit(1)
	}

	// EC and ESC validators both need cluster-wide read access to
	// ElasticCluster, BlockDevice, and Node CRs to enforce their
	// admission contracts (orphan-guard / pre-flight conflict for EC,
	// HighRedundancy preflight for ESC). The webhook is co-deployed
	// with the controller and shares its ServiceAccount, so
	// InClusterConfig is the only path supported here — fail fast at
	// startup if it is not available.
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building in-cluster config for validators: %s", err)
		os.Exit(1)
	}
	dynClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building dynamic client for validators: %s", err)
		os.Exit(1)
	}

	vendorCRValidatingWebhookHandler, err := handlers.GetValidatingWebhookHandler(
		handlers.VendorCRValidate,
		VendorCRValidatorID,
		&unstructured.Unstructured{},
		logger,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating vendorCRValidatingWebhookHandler: %s", err)
		os.Exit(1)
	}

	escValidatingWebhookHandler, err := handlers.GetValidatingWebhookHandler(
		handlers.NewElasticStorageClassValidator(dynClient),
		ElasticStorageClassValidatorID,
		&unstructured.Unstructured{},
		logger,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating escValidatingWebhookHandler: %s", err)
		os.Exit(1)
	}

	eccValidatingWebhookHandler, err := handlers.GetValidatingWebhookHandler(
		handlers.ElasticClusterCredentialValidate,
		ElasticClusterCredentialValidatorID,
		&unstructured.Unstructured{},
		logger,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating eccValidatingWebhookHandler: %s", err)
		os.Exit(1)
	}

	ecValidatingWebhookHandler, err := handlers.GetValidatingWebhookHandler(
		handlers.NewElasticClusterValidator(dynClient),
		ElasticClusterValidatorID,
		&unstructured.Unstructured{},
		logger,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating ecValidatingWebhookHandler: %s", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/vendor-cr-validate", vendorCRValidatingWebhookHandler)
	mux.Handle("/esc-validate", escValidatingWebhookHandler)
	mux.Handle("/ecc-validate", eccValidatingWebhookHandler)
	mux.Handle("/ec-validate", ecValidatingWebhookHandler)
	mux.HandleFunc("/healthz", httpHandlerHealthz)

	logger.Infof("Listening on %s", port)
	err = http.ListenAndServeTLS(port, cfg.certFile, cfg.keyFile, mux)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error serving webhook: %s", err)
		os.Exit(1)
	}
}
