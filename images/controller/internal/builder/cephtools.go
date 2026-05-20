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

package builder

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1alpha1 "github.com/deckhouse/sds-elastic/api/v1alpha1"
)

// CephToolsDeploymentName is the well-known name of the rook-ceph-tools
// Deployment created in the module namespace.
const CephToolsDeploymentName = "rook-ceph-tools"

// CephToolsDeployment builds the Deployment from instruction.md
// "Создаём под ceph-tools".
func CephToolsDeployment(cluster *v1alpha1.SdsElasticCluster, namespace, registrySecretName string) *appsv1.Deployment {
	cephVersion := defaultIfEmpty(cluster.Spec.CephVersion, "v18.2.7")
	image := fmt.Sprintf("quay.io/ceph/ceph:%s", cephVersion)

	labels := map[string]string{
		"app": "ceph-tools",
	}
	for k, v := range ManagedLabels(cluster.Name) {
		labels[k] = v
	}

	replicas := int32(1)
	defaultMode := int32(420)
	runAsUser := int64(167)
	runAsGroup := int64(167)
	runAsNonRoot := true

	configureScript := `set -euo pipefail
cat << EOF > /etc/ceph/ceph.conf
[global]
mon_host = $(sed 's/[a-z]=//g' /etc/rook/mon-endpoints)
EOF
cat << EOF > /etc/ceph/ceph.client.admin.keyring
[$ROOK_CEPH_USERNAME]
key = $ROOK_CEPH_SECRET
EOF
`

	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      CephToolsDeploymentName,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "ceph-tools"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:    &runAsUser,
						RunAsGroup:   &runAsGroup,
						RunAsNonRoot: &runAsNonRoot,
					},
					InitContainers: []corev1.Container{
						{
							Name:    "configure",
							Image:   image,
							Command: []string{"/bin/bash", "-c", configureScript},
							Env: []corev1.EnvVar{
								{
									Name: "ROOK_CEPH_USERNAME",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: "rook-ceph-mon"},
											Key:                  "ceph-username",
										},
									},
								},
								{
									Name: "ROOK_CEPH_SECRET",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: "rook-ceph-mon"},
											Key:                  "ceph-secret",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "ceph-config", MountPath: "/etc/ceph"},
								{Name: "mon-endpoint-volume", MountPath: "/etc/rook"},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:                     "ceph-tools",
							Image:                    image,
							Command:                  []string{"sleep", "infinity"},
							TerminationMessagePath:   "/dev/termination-log",
							TerminationMessagePolicy: corev1.TerminationMessageReadFile,
							TTY:                      true,
							WorkingDir:               "/var/lib/ceph",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "ceph-config", MountPath: "/etc/ceph"},
								{Name: "homedir", MountPath: "/var/lib/ceph"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "mon-endpoint-volume",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									DefaultMode: &defaultMode,
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "rook-ceph-mon-endpoints",
									},
									Items: []corev1.KeyToPath{
										{Key: "data", Path: "mon-endpoints"},
									},
								},
							},
						},
						{Name: "ceph-config", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "homedir", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}

	if registrySecretName != "" {
		d.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: registrySecretName},
		}
	}

	return d
}

// MaxSurgeIntOrStringOne is kept for completeness; not used right now but
// useful if rolling update strategies are needed later.
var _ = intstr.FromInt32(1)
