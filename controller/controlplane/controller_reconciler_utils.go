package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/samber/lo"
	admregv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kong/gateway-operator/controller/pkg/controlplane"
	"github.com/kong/gateway-operator/controller/pkg/log"
	"github.com/kong/gateway-operator/controller/pkg/op"
	"github.com/kong/gateway-operator/controller/pkg/patch"
	"github.com/kong/gateway-operator/controller/pkg/secrets"
	"github.com/kong/gateway-operator/internal/versions"
	"github.com/kong/gateway-operator/pkg/clientops"
	"github.com/kong/gateway-operator/pkg/consts"
	k8sutils "github.com/kong/gateway-operator/pkg/utils/kubernetes"
	k8sreduce "github.com/kong/gateway-operator/pkg/utils/kubernetes/reduce"
	k8sresources "github.com/kong/gateway-operator/pkg/utils/kubernetes/resources"

	kcfgcontrolplane "github.com/kong/kubernetes-configuration/api/gateway-operator/controlplane"
	operatorv1alpha1 "github.com/kong/kubernetes-configuration/api/gateway-operator/v1alpha1"
	operatorv1beta1 "github.com/kong/kubernetes-configuration/api/gateway-operator/v1beta1"
)

// numReplicasWhenNoDataPlane represents the desired number of replicas
// for the controlplane deployment when no dataplane is set.
const numReplicasWhenNoDataPlane = 0

// -----------------------------------------------------------------------------
// Reconciler - Status Management
// -----------------------------------------------------------------------------

func (r *Reconciler) ensureIsMarkedScheduled(
	cp *operatorv1beta1.ControlPlane,
) bool {
	_, present := k8sutils.GetCondition(kcfgcontrolplane.ConditionTypeProvisioned, cp)
	if !present {
		condition := k8sutils.NewCondition(
			kcfgcontrolplane.ConditionTypeProvisioned,
			metav1.ConditionFalse,
			kcfgcontrolplane.ConditionReasonPodsNotReady,
			"ControlPlane resource is scheduled for provisioning",
		)

		k8sutils.SetCondition(condition, cp)
		return true
	}

	return false
}

// ensureDataPlaneStatus ensures that the dataplane is in the correct state
// to carry on with the controlplane deployments reconciliation.
// Information about the missing dataplane is stored in the controlplane status.
func (r *Reconciler) ensureDataPlaneStatus(
	cp *operatorv1beta1.ControlPlane,
	dataplane *operatorv1beta1.DataPlane,
) (dataplaneIsSet bool) {
	dataplaneIsSet = cp.Spec.DataPlane != nil && *cp.Spec.DataPlane == dataplane.Name
	condition, present := k8sutils.GetCondition(kcfgcontrolplane.ConditionTypeProvisioned, cp)

	newCondition := k8sutils.NewCondition(
		kcfgcontrolplane.ConditionTypeProvisioned,
		metav1.ConditionFalse,
		kcfgcontrolplane.ConditionReasonNoDataPlane,
		"DataPlane is not set",
	)
	if dataplaneIsSet {
		newCondition = k8sutils.NewCondition(
			kcfgcontrolplane.ConditionTypeProvisioned,
			metav1.ConditionFalse,
			kcfgcontrolplane.ConditionReasonPodsNotReady,
			"DataPlane was set, ControlPlane resource is scheduled for provisioning",
		)
	}
	if !present || condition.Status != newCondition.Status || condition.Reason != newCondition.Reason {
		k8sutils.SetCondition(newCondition, cp)
	}
	return dataplaneIsSet
}

// -----------------------------------------------------------------------------
// Reconciler - Spec Management
// -----------------------------------------------------------------------------

func (r *Reconciler) ensureDataPlaneConfiguration(
	ctx context.Context,
	cp *operatorv1beta1.ControlPlane,
	dataplaneServiceName string,
) error {
	changed := setControlPlaneEnvOnDataPlaneChange(
		&cp.Spec.ControlPlaneOptions,
		cp.Namespace,
		dataplaneServiceName,
	)
	if changed {
		if err := r.Update(ctx, cp); err != nil {
			return fmt.Errorf("failed updating ControlPlane's DataPlane: %w", err)
		}
		return nil
	}
	return nil
}

func setControlPlaneEnvOnDataPlaneChange(
	spec *operatorv1beta1.ControlPlaneOptions,
	namespace string,
	dataplaneServiceName string,
) bool {
	container := k8sutils.GetPodContainerByName(&spec.Deployment.PodTemplateSpec.Spec, consts.ControlPlaneControllerContainerName)
	if dataplaneIsSet := spec.DataPlane != nil && *spec.DataPlane != ""; dataplaneIsSet {
		newPublishServiceValue := k8stypes.NamespacedName{Namespace: namespace, Name: dataplaneServiceName}.String()
		if k8sutils.EnvValueByName(container.Env, "CONTROLLER_PUBLISH_SERVICE") != newPublishServiceValue {
			container.Env = k8sutils.UpdateEnv(container.Env, "CONTROLLER_PUBLISH_SERVICE", newPublishServiceValue)
			return true
		}
	} else if k8sutils.EnvValueByName(container.Env, "CONTROLLER_PUBLISH_SERVICE") != "" {
		container.Env = k8sutils.RejectEnvByName(container.Env, "CONTROLLER_PUBLISH_SERVICE")
		return true
	}

	return false
}

// -----------------------------------------------------------------------------
// Reconciler - Owned Resource Management
// -----------------------------------------------------------------------------

// ensureDeploymentParams is a helper struct to pass parameters to the ensureDeployment method.
type ensureDeploymentParams struct {
	ControlPlane                   *operatorv1beta1.ControlPlane
	ServiceAccountName             string
	AdminMTLSCertSecretName        string
	AdmissionWebhookCertSecretName string
	// EnforceConfig is a flag to enforce the configuration of the Deployment.
	// If set to true, the Deployment will be updated even if the spec hash matches.
	// This is useful when the Deployment has been manually modified by something
	// other than the operator to prevent the operator from endless reconciliation
	// (typically mutation webhook that enforces some cluster-wide policy,
	// typically for resources or security).
	EnforceConfig bool
	// WatchNamespaces contains a list of namespaces to watch for resources.
	// This list might have been filtered down from the list in the spec
	// as a result of the ReferenceGrant validation: if a ReferenceGrant is missing
	// in the requested namespace, the namespace is removed from the list.
	WatchNamespaces []string
}

// ensureDeployment ensures that a Deployment is created for the
// ControlPlane resource. Deployment will remain in dormant state until
// corresponding dataplane is set.
func (r *Reconciler) ensureDeployment(
	ctx context.Context,
	logger logr.Logger,
	params ensureDeploymentParams,
) (op.Result, *appsv1.Deployment, error) {
	dataplaneIsSet := params.ControlPlane.Spec.DataPlane != nil && *params.ControlPlane.Spec.DataPlane != ""

	deployments, err := k8sutils.ListDeploymentsForOwner(ctx,
		r.Client,
		params.ControlPlane.Namespace,
		params.ControlPlane.UID,
		client.MatchingLabels{
			consts.GatewayOperatorManagedByLabel: consts.ControlPlaneManagedLabelValue,
		},
	)
	if err != nil {
		return op.Noop, nil, err
	}

	count := len(deployments)
	if count > 1 {
		if err := k8sreduce.ReduceDeployments(ctx, r.Client, deployments); err != nil {
			return op.Noop, nil, err
		}
		return op.Noop, nil, errors.New("number of deployments reduced")
	}

	versionValidationOptions := make([]versions.VersionValidationOption, 0)
	if r.ValidateControlPlaneImage {
		versionValidationOptions = append(versionValidationOptions, versions.IsControlPlaneImageVersionSupported)
	}
	controlplaneImage, err := controlplane.GenerateImage(&params.ControlPlane.Spec.ControlPlaneOptions, versionValidationOptions...)
	if err != nil {
		return op.Noop, nil, err
	}
	generatedDeployment, err := k8sresources.GenerateNewDeploymentForControlPlane(k8sresources.GenerateNewDeploymentForControlPlaneParams{
		ControlPlane:                   params.ControlPlane,
		ControlPlaneImage:              controlplaneImage,
		ServiceAccountName:             params.ServiceAccountName,
		AdminMTLSCertSecretName:        params.AdminMTLSCertSecretName,
		AdmissionWebhookCertSecretName: params.AdmissionWebhookCertSecretName,
		WatchNamespaces:                params.WatchNamespaces,
	})
	if err != nil {
		return op.Noop, nil, err
	}

	if count == 1 {
		existingDeployment := &deployments[0]

		// If the enforceConfig flag is not set, we compare the spec hash of the
		// existing Deployment with the spec hash of the desired Deployment. If
		// the hashes match, we skip the update.
		if !params.EnforceConfig {
			match, err := k8sresources.SpecHashMatchesAnnotation(params.ControlPlane.Spec, existingDeployment)
			if err != nil {
				return op.Noop, nil, err
			}
			if match {
				log.Debug(logger, "ControlPlane Deployment spec hash matches existing Deployment, skipping update")
				return op.Noop, existingDeployment, nil
			}
			// If the spec hash does not match, we need to enforce the configuration
			// so fall through to the update logic.
		}

		var updated bool
		oldExistingDeployment := existingDeployment.DeepCopy()

		// ensure that object metadata is up to date
		updated, existingDeployment.ObjectMeta = k8sutils.EnsureObjectMetaIsUpdated(existingDeployment.ObjectMeta, generatedDeployment.ObjectMeta)

		// some custom comparison rules are needed for some PodTemplateSpec sub-attributes, in particular
		// resources and affinity.
		opts := []cmp.Option{
			cmp.Comparer(k8sresources.ResourceRequirementsEqual),
		}

		// ensure that PodTemplateSpec is up to date
		if !cmp.Equal(existingDeployment.Spec.Template, generatedDeployment.Spec.Template, opts...) {
			existingDeployment.Spec.Template = generatedDeployment.Spec.Template
			updated = true
		}

		// ensure that replication strategy is up to date
		replicas := params.ControlPlane.Spec.Deployment.Replicas
		switch {
		case !dataplaneIsSet && (replicas == nil || *replicas != numReplicasWhenNoDataPlane):
			// DataPlane was just unset, so we need to scale down the Deployment.
			if !cmp.Equal(existingDeployment.Spec.Replicas, lo.ToPtr(int32(numReplicasWhenNoDataPlane))) {
				existingDeployment.Spec.Replicas = lo.ToPtr(int32(numReplicasWhenNoDataPlane))
				updated = true
			}
		case dataplaneIsSet && (replicas != nil && *replicas != numReplicasWhenNoDataPlane):
			// DataPlane was just set, so we need to scale up the Deployment
			// and ensure the env variables that might have been changed in
			// deployment are updated.
			if !cmp.Equal(existingDeployment.Spec.Replicas, replicas) {
				existingDeployment.Spec.Replicas = replicas
				updated = true
			}
		}

		return patch.ApplyPatchIfNotEmpty(ctx, r.Client, logger, existingDeployment, oldExistingDeployment, updated)
	}

	if !dataplaneIsSet {
		generatedDeployment.Spec.Replicas = lo.ToPtr(int32(numReplicasWhenNoDataPlane))
	}
	if err := r.Create(ctx, generatedDeployment); err != nil {
		return op.Noop, nil, fmt.Errorf("failed creating ControlPlane Deployment %s: %w", generatedDeployment.Name, err)
	}

	log.Debug(logger, "deployment for ControlPlane created", "deployment", generatedDeployment.Name)
	return op.Created, generatedDeployment, nil
}

func (r *Reconciler) ensureServiceAccount(
	ctx context.Context,
	cp *operatorv1beta1.ControlPlane,
) (createdOrModified bool, sa *corev1.ServiceAccount, err error) {
	serviceAccounts, err := k8sutils.ListServiceAccountsForOwner(
		ctx,
		r.Client,
		cp.Namespace,
		cp.UID,
		client.MatchingLabels{
			consts.GatewayOperatorManagedByLabel: consts.ControlPlaneManagedLabelValue,
		},
	)
	if err != nil {
		return false, nil, err
	}

	count := len(serviceAccounts)
	if count > 1 {
		if err := k8sreduce.ReduceServiceAccounts(ctx, r.Client, serviceAccounts); err != nil {
			return false, nil, err
		}
		return false, nil, errors.New("number of serviceAccounts reduced")
	}

	generatedServiceAccount := k8sresources.GenerateNewServiceAccountForControlPlane(cp.Namespace, cp.Name)
	k8sutils.SetOwnerForObject(generatedServiceAccount, cp)

	if count == 1 {
		var updated bool
		existingServiceAccount := &serviceAccounts[0]
		updated, existingServiceAccount.ObjectMeta = k8sutils.EnsureObjectMetaIsUpdated(existingServiceAccount.ObjectMeta, generatedServiceAccount.ObjectMeta)
		if updated {
			if err := r.Update(ctx, existingServiceAccount); err != nil {
				return false, existingServiceAccount, fmt.Errorf("failed updating ControlPlane's ServiceAccount %s: %w", existingServiceAccount.Name, err)
			}
			return true, existingServiceAccount, nil
		}
		return false, existingServiceAccount, nil
	}

	return true, generatedServiceAccount, r.Create(ctx, generatedServiceAccount)
}

func (r *Reconciler) ensureRolesAndClusterRoles(
	ctx context.Context,
	logger logr.Logger,
	cp *operatorv1beta1.ControlPlane,
	controlplaneServiceAccount *corev1.ServiceAccount,
	validatedWatchNamespaces []string,
) (op.Result, error) {
	generatedRoles, generatedClusterRole, err := r.generateRoleAndClusterRole(cp, validatedWatchNamespaces)
	if err != nil {
		return op.Noop, err
	}

	log.Trace(logger, "ensuring ClusterRoles for ControlPlane Deployment exist")
	createdOrUpdated, controlplaneClusterRole, err := r.ensureClusterRole(ctx, cp, generatedClusterRole)
	if err != nil {
		return op.Noop, err
	}
	if createdOrUpdated {
		log.Debug(logger, "clusterRole updated")
		return op.Updated, nil // requeue will be triggered by the creation or update of the owned object
	}

	log.Trace(logger, "ensuring that ClusterRoleBindings for ControlPlane Deployment exist")
	createdOrUpdated, _, err = r.ensureClusterRoleBinding(ctx, cp, controlplaneServiceAccount.Name, controlplaneClusterRole.Name)
	if err != nil {
		return op.Noop, err
	}
	if createdOrUpdated {
		log.Debug(logger, "clusterRoleBinding updated")
		return op.Updated, nil // requeue will be triggered by the creation or update of the owned object
	}

	// If watchNamespaces is not empty then we need to generate roles and role bindings for each namespace.
	if len(generatedRoles) > 0 {
		log.Trace(logger, "ensuring Roles for ControlPlane Deployment exist")
		createdOrUpdated, controlplaneRoles, err := r.ensureRoles(ctx, cp, generatedRoles)
		if err != nil {
			return op.Noop, err
		}
		if createdOrUpdated {
			log.Debug(logger, "Roles created/updated")
			return op.Updated, nil // requeue will be triggered by the creation or update of Roles.
		}

		res, err := r.ensureRoleBindings(ctx, cp, controlplaneServiceAccount, controlplaneRoles)
		if err != nil {
			return op.Noop, err
		}
		if res != op.Noop {
			log.Debug(logger, "RoleBindings created/updated")
			return res, nil
		}

		deleted, err := r.pruneOutdatedRoles(ctx, logger, cp, validatedWatchNamespaces)
		if err != nil {
			return op.Noop, err
		}
		if deleted {
			log.Debug(logger, "not needed Roles deleted")
		}

		deleted, err = r.pruneOutdatedRoleBindings(ctx, logger, cp, validatedWatchNamespaces)
		if err != nil {
			return op.Noop, err
		}
		if deleted {
			log.Debug(logger, "not needed RoleBindings deleted")
		}
	}

	return op.Noop, nil
}

func (r *Reconciler) pruneOutdatedRoles(
	ctx context.Context,
	logger logr.Logger,
	cp *operatorv1beta1.ControlPlane,
	validatedWatchNamespaces []string,
) (deleted bool, err error) {
	namespaces := sets.New(validatedWatchNamespaces...)

	roles, err := k8sutils.ListRoles(
		ctx,
		r.Client,
		client.MatchingLabels(k8sutils.GetManagedByLabelSet(cp)),
	)
	if err != nil {
		return false, err
	}

	for _, role := range roles {
		objDeleted, err := deleteIfNotInNamespaceSet(ctx, r.Client, &role, namespaces, "ControlPlane's Role")
		if err != nil {
			return false, err
		}
		if !objDeleted {
			continue
		}

		deleted = true
		log.Debug(logger, "Role deleted", "role", role)
	}

	return deleted, nil
}

func (r *Reconciler) pruneOutdatedRoleBindings(
	ctx context.Context,
	logger logr.Logger,
	cp *operatorv1beta1.ControlPlane,
	validatedWatchNamespaces []string,
) (deleted bool, err error) {
	namespaces := sets.New(validatedWatchNamespaces...)

	roleBindings, err := k8sutils.ListRoleBindings(
		ctx,
		r.Client,
		client.MatchingLabels(k8sutils.GetManagedByLabelSet(cp)),
	)
	if err != nil {
		return false, err
	}

	for _, rb := range roleBindings {
		objDeleted, err := deleteIfNotInNamespaceSet(ctx, r.Client, &rb, namespaces, "ControlPlane's RoleBinding")
		if err != nil {
			return false, err
		}
		if !objDeleted {
			continue
		}

		deleted = true
		log.Debug(logger, "RoleBinding deleted", "role_binding", rb)
	}

	return deleted, nil
}

func deleteIfNotInNamespaceSet(
	ctx context.Context, cl client.Client, obj client.Object, namespaces sets.Set[string], msg string,
) (bool, error) {
	if namespaces.Has(obj.GetNamespace()) {
		return false, nil
	}

	if err := cl.Delete(ctx, obj); err != nil {
		return false, fmt.Errorf("failed deleting %s %s: %w", msg, client.ObjectKeyFromObject(obj), err)
	}
	return true, nil
}

func (r *Reconciler) ensureRoles(
	ctx context.Context,
	cp *operatorv1beta1.ControlPlane,
	generatedRoles []*rbacv1.Role,
) (createdOrUpdated bool, roles []*rbacv1.Role, err error) {
rolesLoop:
	for _, generatedRole := range generatedRoles {
		existingRoles, err := k8sutils.ListRoles(
			ctx,
			r.Client,
			client.MatchingLabels(k8sutils.GetManagedByLabelSet(cp)),
			client.InNamespace(generatedRole.Namespace),
		)
		if err != nil {
			return false, nil, err
		}

		switch count := len(existingRoles); count {
		case 0:
			if err := r.Create(ctx, generatedRole); err != nil {
				return false, nil, err
			}
			roles = append(roles, generatedRole)
			createdOrUpdated = true
		case 1:
			var (
				updated  bool
				existing = &existingRoles[0]
				old      = existing.DeepCopy()
			)

			updated, existing.ObjectMeta = k8sutils.EnsureObjectMetaIsUpdated(existing.ObjectMeta, generatedRole.ObjectMeta)
			if updated ||
				!cmp.Equal(existing.Rules, generatedRole.Rules) {
				existing.Rules = generatedRole.Rules
				if err := r.Patch(ctx, existing, client.MergeFrom(old)); err != nil {
					return false, nil, fmt.Errorf("failed patching ControlPlane's Role %s: %w", existing.Name, err)
				}
				roles = append(roles, existing)
				createdOrUpdated = true
				continue rolesLoop
			}
			roles = append(roles, existing)
		default:
			if err := k8sreduce.ReduceRoles(ctx, r.Client, existingRoles); err != nil {
				return false, nil, err
			}
			return false, nil, fmt.Errorf("number of Roles reduced from: %d to 1", count)
		}

	}

	return createdOrUpdated, roles, nil
}

func (r *Reconciler) ensureRoleBindings(
	ctx context.Context,
	cp *operatorv1beta1.ControlPlane,
	controlplaneServiceAccount *corev1.ServiceAccount,
	roles []*rbacv1.Role,
) (op.Result, error) {
	res := op.Noop
	for _, role := range roles {
		resEnsure, _, err := r.ensureRoleBinding(ctx, cp, controlplaneServiceAccount.Name, client.ObjectKeyFromObject(role))
		if err != nil {
			return op.Noop, err
		}

		if resEnsure != op.Noop {
			res = resEnsure
		}
	}

	return res, nil
}

func (r *Reconciler) ensureRoleBinding(
	ctx context.Context,
	cp *operatorv1beta1.ControlPlane,
	serviceAccountName string,
	roleNN k8stypes.NamespacedName,
) (op.Result, *rbacv1.RoleBinding, error) {
	logger := log.GetLogger(ctx, "controlplane.ensureRoleBinding", r.LoggingMode)

	roleBindings, err := k8sutils.ListRoleBindings(
		ctx,
		r.Client,
		client.MatchingLabels(k8sutils.GetManagedByLabelSet(cp)),
		client.InNamespace(roleNN.Namespace),
	)
	if err != nil {
		return op.Noop, nil, err
	}

	count := len(roleBindings)
	if count > 1 {
		if err := k8sreduce.ReduceRoleBindings(ctx, r.Client, roleBindings); err != nil {
			return op.Noop, nil, err
		}
		return op.Noop, nil, errors.New("number of roleBindings reduced")
	}

	generated := k8sresources.GenerateNewRoleBindingForControlPlane(cp, serviceAccountName, roleNN)
	k8sutils.SetOwnerForObjectThroughLabels(generated, cp)

	if count == 1 {
		existing := &roleBindings[0]
		// Delete and re-create RoleBinding if name of Role changed because RoleRef is immutable.
		if !k8sresources.CompareRoleName(existing, roleNN.Name) {
			log.Debug(logger, "Role name changed, delete and re-create a RoleBinding",
				"old_role", existing.RoleRef.Name,
				"new_role", roleNN,
			)
			if err := r.Delete(ctx, existing); err != nil {
				return op.Noop, nil, err
			}
			return op.Noop, nil, errors.New("name of Role changed, out of date RoleBinding deleted")
		}

		var (
			old                   = existing.DeepCopy()
			updated               bool
			updatedServiceAccount bool
		)
		updated, existing.ObjectMeta = k8sutils.EnsureObjectMetaIsUpdated(existing.ObjectMeta, generated.ObjectMeta)

		if !k8sresources.RoleBindingContainsServiceAccount(existing, cp.Namespace, serviceAccountName) {
			existing.Subjects = generated.Subjects
			updatedServiceAccount = true
		}

		if updated || updatedServiceAccount {
			if err := r.Patch(ctx, existing, client.MergeFrom(old)); err != nil {
				return op.Noop, existing, fmt.Errorf("failed patching ControlPlane's RoleBinding %s: %w", existing.Name, err)
			}
			return op.Updated, existing, nil
		}
		return op.Noop, existing, nil

	}

	return op.Created, generated, r.Create(ctx, generated)
}

func (r *Reconciler) generateRoleAndClusterRole(
	cp *operatorv1beta1.ControlPlane,
	verifiedWatchNamespaces []string,
) ([]*rbacv1.Role, *rbacv1.ClusterRole, error) {
	controlplaneContainer := k8sutils.GetPodContainerByName(&cp.Spec.Deployment.PodTemplateSpec.Spec, consts.ControlPlaneControllerContainerName)
	clusterRole, err := k8sresources.GenerateNewClusterRoleForControlPlane(cp.Name, controlplaneContainer.Image, r.ValidateControlPlaneImage)
	if err != nil {
		return nil, nil, err
	}
	k8sutils.SetOwnerForObjectThroughLabels(clusterRole, cp)

	// If watchNamespaces is not empty, we're generating Roles and a ClusterRole.
	if len(verifiedWatchNamespaces) > 0 {
		m, err := r.DiscoveryClient.GetAPIResourceListMapping()
		if err != nil {
			return nil, nil, err
		}
		roleRules, clusterRolesRules := processClusterRole(clusterRole, m)
		clusterRole.Rules = clusterRolesRules

		roles := make([]*rbacv1.Role, 0, len(verifiedWatchNamespaces))
		for _, namespace := range verifiedWatchNamespaces {
			role := k8sresources.GenerateNewRoleForControlPlane(cp, namespace, roleRules)
			k8sutils.SetOwnerForObjectThroughLabels(role, cp)
			roles = append(roles, role)
		}
		return roles, clusterRole, nil
	}

	return nil, clusterRole, nil
}

func (r *Reconciler) ensureClusterRole(
	ctx context.Context,
	cp *operatorv1beta1.ControlPlane,
	generatedClusterRole *rbacv1.ClusterRole,
) (createdOrUpdated bool, cr *rbacv1.ClusterRole, err error) {
	clusterRoles, err := k8sutils.ListClusterRoles(
		ctx,
		r.Client,
		client.MatchingLabels(k8sutils.GetManagedByLabelSet(cp)),
	)
	if err != nil {
		return false, nil, err
	}

	count := len(clusterRoles)
	if count > 1 {
		if err := k8sreduce.ReduceClusterRoles(ctx, r.Client, clusterRoles); err != nil {
			return false, nil, err
		}
		return false, nil, errors.New("number of clusterRoles reduced")
	}

	if count == 1 {
		var (
			updated  bool
			existing = &clusterRoles[0]
			old      = existing.DeepCopy()
		)

		updated, existing.ObjectMeta = k8sutils.EnsureObjectMetaIsUpdated(existing.ObjectMeta, generatedClusterRole.ObjectMeta)
		if updated ||
			!cmp.Equal(existing.Rules, generatedClusterRole.Rules) ||
			!cmp.Equal(existing.AggregationRule, generatedClusterRole.AggregationRule) {
			existing.Rules = generatedClusterRole.Rules
			existing.AggregationRule = generatedClusterRole.AggregationRule
			if err := r.Patch(ctx, existing, client.MergeFrom(old)); err != nil {
				return false, existing, fmt.Errorf("failed patching ControlPlane's ClusterRole %s: %w", existing.Name, err)
			}
			return true, existing, nil
		}
		return false, existing, nil
	}

	return true, generatedClusterRole, r.Create(ctx, generatedClusterRole)
}

// processClusterRole processes the generated ClusterRole and splits its policy rules
// into two slices: one for the Role and one for the ClusterRole.
// The split is based on the availability of the resources in the cluster.
//
// For example assuming the following rules on the ClusterRole:
// ```
// rules:
// - apiGroups:
//   - ""
//     resources:
//   - configmaps
//   - namespaces
//     verbs:
//   - create
//
// ```
//
// If the cluster has the `configmaps` resource available in the cluster,
// the rule will be added to the RolePolicyRules as it's a namespaced resource.
// If the cluster has the `namespaces` resource available in the cluster,
// the rule will be added to the ClusterRolePolicyRules as it's a cluster-scoped resource.
func processClusterRole(
	generated *rbacv1.ClusterRole,
	gvl map[schema.GroupVersion]*metav1.APIResourceList,
) (rolePolicyRules []rbacv1.PolicyRule, clusterRolePolicyRules []rbacv1.PolicyRule) {
	for _, rule := range generated.Rules {
		// There's typically just 1 APIGroup in the rule, but we loop over all of them
		// to ensure that we're only adding rules for the APIGroups that are available.
		for _, apiGroup := range rule.APIGroups {
			for _, resource := range rule.Resources {
				var found bool
				for gv, apiresl := range gvl {
					if gv.Group != apiGroup {
						continue
					}

					apires, ok := lo.Find(apiresl.APIResources,
						func(apires metav1.APIResource) bool {
							return apires.Group == apiGroup && apires.Name == resource
						})
					if !ok {
						continue
					}

					found = true

					// There can be more than one resource in policy rule so we need to
					// create a new rule for each resource separately.
					rule.Resources = []string{resource}

					if apires.Namespaced {
						rolePolicyRules = append(rolePolicyRules, rule)
					} else {
						clusterRolePolicyRules = append(clusterRolePolicyRules, rule)
					}
					break
				}

				// Just in case the resource is not found in the cluster, we add
				// it to ClusterRole as it's the default behavior and we better
				// have the rule set rather than not and break the configuration.
				if !found {
					clusterRolePolicyRules = append(clusterRolePolicyRules, rule)
				}
			}
		}
	}

	return rolePolicyRules, clusterRolePolicyRules
}

func (r *Reconciler) ensureClusterRoleBinding(
	ctx context.Context,
	cp *operatorv1beta1.ControlPlane,
	serviceAccountName string,
	clusterRoleName string,
) (createdOrUpdate bool, crb *rbacv1.ClusterRoleBinding, err error) {
	logger := log.GetLogger(ctx, "controlplane.ensureClusterRoleBinding", r.LoggingMode)

	clusterRoleBindings, err := k8sutils.ListClusterRoleBindings(
		ctx,
		r.Client,
		client.MatchingLabels(k8sutils.GetManagedByLabelSet(cp)),
	)
	if err != nil {
		return false, nil, err
	}

	count := len(clusterRoleBindings)
	if count > 1 {
		if err := k8sreduce.ReduceClusterRoleBindings(ctx, r.Client, clusterRoleBindings); err != nil {
			return false, nil, err
		}
		return false, nil, errors.New("number of clusterRoleBindings reduced")
	}

	generated := k8sresources.GenerateNewClusterRoleBindingForControlPlane(cp.Namespace, cp.Name, serviceAccountName, clusterRoleName)
	k8sutils.SetOwnerForObjectThroughLabels(generated, cp)

	if count == 1 {
		existing := &clusterRoleBindings[0]
		// Delete and re-create ClusterRoleBinding if name of ClusterRole changed because RoleRef is immutable.
		if !k8sresources.CompareClusterRoleName(existing, clusterRoleName) {
			log.Debug(logger, "ClusterRole name changed, delete and re-create a ClusterRoleBinding",
				"old_cluster_role", existing.RoleRef.Name,
				"new_cluster_role", clusterRoleName,
			)
			if err := r.Delete(ctx, existing); err != nil {
				return false, nil, err
			}
			return false, nil, errors.New("name of ClusterRole changed, out of date ClusterRoleBinding deleted")
		}

		var (
			old                   = existing.DeepCopy()
			updated               bool
			updatedServiceAccount bool
		)
		updated, existing.ObjectMeta = k8sutils.EnsureObjectMetaIsUpdated(existing.ObjectMeta, generated.ObjectMeta)

		if !k8sresources.ClusterRoleBindingContainsServiceAccount(existing, cp.Namespace, serviceAccountName) {
			existing.Subjects = generated.Subjects
			updatedServiceAccount = true
		}

		if updated || updatedServiceAccount {
			if err := r.Patch(ctx, existing, client.MergeFrom(old)); err != nil {
				return false, existing, fmt.Errorf("failed patching ControlPlane's ClusterRoleBinding %s: %w", existing.Name, err)
			}
			return true, existing, nil
		}
		return false, existing, nil

	}

	return true, generated, r.Create(ctx, generated)
}

// ensureAdminMTLSCertificateSecret ensures that a Secret is created with the certificate for mTLS communication between the
// ControlPlane and the DataPlane.
func (r *Reconciler) ensureAdminMTLSCertificateSecret(
	ctx context.Context,
	cp *operatorv1beta1.ControlPlane,
) (
	op.Result,
	*corev1.Secret,
	error,
) {
	usages := []certificatesv1.KeyUsage{
		certificatesv1.UsageKeyEncipherment,
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageClientAuth,
	}
	matchingLabels := client.MatchingLabels{
		consts.SecretUsedByServiceLabel: consts.ControlPlaneServiceKindAdmin,
	}
	// this subject is arbitrary. data planes only care that client certificates are signed by the trusted CA, and will
	// accept a certificate with any subject
	return secrets.EnsureCertificate(ctx,
		cp,
		fmt.Sprintf("%s.%s", cp.Name, cp.Namespace),
		k8stypes.NamespacedName{
			Namespace: r.ClusterCASecretNamespace,
			Name:      r.ClusterCASecretName,
		},
		usages,
		r.ClusterCAKeyConfig,
		r.Client,
		matchingLabels,
	)
}

// ensureAdmissionWebhookCertificateSecret ensures that a Secret is created with the serving certificate for the
// ControlPlane's admission webhook.
func (r *Reconciler) ensureAdmissionWebhookCertificateSecret(
	ctx context.Context,
	logger logr.Logger,
	cp *operatorv1beta1.ControlPlane,
	admissionWebhookService *corev1.Service,
) (
	op.Result,
	*corev1.Secret,
	error,
) {
	usages := []certificatesv1.KeyUsage{
		certificatesv1.UsageKeyEncipherment,
		certificatesv1.UsageServerAuth,
		certificatesv1.UsageDigitalSignature,
	}
	matchingLabels := client.MatchingLabels{
		consts.SecretUsedByServiceLabel: consts.ControlPlaneServiceKindWebhook,
	}
	if !isAdmissionWebhookEnabled(ctx, r.Client, logger, cp) {
		labels := k8sresources.GetManagedLabelForOwner(cp)
		labels[consts.SecretUsedByServiceLabel] = consts.ControlPlaneServiceKindWebhook
		secrets, err := k8sutils.ListSecretsForOwner(ctx, r.Client, cp.GetUID(), matchingLabels)
		if err != nil {
			return op.Noop, nil, fmt.Errorf("failed listing Secrets for ControlPlane %s/: %w", client.ObjectKeyFromObject(cp), err)
		}
		if len(secrets) == 0 {
			return op.Noop, nil, nil
		}
		if err := clientops.DeleteAll(ctx, r.Client, secrets); err != nil {
			return op.Noop, nil, fmt.Errorf("failed deleting ControlPlane admission webhook Secret: %w", err)
		}
		return op.Deleted, nil, nil
	}

	return secrets.EnsureCertificate(ctx,
		cp,
		fmt.Sprintf("%s.%s.svc", admissionWebhookService.Name, admissionWebhookService.Namespace),
		k8stypes.NamespacedName{
			Namespace: r.ClusterCASecretNamespace,
			Name:      r.ClusterCASecretName,
		},
		usages,
		r.ClusterCAKeyConfig,
		r.Client,
		matchingLabels,
	)
}

// ensureOwnedClusterRolesDeleted removes all the owned ClusterRoles of the controlplane.
// it is called on cleanup of owned cluster resources on controlplane deletion.
// returns nil if all of owned ClusterRoles successfully deleted (ok if no owned CRs or NotFound on deleting CRs).
func (r *Reconciler) ensureOwnedClusterRolesDeleted(
	ctx context.Context,
	cp *operatorv1beta1.ControlPlane,
) (deletions bool, err error) {
	clusterRoles, err := k8sutils.ListClusterRoles(
		ctx,
		r.Client,
		client.MatchingLabels(k8sutils.GetManagedByLabelSet(cp)),
	)
	if err != nil {
		return false, err
	}

	var (
		deleted bool
		errs    []error
	)
	for i := range clusterRoles {
		if err = r.Delete(ctx, &clusterRoles[i]); client.IgnoreNotFound(err) != nil {
			errs = append(errs, err)
			continue
		}
		deleted = true
	}

	return deleted, errors.Join(errs...)
}

// ensureOwnedClusterRoleBindingsDeleted removes all the owned ClusterRoleBindings of the controlplane
// it is called on cleanup of owned cluster resources on controlplane deletion.
// returns nil if all of owned ClusterRoleBindings successfully deleted (ok if no owned CRBs or NotFound on deleting CRBs).
func (r *Reconciler) ensureOwnedClusterRoleBindingsDeleted(
	ctx context.Context,
	cp *operatorv1beta1.ControlPlane,
) (deletions bool, err error) {
	clusterRoleBindings, err := k8sutils.ListClusterRoleBindings(
		ctx,
		r.Client,
		client.MatchingLabels(k8sutils.GetManagedByLabelSet(cp)),
	)
	if err != nil {
		return false, err
	}

	var (
		deleted bool
		errs    []error
	)
	for i := range clusterRoleBindings {
		if err = r.Delete(ctx, &clusterRoleBindings[i]); client.IgnoreNotFound(err) != nil {
			errs = append(errs, err)
			continue
		}
		deleted = true
	}

	return deleted, errors.Join(errs...)
}

func (r *Reconciler) ensureOwnedValidatingWebhookConfigurationDeleted(ctx context.Context,
	cp *operatorv1beta1.ControlPlane,
) (deletions bool, err error) {
	validatingWebhookConfigurations, err := k8sutils.ListValidatingWebhookConfigurations(
		ctx,
		r.Client,
		client.MatchingLabels(k8sutils.GetManagedByLabelSet(cp)),
	)
	if err != nil {
		return false, fmt.Errorf("failed listing webhook configurations for owner: %w", err)
	}

	var (
		deleted bool
		errs    []error
	)
	for i := range validatingWebhookConfigurations {
		if err = r.Delete(ctx, &validatingWebhookConfigurations[i]); client.IgnoreNotFound(err) != nil {
			errs = append(errs, err)
			continue
		}
		deleted = true
	}
	return deleted, errors.Join(errs...)
}

func (r *Reconciler) ensureAdmissionWebhookService(
	ctx context.Context,
	logger logr.Logger,
	cl client.Client,
	cp *operatorv1beta1.ControlPlane,
) (op.Result, *corev1.Service, error) {
	matchingLabels := k8sresources.GetManagedLabelForOwner(cp)
	matchingLabels[consts.ControlPlaneServiceLabel] = consts.ControlPlaneServiceKindWebhook

	services, err := k8sutils.ListServicesForOwner(
		ctx,
		cl,
		cp.Namespace,
		cp.UID,
		matchingLabels,
	)
	if err != nil {
		return op.Noop, nil, fmt.Errorf("failed listing admission webhook Services for ControlPlane %s/%s: %w", cp.Namespace, cp.Name, err)
	}

	if !isAdmissionWebhookEnabled(ctx, cl, logger, cp) {
		if len(services) == 0 {
			return op.Noop, nil, nil
		}
		if err := clientops.DeleteAll(ctx, r.Client, services); err != nil {
			return op.Noop, nil, fmt.Errorf("failed deleting ControlPlane admission webhook Service: %w", err)
		}
		return op.Deleted, nil, nil
	}

	count := len(services)
	if count > 1 {
		if err := k8sreduce.ReduceServices(ctx, cl, services); err != nil {
			return op.Noop, nil, err
		}
		return op.Noop, nil, errors.New("number of ControlPlane admission webhook Services reduced")
	}

	generatedService, err := k8sresources.GenerateNewAdmissionWebhookServiceForControlPlane(cp)
	if err != nil {
		return op.Noop, nil, err
	}

	if count == 1 {
		var updated bool
		existingService := &services[0]
		updated, existingService.ObjectMeta = k8sutils.EnsureObjectMetaIsUpdated(existingService.ObjectMeta, generatedService.ObjectMeta)

		if !cmp.Equal(existingService.Spec.Selector, generatedService.Spec.Selector) {
			existingService.Spec.Selector = generatedService.Spec.Selector
			updated = true
		}
		if !cmp.Equal(existingService.Spec.Ports, generatedService.Spec.Ports) {
			existingService.Spec.Ports = generatedService.Spec.Ports
			updated = true
		}

		if updated {
			if err := cl.Update(ctx, existingService); err != nil {
				return op.Noop, existingService, fmt.Errorf("failed updating ControlPlane admission webhook Service %s: %w", existingService.Name, err)
			}
			return op.Updated, existingService, nil
		}
		return op.Noop, existingService, nil
	}

	if err := cl.Create(ctx, generatedService); err != nil {
		return op.Noop, nil, fmt.Errorf("failed creating ControlPlane admission webhook Service: %w", err)
	}

	return op.Created, generatedService, nil
}

func (r *Reconciler) ensureValidatingWebhookConfiguration(
	ctx context.Context,
	cp *operatorv1beta1.ControlPlane,
	certSecret *corev1.Secret,
	webhookService *corev1.Service,
	enforceConfig bool,
) (op.Result, error) {
	logger := log.GetLogger(ctx, "controlplane.ensureValidatingWebhookConfiguration", r.LoggingMode)

	validatingWebhookConfigurations, err := k8sutils.ListValidatingWebhookConfigurations(
		ctx,
		r.Client,
		client.MatchingLabels(k8sutils.GetManagedByLabelSet(cp)),
	)
	if err != nil {
		return op.Noop, fmt.Errorf("failed listing webhook configurations for owner: %w", err)
	}

	count := len(validatingWebhookConfigurations)
	if count > 1 {
		if err := k8sreduce.ReduceValidatingWebhookConfigurations(ctx, r.Client, validatingWebhookConfigurations); err != nil {
			return op.Noop, err
		}
		return op.Noop, errors.New("number of validatingWebhookConfigurations reduced")
	}

	if !isAdmissionWebhookEnabled(ctx, r.Client, logger, cp) {
		if len(validatingWebhookConfigurations) == 0 {
			return op.Noop, nil
		}
		if err := clientops.DeleteAll(ctx, r.Client, validatingWebhookConfigurations); err != nil {
			return op.Noop, fmt.Errorf("failed deleting ControlPlane admission webhook ValidatingWebhookConfiguration: %w", err)
		}
		return op.Deleted, nil
	}

	cpContainer := k8sutils.GetPodContainerByName(&cp.Spec.Deployment.PodTemplateSpec.Spec, consts.ControlPlaneControllerContainerName)
	if cpContainer == nil {
		return op.Noop, errors.New("controller container not found")
	}

	caBundle, ok := certSecret.Data["ca.crt"]
	if !ok {
		return op.Noop, errors.New("ca.crt not found in secret")
	}
	generatedWebhookConfiguration, err := k8sresources.GenerateValidatingWebhookConfigurationForControlPlane(
		cp.Name,
		cpContainer.Image,
		r.ValidateControlPlaneImage,
		admregv1.WebhookClientConfig{
			Service: &admregv1.ServiceReference{
				Namespace: cp.Namespace,
				Name:      webhookService.GetName(),
				Port:      lo.ToPtr(int32(consts.ControlPlaneAdmissionWebhookListenPort)),
			},
			CABundle: caBundle,
		},
	)
	if err != nil {
		return op.Noop, fmt.Errorf("failed generating ControlPlane's ValidatingWebhookConfiguration: %w", err)
	}
	k8sutils.SetOwnerForObjectThroughLabels(generatedWebhookConfiguration, cp)
	if err := k8sresources.AnnotateObjWithHash(generatedWebhookConfiguration, cp.Spec); err != nil {
		return op.Noop, err
	}

	if count == 1 {
		var updated bool
		webhookConfiguration := validatingWebhookConfigurations[0]

		// If the enforceConfig flag is not set, we compare the spec hash of the
		// existing ValidatingWebhookConfiguration with the spec hash of the desired
		// ValidatingWebhookConfiguration. If the hashes match, we skip the update.
		if !enforceConfig {
			match, err := k8sresources.SpecHashMatchesAnnotation(cp.Spec, &webhookConfiguration)
			if err != nil {
				return op.Noop, err
			}
			if match {
				log.Debug(logger, "ControlPlane ValidatingWebhookConfiguration spec hash matches existing ValidatingWebhookConfiguration, skipping update")
				return op.Noop, nil
			}
			// If the spec hash does not match, we need to enforce the configuration
			// so fall through to the update logic.
		}

		old := webhookConfiguration.DeepCopy()

		updated, webhookConfiguration.ObjectMeta = k8sutils.EnsureObjectMetaIsUpdated(webhookConfiguration.ObjectMeta, generatedWebhookConfiguration.ObjectMeta)

		if !cmp.Equal(webhookConfiguration.Webhooks, generatedWebhookConfiguration.Webhooks) ||
			!cmp.Equal(webhookConfiguration.Labels, generatedWebhookConfiguration.Labels) {
			webhookConfiguration.Webhooks = generatedWebhookConfiguration.Webhooks
			updated = true
		}

		if updated {
			log.Debug(logger, "patching existing ValidatingWebhookConfiguration")
			return op.Updated, r.Patch(ctx, &webhookConfiguration, client.MergeFrom(old))
		}

		return op.Noop, nil
	}

	return op.Created, r.Create(ctx, generatedWebhookConfiguration)
}

func (r *Reconciler) validateWatchNamespaceGrants(
	ctx context.Context,
	cp *operatorv1beta1.ControlPlane,
) ([]string, error) {
	if cp.Spec.WatchNamespaces == nil {
		return nil, errors.New("spec.watchNamespaces cannot be empty")
	}

	switch cp.Spec.WatchNamespaces.Type {
	// NOTE: We currentlty do not require any ReferenceGrants or other permission
	// granting resources for the "All" case.
	case operatorv1beta1.WatchNamespacesTypeAll:
		return nil, nil
	// No special permissions are required to watch the controlplane's own namespace.
	case operatorv1beta1.WatchNamespacesTypeOwn:
		return []string{cp.Namespace}, nil
	case operatorv1beta1.WatchNamespacesTypeList:
		var nsList []string
		for _, ns := range cp.Spec.WatchNamespaces.List {
			if err := ensureWatchNamespaceGrantsForNamespace(ctx, r.Client, cp, ns); err != nil {
				return nsList, err
			}
			nsList = append(nsList, ns)
		}
		// Add ControlPlane's own namespace as it will add it anyway because
		// that's where the default "publish service" exists.
		// We add it here as we do not require a ReferenceGrant for own namespace
		// so there's no validation whether a grant exists.
		nsList = append(nsList, cp.Namespace)

		return nsList, nil
	default:
		return nil, fmt.Errorf("unexpected watchNamespaces.type: %q", cp.Spec.WatchNamespaces.Type)
	}
}

// +kubebuilder:rbac:groups=gateway-operator.konghq.com,resources=watchnamespacegrants,verbs=list

// ensureWatchNamespaceGrantsForNamespace ensures that a WatchNamespaceGrant exists for the
// given namespace and ControlPlane.
// It returns an error if a WatchNamespaceGrant is missing.
func ensureWatchNamespaceGrantsForNamespace(
	ctx context.Context,
	cl client.Client,
	cp *operatorv1beta1.ControlPlane,
	ns string,
) error {
	var grants operatorv1alpha1.WatchNamespaceGrantList
	if err := cl.List(ctx, &grants, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("failed listing WatchNamespaceGrants in namespace %s: %w", ns, err)
	}
	for _, refGrant := range grants.Items {
		if !lo.ContainsBy(refGrant.Spec.From, func(from operatorv1alpha1.WatchNamespaceGrantFrom) bool {
			return from.Group == operatorv1beta1.SchemeGroupVersion.Group &&
				from.Kind == "ControlPlane" &&
				from.Namespace == cp.Namespace
		}) {
			continue
		}

		return nil
	}
	return fmt.Errorf("WatchNamespaceGrant in Namespace %s to ControlPlane in Namespace %s not found", ns, cp.Namespace)
}
