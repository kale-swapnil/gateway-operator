package crdsvalidation

import (
	"testing"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kong/gateway-operator/modules/manager/scheme"
	"github.com/kong/gateway-operator/test/envtest"

	operatorv1beta1 "github.com/kong/kubernetes-configuration/api/gateway-operator/v1beta1"
	"github.com/kong/kubernetes-configuration/test/crdsvalidation/common"
)

func TestControlPlane(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cfg, ns := envtest.Setup(t, ctx, scheme.Get())

	t.Run("spec", func(t *testing.T) {
		common.TestCasesGroup[*operatorv1beta1.ControlPlane]{
			{
				Name: "not providing image fails",
				TestObject: &operatorv1beta1.ControlPlane{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "cp-",
						Namespace:    ns.Name,
					},
					Spec: operatorv1beta1.ControlPlaneSpec{},
				},
				ExpectedErrorEventuallyConfig: sharedEventuallyConfig,
				ExpectedErrorMessage:          lo.ToPtr("ControlPlane requires an image to be set on controller container"),
			},
			{
				Name: "providing image succeeds",
				TestObject: &operatorv1beta1.ControlPlane{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: "cp-",
						Namespace:    ns.Name,
					},
					Spec: operatorv1beta1.ControlPlaneSpec{
						ControlPlaneOptions: operatorv1beta1.ControlPlaneOptions{
							Deployment: operatorv1beta1.ControlPlaneDeploymentOptions{
								PodTemplateSpec: &corev1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{
												Name:  "controller",
												Image: "kong/kubernetes-ingress-controller:3.4.1",
											},
										},
									},
								},
							},
						},
					},
				},
				ExpectedErrorEventuallyConfig: sharedEventuallyConfig,
			},
		}.RunWithConfig(t, cfg, scheme.Get())
	})
}
