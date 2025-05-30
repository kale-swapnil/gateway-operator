// Code generated by hack/generators/testcases-registration/main.go. DO NOT EDIT.
package integration

func init() {
	addTestsToTestSuite(
		TestAIGatewayCreation,
		TestControlPlaneEssentials,
		TestControlPlaneExtensionsDataPlaneMetrics,
		TestControlPlaneUpdate,
		TestControlPlaneWatchNamespaces,
		TestControlPlaneWhenNoDataPlane,
		TestDataPlaneBlueGreenHorizontalScaling,
		TestDataPlaneBlueGreenResourcesNotDeletedUntilOwnerIsRemoved,
		TestDataPlaneBlueGreenRollout,
		TestDataPlaneEssentials,
		TestDataPlaneHorizontalScaling,
		TestDataPlaneKonnectCert,
		TestDataPlanePodDisruptionBudget,
		TestDataPlaneScaleSubresource,
		TestDataPlaneServiceExternalTrafficPolicy,
		TestDataPlaneServiceTypes,
		TestDataPlaneSpecifyingServiceName,
		TestDataPlaneUpdate,
		TestDataPlaneValidation,
		TestDataPlaneVolumeMounts,
		TestGatewayClassCreation,
		TestGatewayClassUpdates,
		TestGatewayConfigurationEssentials,
		TestGatewayConfigurationServiceName,
		TestGatewayDataPlaneNetworkPolicy,
		TestGatewayEssentials,
		TestGatewayMultiple,
		TestGatewayWithMultipleListeners,
		TestHTTPRoute,
		TestHTTPRouteWithTLS,
		TestIngressEssentials,
		TestKongPluginInstallationEssentials,
		TestKonnectEntities,
		TestKonnectExtension,
		TestKonnectExtensionKonnectControlPlaneNotFound,
		TestManualGatewayUpgradesAndDowngrades,
		TestScalingDataPlaneThroughGatewayConfiguration,
	)
}
