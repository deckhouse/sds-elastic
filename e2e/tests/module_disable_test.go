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

package tests

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	storagekube "github.com/deckhouse/storage-e2e/pkg/kubernetes"
)

// moduleDisableBlockedSpec registers the non-destructive disable-guard spec. It
// is built BEFORE deleteSpecs so it runs while the shared ElasticCluster is
// still alive. Requires the C3 webhook in the module image under test.
func moduleDisableBlockedSpec() {
	It("denies disabling the sds-elastic module without the force annotation while an ElasticCluster exists", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		By("Confirming the shared ElasticCluster exists to arm the disable guard")
		_, err := suiteDyn.Resource(storagekube.ElasticClusterGVR).Get(ctx, suiteCfg.ecName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "shared ElasticCluster %s must exist", suiteCfg.ecName)

		By("Granting Deckhouse's own disable confirmation so only the sds-elastic guard is exercised")
		Expect(allowDeckhouseDisabling(ctx, moduleConfigName)).To(Succeed())

		By("Patching spec.enabled=false WITHOUT the force annotation and expecting an admission denial")
		Expect(expectModuleConfigDenied(ctx, moduleConfigName, false)).To(Succeed())

		By("Confirming the ModuleConfig stays enabled")
		Consistently(func(g Gomega) {
			enabled, err := moduleConfigEnabled(ctx, moduleConfigName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(enabled).To(BeTrue(), "ModuleConfig %s must remain enabled after the denied patch", moduleConfigName)
		}, 30*time.Second, pollInterval).Should(Succeed())
	})
}

// moduleDisableForceSpec registers the terminal force-disable spec. It runs
// after deleteSpecs (the shared EC is gone), arms the guard with a bare EC, and
// then force-disables the module. This destroys the module under test, so it
// must be the last module-level spec.
func moduleDisableForceSpec() {
	It("force-disables the module via the force annotation and uninstalls it (resources/VWC/namespace removed)", func() {
		ctx, cancel := context.WithTimeout(context.Background(), moduleGoneTimeout+10*time.Minute)
		defer cancel()

		By("Arming the disable guard with a bare ElasticCluster (no wait for Ready)")
		bareEC := suiteCfg.ecName + "-bare"
		Expect(applyElasticClusterNoWait(ctx, bareEC)).To(Succeed())

		By("Confirming disable is still denied without the force annotation")
		Expect(expectModuleConfigDenied(ctx, moduleConfigName, false)).To(Succeed())

		By("Setting the force-disable annotation, then disabling the module")
		// Deckhouse's allow-disabling annotation is already set by the blocked spec;
		// re-apply defensively so this spec is robust if run in isolation.
		Expect(allowDeckhouseDisabling(ctx, moduleConfigName)).To(Succeed())
		Expect(setForceDisableAnnotation(ctx, moduleConfigName)).To(Succeed())
		Expect(patchModuleConfigEnabled(ctx, moduleConfigName, false)).To(Succeed())

		By("Waiting for the module to uninstall (d8-sds-elastic namespace + mc-validation webhook removed)")
		Expect(waitModuleUninstalled(ctx, moduleGoneTimeout)).To(Succeed())
	})
}

// moduleConfigEnabled reads spec.enabled of a ModuleConfig.
func moduleConfigEnabled(ctx context.Context, name string) (bool, error) {
	obj, err := suiteDyn.Resource(moduleConfigGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	enabled, found, err := unstructured.NestedBool(obj.Object, "spec", "enabled")
	if err != nil {
		return false, err
	}
	if !found {
		return false, fmt.Errorf("ModuleConfig %s has no spec.enabled", name)
	}
	return enabled, nil
}
