package konnect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	sdkkonnectcomp "github.com/Kong/sdk-konnect-go/models/components"
	"github.com/google/go-cmp/cmp"
	"github.com/samber/lo"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	extensionserrors "github.com/kong/gateway-operator/controller/pkg/extensions/errors"
	"github.com/kong/gateway-operator/controller/pkg/op"
	"github.com/kong/gateway-operator/controller/pkg/patch"
	"github.com/kong/gateway-operator/controller/pkg/secrets"
	"github.com/kong/gateway-operator/internal/utils/index"
	"github.com/kong/gateway-operator/pkg/consts"
	k8sutils "github.com/kong/gateway-operator/pkg/utils/kubernetes"

	commonv1alpha1 "github.com/kong/kubernetes-configuration/api/common/v1alpha1"
	operatorv1beta1 "github.com/kong/kubernetes-configuration/api/gateway-operator/v1beta1"
	"github.com/kong/kubernetes-configuration/api/konnect"
	konnectv1alpha1 "github.com/kong/kubernetes-configuration/api/konnect/v1alpha1"
)

// getGatewayKonnectControlPlane retrieves the Konnect Control Plane from K8s cluster
// based on the provided KonnectExtension specification.
// It supports two types of ControlPlaneRef: KonnectNamespacedRef and KonnectID.
//
// Returns:
// - cp: The retrieved Konnect Control Plane.
// - res: The result of the controller reconciliation.
// - err: An error if the retrieval fails.
func (r *KonnectExtensionReconciler) getGatewayKonnectControlPlane(
	ctx context.Context,
	ext konnectv1alpha1.KonnectExtension,
	dependingConditions ...metav1.Condition,
) (cp *konnectv1alpha1.KonnectGatewayControlPlane, res ctrl.Result, err error) {
	// Get respective KonnectGatewayControlPlane from K8s cluster.
	var errGetFromK8s error
	switch ext.Spec.Konnect.ControlPlane.Ref.Type {
	case commonv1alpha1.ControlPlaneRefKonnectNamespacedRef:
		// TODO: get namespace from cpRef.Namespace when allowed to reference CP from another namespace.
		cpNN := client.ObjectKey{
			Name:      ext.Spec.Konnect.ControlPlane.Ref.KonnectNamespacedRef.Name,
			Namespace: ext.Namespace,
		}
		kgcp := &konnectv1alpha1.KonnectGatewayControlPlane{}
		// Set the controlPlaneRefValidCond to false in case the KonnectGatewayControlPlane is not found.
		if err := r.Get(ctx, cpNN, kgcp); err != nil {
			if k8serrors.IsNotFound(err) {
				errGetFromK8s = err
			} else {
				return nil, ctrl.Result{}, err
			}
		}
		cp = kgcp
	case commonv1alpha1.ControlPlaneRefKonnectID:
		kgcpList := &konnectv1alpha1.KonnectGatewayControlPlaneList{}
		if err := r.List(ctx, kgcpList, client.InNamespace(ext.Namespace), client.MatchingFields{
			index.IndexFieldKonnectGatewayControlPlaneOnKonnectID: string(*ext.Spec.Konnect.ControlPlane.Ref.KonnectID),
		}); err != nil {
			return nil, ctrl.Result{}, err
		}
		kgcps := kgcpList.Items
		switch l := len(kgcps); l {
		case 0:
			errGetFromK8s = k8serrors.NewNotFound(
				konnectv1alpha1.Resource("KonnectGatewayControlPlane"),
				fmt.Sprintf("with KonnectID %s in namespace %s",
					*ext.Spec.Konnect.ControlPlane.Ref.KonnectID, ext.Namespace,
				),
			)
		default:
			cp = &kgcps[0]
		}
	}

	controlPlaneRefValidCond := metav1.Condition{
		Type:    konnectv1alpha1.ControlPlaneRefValidConditionType,
		Status:  metav1.ConditionTrue,
		Reason:  konnectv1alpha1.ControlPlaneRefReasonValid,
		Message: "ControlPlaneRef is valid",
	}

	// Check if the KonnectGatewayControlPlane has been found.
	if errGetFromK8s != nil {
		controlPlaneRefValidCond.Status = metav1.ConditionFalse
		controlPlaneRefValidCond.Reason = konnectv1alpha1.ControlPlaneRefReasonInvalid
		controlPlaneRefValidCond.Message = errGetFromK8s.Error()
		if res, _, errPatch := patch.StatusWithConditions(
			ctx,
			r.Client,
			&ext,
			append(dependingConditions, controlPlaneRefValidCond)...,
		); errPatch != nil || !res.IsZero() {
			return nil, res, errPatch
		}
		return nil, ctrl.Result{}, errGetFromK8s
	}

	// Set the controlPlaneRefValidCond to false in case the KonnectGatewayControlPlane is not programmed yet.
	if !k8sutils.HasConditionTrue(konnectv1alpha1.KonnectEntityProgrammedConditionType, cp) {
		controlPlaneRefValidCond.Status = metav1.ConditionFalse
		controlPlaneRefValidCond.Reason = konnectv1alpha1.ControlPlaneRefReasonInvalid
		controlPlaneRefValidCond.Message = fmt.Sprintf("Konnect control plane %s/%s not programmed yet", cp.Name, cp.Namespace)
		if res, _, errPatch := patch.StatusWithConditions(
			ctx,
			r.Client,
			&ext,
			append(dependingConditions, controlPlaneRefValidCond)...,
		); errPatch != nil || !res.IsZero() {
			return nil, res, errPatch
		}
		return nil, ctrl.Result{}, extensionserrors.ErrKonnectGatewayControlPlaneNotProgrammed
	}

	// Set the controlPlaneRefValidCond to true in case the ControlPlane is configured properly.
	if res, _, errPatch := patch.StatusWithConditions(
		ctx,
		r.Client,
		&ext,
		controlPlaneRefValidCond,
	); errPatch != nil || !res.IsZero() {
		return nil, res, errPatch
	}

	return cp, ctrl.Result{}, nil
}

// ensureExtendablesReferencesInStatus ensures that the KonnectExtension references to DataPlane and ControlPlane are up-to-date.
// Only DataPlanes and ControlPlanes with the condition KonnectExtensionApplied=True are added to the status.
func (r *KonnectExtensionReconciler) ensureExtendablesReferencesInStatus(
	ctx context.Context,
	ext *konnectv1alpha1.KonnectExtension,
	dps operatorv1beta1.DataPlaneList,
	cps operatorv1beta1.ControlPlaneList,
) (ctrl.Result, error) {
	sortRefs := func(refs []commonv1alpha1.NamespacedRef) {
		refToStr := func(ref commonv1alpha1.NamespacedRef) string {
			// We can safely assume that the namespace is not nil, as we fill it when mapping refs.
			return fmt.Sprintf("%s/%s", *ref.Namespace, ref.Name)
		}
		sort.Slice(refs, func(i, j int) bool {
			return refToStr(refs[i]) < refToStr(refs[j])
		})
	}
	hasExtensionAppliedCondition := func(conditions []metav1.Condition) bool {
		return lo.ContainsBy(conditions, func(cond metav1.Condition) bool {
			return cond.Type == string(konnect.KonnectExtensionAppliedType) &&
				cond.Status == metav1.ConditionTrue
		})
	}

	extOld := ext.DeepCopy()

	// Ensure DataPlaneRefs are up-to-date.
	var dpRefs []commonv1alpha1.NamespacedRef
	for _, dp := range dps.Items {
		// Only add DataPlanes with the KonnectExtensionApplied condition set to true.
		if !hasExtensionAppliedCondition(dp.Status.Conditions) {
			continue
		}
		dpRefs = append(dpRefs, commonv1alpha1.NamespacedRef{
			Name:      dp.Name,
			Namespace: &dp.Namespace,
		})
	}
	sortRefs(dpRefs)
	ext.Status.DataPlaneRefs = dpRefs

	// Ensure ControlPlaneRefs are up-to-date.
	var cpRefs []commonv1alpha1.NamespacedRef
	for _, cp := range cps.Items {
		// Only add ControlPlanes with the KonnectExtensionApplied condition set to true.
		if !hasExtensionAppliedCondition(cp.Status.Conditions) {
			continue
		}
		cpRefs = append(cpRefs, commonv1alpha1.NamespacedRef{
			Name:      cp.Name,
			Namespace: &cp.Namespace,
		})
	}
	sortRefs(cpRefs)
	ext.Status.ControlPlaneRefs = cpRefs

	if shouldUpdate := !cmp.Equal(ext.Status, extOld.Status); !shouldUpdate {
		return ctrl.Result{}, nil
	}

	if err := r.Client.Status().Update(ctx, ext); err != nil {
		if k8serrors.IsConflict(err) {
			// Gracefully requeue in case of conflict.
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to update KonnectExtension ControlPlane and DataPlane references in status: %w", err)
	}
	return ctrl.Result{Requeue: true}, nil
}

func getKonnectAPIAuthRefNN(ctx context.Context, cl client.Client, ext *konnectv1alpha1.KonnectExtension) (types.NamespacedName, error) {
	if ext.Spec.Konnect.Configuration != nil {
		// TODO: handle cross namespace refs when supported.
		return types.NamespacedName{
			Namespace: ext.Namespace,
			Name:      ext.Spec.Konnect.Configuration.APIAuthConfigurationRef.Name,
		}, nil
	}

	// In case the KonnectConfiguration is not set, we fetch the KonnectGatewayControlPlane
	// and get the KonnectConfiguration from `spec.konnectControlPlane.controlPlane.konnectNamespacedRef`.
	// KonnectGatewayControlPlane reference and KonnectConfiguration
	// are mutually exclusive in the KonnectExtension API.
	cpRef := ext.Spec.Konnect.ControlPlane.Ref.KonnectNamespacedRef
	kgcp := &konnectv1alpha1.KonnectGatewayControlPlane{}
	err := cl.Get(ctx, client.ObjectKey{
		// TODO: handle cross namespace refs to KonnectGatewayControlPlane when referencing CP from another namespace is supported.
		Namespace: ext.Namespace,
		Name:      cpRef.Name,
	}, kgcp)
	if err != nil {
		return types.NamespacedName{}, err
	}
	return types.NamespacedName{
		Namespace: kgcp.Namespace,
		Name:      kgcp.Spec.KonnectConfiguration.APIAuthConfigurationRef.Name,
	}, nil
}

func (r *KonnectExtensionReconciler) ensureCertificateSecret(ctx context.Context, ext *konnectv1alpha1.KonnectExtension) (op.Result, *corev1.Secret, error) {
	usages := []certificatesv1.KeyUsage{
		certificatesv1.UsageKeyEncipherment,
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageClientAuth,
	}
	matchingLabels := client.MatchingLabels{
		consts.SecretProvisioningLabelKey:      consts.SecretProvisioningAutomaticLabelValue,
		SecretKonnectDataPlaneCertificateLabel: "true",
	}
	return secrets.EnsureCertificate(ctx,
		ext,
		fmt.Sprintf("%s.%s", ext.Name, ext.Namespace),
		types.NamespacedName{
			Namespace: r.ClusterCASecretNamespace,
			Name:      r.ClusterCASecretName,
		},
		usages,
		r.ClusterCAKeyConfig,
		r.Client,
		matchingLabels,
	)
}

func (r *KonnectExtensionReconciler) getCertificateSecret(ctx context.Context, ext konnectv1alpha1.KonnectExtension, cleanup bool) (op.Result, *corev1.Secret, error) {
	var (
		certificateSecret = &corev1.Secret{}
		err               error
		res               = op.Noop
	)

	switch {
	case cleanup:
		if ext.Status.DataPlaneClientAuth != nil && ext.Status.DataPlaneClientAuth.CertificateSecretRef != nil {
			err = r.Get(ctx, types.NamespacedName{
				Namespace: ext.Namespace,
				Name:      ext.Status.DataPlaneClientAuth.CertificateSecretRef.Name,
			}, certificateSecret)
		}
	case *ext.Spec.ClientAuth.CertificateSecret.Provisioning == konnectv1alpha1.ManualSecretProvisioning:
		// No need to check CertificateSecretRef is nil, as it is enforced at the CRD level.
		err = r.Get(ctx, types.NamespacedName{
			Namespace: ext.Namespace,
			Name:      ext.Spec.ClientAuth.CertificateSecret.CertificateSecretRef.Name,
		}, certificateSecret)
	case *ext.Spec.ClientAuth.CertificateSecret.Provisioning == konnectv1alpha1.AutomaticSecretProvisioning:
		res, certificateSecret, err = r.ensureCertificateSecret(ctx, &ext)
	}
	return res, certificateSecret, err
}

func konnectClusterTypeToCRDClusterType(clusterType sdkkonnectcomp.ControlPlaneClusterType) konnectv1alpha1.KonnectExtensionClusterType {
	switch clusterType {
	// When it's not specified by the caller (left empty) in Konnect it's set to CLUSTER_TYPE_CONTROL_PLANE.
	case sdkkonnectcomp.ControlPlaneClusterTypeClusterTypeControlPlane, "":
		return konnectv1alpha1.ClusterTypeControlPlane
	case sdkkonnectcomp.ControlPlaneClusterTypeClusterTypeK8SIngressController:
		return konnectv1alpha1.ClusterTypeK8sIngressController
	default:
		// default never happens as the validation is at the CRD level
		return ""
	}
}

func sanitizeCert(cert string) string {
	newCert := strings.TrimSuffix(cert, "\n")
	newCert = strings.ReplaceAll(newCert, "\r", "")
	return newCert
}
