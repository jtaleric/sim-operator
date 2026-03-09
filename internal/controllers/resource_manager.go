package controllers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	mathrand "math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	buildv1 "github.com/openshift/api/build/v1"
	imagev1 "github.com/openshift/api/image/v1"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	scalev1 "github.com/jtaleric/sim-operator/api/v1"
)

// manageNamespaceResources creates and manages resources within a namespace
func (r *ScaleLoadConfigReconciler) manageNamespaceResources(ctx context.Context,
	config *scalev1.ScaleLoadConfig, namespace corev1.Namespace) (map[string]int, error) {

	log := r.Log.WithName("resource-manager").WithValues("namespace", namespace.Name)
	resourceCounts := make(map[string]int)
	var totalApiCalls, totalCreated, totalDeleted, totalUpdated int32
	startTime := time.Now()

	// Verify namespace exists and is ready before creating any resources
	ready, phase := r.checkNamespaceStatus(ctx, namespace.Name)
	if !ready {
		log.V(1).Info("Namespace not ready for resource creation", "phase", phase)
		return resourceCounts, fmt.Errorf("namespace %s is not ready (phase: %s)", namespace.Name, phase)
	}

	log.V(1).Info("Starting resource management for namespace",
		"phase", phase,
		"configmapsEnabled", config.Spec.ResourceChurn.ConfigMaps.Enabled,
		"secretsEnabled", config.Spec.ResourceChurn.Secrets.Enabled,
		"routesEnabled", config.Spec.ResourceChurn.Routes.Enabled,
		"imagestreamsEnabled", config.Spec.ResourceChurn.ImageStreams.Enabled,
		"buildconfigsEnabled", config.Spec.ResourceChurn.BuildConfigs.Enabled,
		"eventsEnabled", config.Spec.ResourceChurn.Events.Enabled,
		"podsEnabled", config.Spec.ResourceChurn.Pods.Enabled)

	// Use parallel resource management for optimal performance
	// This processes all resource types concurrently within the namespace
	resourceCounts = r.manageResourceTypesParallel(ctx, config, namespace)

	duration := time.Since(startTime)

	// Calculate totals from individual resource managers
	for resourceType, count := range resourceCounts {
		if resourceType == "events" {
			totalCreated += int32(count) // Events are only created, not updated
		}
	}

	log.V(1).Info("Namespace resource management completed",
		"duration", duration.String(),
		"totalApiCalls", totalApiCalls,
		"totalCreated", totalCreated,
		"totalUpdated", totalUpdated,
		"totalDeleted", totalDeleted,
		"resourceCounts", resourceCounts,
		"apiCallsPerSecond", fmt.Sprintf("%.1f", float64(totalApiCalls)/duration.Seconds()))

	return resourceCounts, nil
}

// shouldCreateResourceForNamespace checks if a resource should be created based on namespace interval
func (r *ScaleLoadConfigReconciler) shouldCreateResourceForNamespace(namespace corev1.Namespace, interval int32) bool {
	// Default to creating resource if no interval specified or interval is 1
	if interval <= 1 {
		return true
	}

	// Extract namespace index from labels
	indexStr, exists := namespace.Labels["scale.openshift.io/namespace-index"]
	if !exists {
		// If no index label (for backwards compatibility), create the resource
		return true
	}

	index, err := strconv.Atoi(indexStr)
	if err != nil {
		// If index is not parseable, default to creating the resource
		return true
	}

	// Create resource only if namespace index is divisible by interval
	// This creates resources in every Nth namespace (0, interval, 2*interval, etc.)
	return index%int(interval) == 0
}

// manageConfigMaps creates and manages ConfigMap resources
func (r *ScaleLoadConfigReconciler) manageConfigMaps(ctx context.Context,
	config *scalev1.ScaleLoadConfig, namespace string, targetCount int32) (int32, error) {

	log := r.Log.WithName("configmap-manager").WithValues("namespace", namespace, "targetCount", targetCount)

	// Check if it's time to perform configmap operations based on update frequency
	if !r.shouldPerformResourceOperation(namespace, "configMaps", config.Spec.ResourceChurn.ConfigMaps.UpdateFrequencyMin, config.Spec.ResourceChurn.ConfigMaps.UpdateFrequencyMax) {
		log.V(1).Info("Skipping configmap operations - not within update frequency window")
		return r.getCurrentResourceCount(ctx, config, namespace, "configMaps")
	}

	log.V(1).Info("Performing configmap operations within update frequency window")

	// Check maximum limit and adjust target count if needed
	effectiveTargetCount, err := r.checkMaximumLimit(ctx, config, "configMaps", targetCount, config.Spec.ResourceChurn.ConfigMaps.Maximum)
	if err != nil {
		return 0, fmt.Errorf("failed to check maximum limit for configMaps: %w", err)
	}

	if effectiveTargetCount != targetCount {
		log.Info("ConfigMap creation limited by maximum",
			"requestedCount", targetCount,
			"effectiveCount", effectiveTargetCount,
			"maximum", config.Spec.ResourceChurn.ConfigMaps.Maximum)
		targetCount = effectiveTargetCount
	}

	// List existing ConfigMaps managed by this operator
	configMapList := &corev1.ConfigMapList{}
	listOpts := &client.ListOptions{
		Namespace: namespace,
	}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "configmap",
	}.ApplyToList(listOpts)

	if err := r.List(ctx, configMapList, listOpts); err != nil {
		return 0, fmt.Errorf("failed to list ConfigMaps: %w", err)
	}
	r.recordAPICall(config, 1) // List operation

	currentCount := len(configMapList.Items)
	var created, deleted, apiCalls int32
	apiCalls++ // List operation

	// Update last operation time for this resource type in this namespace
	r.updateLastResourceOperation(namespace, "configMaps")

	log.V(1).Info("ConfigMap management starting", "current", currentCount, "target", targetCount)

	// Scale up if needed
	if int32(currentCount) < targetCount {
		toCreate := targetCount - int32(currentCount)
		for i := int32(currentCount); i < targetCount; i++ {
			configMap := r.generateConfigMap(config, namespace, i)
			if err := r.Create(ctx, configMap); err != nil {
				log.Error(err, "Failed to create ConfigMap", "name", configMap.Name, "created", created)
				return int32(currentCount) + created, fmt.Errorf("failed to create ConfigMap: %w", err)
			}
			r.recordAPICall(config, 1) // Create operation
			created++
			apiCalls++
		}
		log.V(1).Info("ConfigMaps created", "count", toCreate, "apiCalls", created)
	}

	// Scale down if needed
	if int32(currentCount) > targetCount {
		toDelete := int32(currentCount) - targetCount
		var deleted int32
		for i := int32(len(configMapList.Items)) - 1; i >= targetCount && deleted < toDelete; i-- {
			if err := r.Delete(ctx, &configMapList.Items[i]); err != nil {
				log.Error(err, "Failed to delete ConfigMap", "name", configMapList.Items[i].Name, "deleted", deleted)
				return int32(currentCount) - deleted, fmt.Errorf("failed to delete ConfigMap: %w", err)
			}
			r.recordAPICall(config, 1) // Delete operation
			deleted++
			apiCalls++
		}
		log.V(1).Info("ConfigMaps deleted", "count", deleted, "apiCalls", deleted)
	}

	// Randomly update some ConfigMaps to simulate churn
	objs := make([]client.Object, len(configMapList.Items))
	for i, item := range configMapList.Items {
		objs[i] = &item
	}
	updatedCount := r.performResourceChurn(ctx, config, objs, namespace, "configmap")
	apiCalls += updatedCount

	log.V(1).Info("ConfigMap management completed",
		"final", targetCount,
		"created", created,
		"deleted", deleted,
		"updated", updatedCount,
		"totalApiCalls", apiCalls)

	return targetCount, nil
}

// generateConfigMap creates a realistic ConfigMap resource
func (r *ScaleLoadConfigReconciler) generateConfigMap(config *scalev1.ScaleLoadConfig, namespace string, index int32) *corev1.ConfigMap {
	name := r.generateUniqueConfigMapName(namespace, int(index))

	// Generate realistic configuration data
	configData := map[string]string{
		"app.properties": generateAppProperties(),
		"config.yaml":    generateConfigYAML(),
		"settings.json":  generateSettingsJSON(),
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"scale.openshift.io/managed-by":    config.Name,
				"scale.openshift.io/resource-type": "configmap",
				"scale.openshift.io/created-by":    "sim-operator",
				"app.kubernetes.io/name":           fmt.Sprintf("sim-app-%d", index),
				"app.kubernetes.io/component":      "configuration",
			},
		},
		Data: configData,
	}
}

// manageSecrets creates and manages Secret resources
func (r *ScaleLoadConfigReconciler) manageSecrets(ctx context.Context,
	config *scalev1.ScaleLoadConfig, namespace string, targetCount int32) (int32, error) {

	log := r.Log.WithName("secret-manager").WithValues("namespace", namespace, "targetCount", targetCount)

	// Check if it's time to perform secret operations based on update frequency
	if !r.shouldPerformResourceOperation(namespace, "secrets", config.Spec.ResourceChurn.Secrets.UpdateFrequencyMin, config.Spec.ResourceChurn.Secrets.UpdateFrequencyMax) {
		log.V(1).Info("Skipping secret operations - not within update frequency window")
		return r.getCurrentResourceCount(ctx, config, namespace, "secrets")
	}

	log.V(1).Info("Performing secret operations within update frequency window")

	// Check maximum limit and adjust target count if needed
	effectiveTargetCount, err := r.checkMaximumLimit(ctx, config, "secrets", targetCount, config.Spec.ResourceChurn.Secrets.Maximum)
	if err != nil {
		return 0, fmt.Errorf("failed to check maximum limit for secrets: %w", err)
	}

	if effectiveTargetCount != targetCount {
		log.Info("Secret creation limited by maximum",
			"requestedCount", targetCount,
			"effectiveCount", effectiveTargetCount,
			"maximum", config.Spec.ResourceChurn.Secrets.Maximum)
		targetCount = effectiveTargetCount
	}

	secretList := &corev1.SecretList{}
	listOpts := &client.ListOptions{
		Namespace: namespace,
	}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "secret",
	}.ApplyToList(listOpts)

	if err := r.List(ctx, secretList, listOpts); err != nil {
		return 0, fmt.Errorf("failed to list Secrets: %w", err)
	}
	r.recordAPICall(config, 1) // List operation

	currentCount := len(secretList.Items)
	var created, deleted, apiCalls int32
	apiCalls++ // List operation

	// Update last operation time for this resource type in this namespace
	r.updateLastResourceOperation(namespace, "secrets")

	log.V(1).Info("Secret management starting", "current", currentCount, "target", targetCount)

	// Scale up if needed
	if int32(currentCount) < targetCount {
		toCreate := targetCount - int32(currentCount)
		for i := int32(currentCount); i < targetCount; i++ {
			secret := r.generateSecret(config, namespace, i)
			if err := r.Create(ctx, secret); err != nil {
				log.Error(err, "Failed to create Secret", "name", secret.Name, "created", created)
				return int32(currentCount) + created, fmt.Errorf("failed to create Secret: %w", err)
			}
			r.recordAPICall(config, 1) // Create operation
			created++
			apiCalls++
		}
		log.V(1).Info("Secrets created", "count", toCreate, "apiCalls", created)
	}

	// Scale down if needed
	if int32(currentCount) > targetCount {
		toDelete := int32(currentCount) - targetCount
		var deleted int32
		for i := int32(len(secretList.Items)) - 1; i >= targetCount && deleted < toDelete; i-- {
			if err := r.Delete(ctx, &secretList.Items[i]); err != nil {
				log.Error(err, "Failed to delete Secret", "name", secretList.Items[i].Name, "deleted", deleted)
				return int32(currentCount) - deleted, fmt.Errorf("failed to delete Secret: %w", err)
			}
			r.recordAPICall(config, 1) // Delete operation
			deleted++
			apiCalls++
		}
		log.V(1).Info("Secrets deleted", "count", deleted, "apiCalls", deleted)
	}

	// Simulate secret rotation
	objs := make([]client.Object, len(secretList.Items))
	for i, item := range secretList.Items {
		objs[i] = &item
	}
	updatedCount := r.performResourceChurn(ctx, config, objs, namespace, "secret")
	apiCalls += updatedCount

	log.V(1).Info("Secret management completed",
		"final", targetCount,
		"created", created,
		"deleted", deleted,
		"updated", updatedCount,
		"totalApiCalls", apiCalls)

	return targetCount, nil
}

// generateSecret creates a realistic Secret resource
func (r *ScaleLoadConfigReconciler) generateSecret(config *scalev1.ScaleLoadConfig, namespace string, index int32) *corev1.Secret {
	name := r.generateUniqueSecretName(namespace, int(index))

	secretData := map[string][]byte{
		"username":    []byte(fmt.Sprintf("user-%d", index)),
		"password":    []byte(generateRandomPassword(32)),
		"api-key":     []byte(generateRandomAPIKey()),
		"config.yaml": []byte(generateSecretConfig()),
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"scale.openshift.io/managed-by":    config.Name,
				"scale.openshift.io/resource-type": "secret",
				"scale.openshift.io/created-by":    "sim-operator",
				"app.kubernetes.io/name":           fmt.Sprintf("sim-app-%d", index),
				"app.kubernetes.io/component":      "credentials",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: secretData,
	}
}

// manageRoutes creates and manages Route resources (OpenShift specific)
func (r *ScaleLoadConfigReconciler) manageRoutes(ctx context.Context,
	config *scalev1.ScaleLoadConfig, namespace string, targetCount int32) (int32, error) {

	log := r.Log.WithName("route-manager").WithValues("namespace", namespace)

	// Add error resilience for OpenShift API server timeouts
	defer func() {
		if r := recover(); r != nil {
			log.Error(fmt.Errorf("route management panic: %v", r), "Route management failed due to panic, continuing with other resources")
		}
	}()

	// Check if it's time to perform route operations based on update frequency
	if !r.shouldPerformResourceOperation(namespace, "routes", config.Spec.ResourceChurn.Routes.UpdateFrequencyMin, config.Spec.ResourceChurn.Routes.UpdateFrequencyMax) {
		log.V(1).Info("Skipping route operations - not within update frequency window")
		// Return current count without performing any operations
		return r.getCurrentResourceCount(ctx, config, namespace, "routes")
	}

	log.V(1).Info("Performing route operations within update frequency window")

	// Check maximum limit and adjust target count if needed
	effectiveTargetCount, err := r.checkMaximumLimit(ctx, config, "routes", targetCount, config.Spec.ResourceChurn.Routes.Maximum)
	if err != nil {
		return 0, fmt.Errorf("failed to check maximum limit for routes: %w", err)
	}

	if effectiveTargetCount != targetCount {
		log.Info("Route creation limited by maximum",
			"requestedCount", targetCount,
			"effectiveCount", effectiveTargetCount,
			"maximum", config.Spec.ResourceChurn.Routes.Maximum)
		targetCount = effectiveTargetCount
	}

	routeList := &routev1.RouteList{}
	listOpts := &client.ListOptions{
		Namespace: namespace,
	}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "route",
	}.ApplyToList(listOpts)

	if err := r.List(ctx, routeList, listOpts); err != nil {
		if isAPIServerTimeoutError(err) {
			log.Info("API server timeout listing routes, skipping route management for this cycle",
				"error", err.Error(),
				"namespace", namespace)
			return 0, nil // Return 0 count but no error to continue with other resources
		}
		return 0, fmt.Errorf("failed to list Routes: %w", err)
	}
	r.recordAPICall(config, 1) // List operation

	currentCount := len(routeList.Items)

	log.V(1).Info("Route management starting", "current", currentCount, "target", targetCount)

	// Update last operation time for this resource type in this namespace
	r.updateLastResourceOperation(namespace, "routes")

	// Scale up if needed
	if int32(currentCount) < targetCount {
		toCreate := targetCount - int32(currentCount)
		var created int32
		for i := int32(currentCount); i < targetCount; i++ {
			// First create the Service that the Route will reference
			service := r.generateService(config, namespace, i)
			if err := r.Create(ctx, service); err != nil {
				// Check if the service already exists
				if !errors.IsAlreadyExists(err) {
					return int32(currentCount) + created, fmt.Errorf("failed to create Service: %w", err)
				}
			} else {
				r.recordAPICall(config, 1) // Service create operation
			}

			// Then create the Route that references the service by name
			route := r.generateRouteForService(config, namespace, i, service.Name)
			if err := r.Create(ctx, route); err != nil {
				return int32(currentCount) + created, fmt.Errorf("failed to create Route: %w", err)
			}
			r.recordAPICall(config, 1) // Route create operation
			created++
		}
		log.V(1).Info("Routes created", "count", toCreate, "apiCalls", created*2) // *2 for service+route
	}

	// Scale down if needed
	if int32(currentCount) > targetCount {
		toDelete := int32(currentCount) - targetCount

		// Check if we can perform deletion safely
		if !r.deletionManager.CanPerformDeletion("routes", config) {
			log.V(1).Info("Cannot perform route deletion due to safety constraints, skipping",
				"currentCount", currentCount, "targetCount", targetCount)
			return int32(currentCount), nil
		}

		// Use enhanced deletion if safe deletion is enabled
		if config.Spec.ResourceChurn.Routes.SafeDeletionEnabled {
			// Convert routes to client.Object slice
			routeObjects := make([]client.Object, len(routeList.Items))
			for i := range routeList.Items {
				routeObjects[i] = &routeList.Items[i]
			}

			if err := r.deletionManager.DeleteResourcesBatched(ctx, config, routeObjects, "routes", toDelete); err != nil {
				return int32(currentCount), fmt.Errorf("failed to delete routes with enhanced deletion: %w", err)
			}

			log.V(1).Info("Routes deleted with enhanced batching", "targetDeleted", toDelete)
		} else {
			// Use legacy deletion for backward compatibility
			deleted := r.deleteRoutesLegacy(ctx, config, routeList.Items, toDelete, log)
			log.V(1).Info("Routes deleted", "count", deleted, "apiCalls", deleted*2) // *2 for service+route
		}
	}

	return targetCount, nil
}

// deleteRoutesLegacy provides backward compatibility for route deletion
func (r *ScaleLoadConfigReconciler) deleteRoutesLegacy(ctx context.Context, config *scalev1.ScaleLoadConfig,
	routes []routev1.Route, toDelete int32, log logr.Logger) int32 {

	var deleted int32
	for i := int32(len(routes)) - 1; i >= 0 && deleted < toDelete; i-- {
		route := &routes[i]

		// Get the service name that this route references
		serviceName := route.Spec.To.Name

		// Delete the Route first
		if err := r.Delete(ctx, route); err != nil {
			log.Error(err, "Failed to delete Route", "name", route.Name)
			continue
		}
		r.recordAPICall(config, 1) // Route delete operation

		// Delete the specific service referenced by this route
		if serviceName != "" {
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: route.Namespace,
				},
			}
			if err := r.Delete(ctx, service); err != nil {
				if !errors.IsNotFound(err) {
					log.Error(err, "Failed to delete Service", "service", serviceName, "route", route.Name)
				}
			} else {
				r.recordAPICall(config, 1) // Service delete operation
			}
		}
		deleted++
	}

	return deleted
}

// generateRouteForService creates a realistic Route resource that references a specific service
func (r *ScaleLoadConfigReconciler) generateRouteForService(config *scalev1.ScaleLoadConfig, namespace string, index int32, serviceName string) *routev1.Route {
	name := r.generateUniqueRouteName(namespace, int(index))

	return &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"scale.openshift.io/managed-by":    config.Name,
				"scale.openshift.io/resource-type": "route",
				"scale.openshift.io/created-by":    "sim-operator",
				"app.kubernetes.io/name":           fmt.Sprintf("sim-app-%d", index),
				"app.kubernetes.io/component":      "frontend",
			},
		},
		Spec: routev1.RouteSpec{
			To: routev1.RouteTargetReference{
				Kind: "Service",
				Name: serviceName,
			},
			Port: &routev1.RoutePort{
				TargetPort: intstr.FromInt(8080),
			},
			TLS: &routev1.TLSConfig{
				Termination: routev1.TLSTerminationEdge,
			},
		},
	}
}

// generateService creates a Service resource for the Route to reference
func (r *ScaleLoadConfigReconciler) generateService(config *scalev1.ScaleLoadConfig, namespace string, index int32) *corev1.Service {
	name := r.generateUniqueServiceName(namespace, int(index))

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"scale.openshift.io/managed-by":    config.Name,
				"scale.openshift.io/resource-type": "service",
				"scale.openshift.io/created-by":    "sim-operator",
				"app.kubernetes.io/name":           fmt.Sprintf("sim-app-%d", index),
				"app.kubernetes.io/component":      "backend",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app.kubernetes.io/name": fmt.Sprintf("sim-app-%d", index),
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       8080,
					TargetPort: intstr.FromInt(8080),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
}

// manageImageStreams creates and manages ImageStream resources
func (r *ScaleLoadConfigReconciler) manageImageStreams(ctx context.Context,
	config *scalev1.ScaleLoadConfig, namespace string, targetCount int32) (int32, error) {

	log := r.Log.WithName("imagestream-manager").WithValues("namespace", namespace, "targetCount", targetCount)

	// Check if it's time to perform imagestream operations based on update frequency
	if !r.shouldPerformResourceOperation(namespace, "imageStreams", config.Spec.ResourceChurn.ImageStreams.UpdateFrequencyMin, config.Spec.ResourceChurn.ImageStreams.UpdateFrequencyMax) {
		log.V(1).Info("Skipping imagestream operations - not within update frequency window")
		return r.getCurrentResourceCount(ctx, config, namespace, "imageStreams")
	}

	log.V(1).Info("Performing imagestream operations within update frequency window")

	// Check maximum limit and adjust target count if needed
	effectiveTargetCount, err := r.checkMaximumLimit(ctx, config, "imageStreams", targetCount, config.Spec.ResourceChurn.ImageStreams.Maximum)
	if err != nil {
		return 0, fmt.Errorf("failed to check maximum limit for imageStreams: %w", err)
	}

	if effectiveTargetCount != targetCount {
		log.Info("ImageStream creation limited by maximum",
			"requestedCount", targetCount,
			"effectiveCount", effectiveTargetCount,
			"maximum", config.Spec.ResourceChurn.ImageStreams.Maximum)
		targetCount = effectiveTargetCount
	}

	imageStreamList := &imagev1.ImageStreamList{}
	listOpts := &client.ListOptions{
		Namespace: namespace,
	}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "imagestream",
	}.ApplyToList(listOpts)

	if err := r.List(ctx, imageStreamList, listOpts); err != nil {
		if isAPIServerTimeoutError(err) {
			log.Info("API server timeout listing imagestreams, skipping imagestream management for this cycle",
				"error", err.Error(),
				"namespace", namespace)
			return 0, nil // Return 0 count but no error to continue with other resources
		}
		return 0, fmt.Errorf("failed to list ImageStreams: %w", err)
	}
	r.recordAPICall(config, 1) // List operation

	currentCount := len(imageStreamList.Items)

	log.V(1).Info("ImageStream management starting", "current", currentCount, "target", targetCount)

	// Scale up if needed
	if int32(currentCount) < targetCount {
		toCreate := targetCount - int32(currentCount)
		var created int32
		for i := int32(currentCount); i < targetCount; i++ {
			imageStream := r.generateImageStream(config, namespace, i)
			if err := r.Create(ctx, imageStream); err != nil {
				return int32(currentCount) + created, fmt.Errorf("failed to create ImageStream: %w", err)
			}
			r.recordAPICall(config, 1) // Create operation
			created++
		}
		log.V(1).Info("ImageStreams created", "count", toCreate, "apiCalls", created)
	}

	// Scale down if needed
	if int32(currentCount) > targetCount {
		toDelete := int32(currentCount) - targetCount

		// Check if we can perform deletion safely
		if !r.deletionManager.CanPerformDeletion("imageStreams", config) {
			log.V(1).Info("Cannot perform imagestream deletion due to safety constraints, skipping",
				"currentCount", currentCount, "targetCount", targetCount)
			return int32(currentCount), nil
		}

		// Use enhanced deletion if safe deletion is enabled
		if config.Spec.ResourceChurn.ImageStreams.SafeDeletionEnabled {
			// Convert imagestreams to client.Object slice
			imageStreamObjects := make([]client.Object, len(imageStreamList.Items))
			for i := range imageStreamList.Items {
				imageStreamObjects[i] = &imageStreamList.Items[i]
			}

			if err := r.deletionManager.DeleteResourcesBatched(ctx, config, imageStreamObjects, "imageStreams", toDelete); err != nil {
				return int32(currentCount), fmt.Errorf("failed to delete imagestreams with enhanced deletion: %w", err)
			}

			log.V(1).Info("ImageStreams deleted with enhanced batching", "targetDeleted", toDelete)
		} else {
			// Use legacy deletion for backward compatibility
			deleted := r.deleteImageStreamsLegacy(ctx, config, imageStreamList.Items, toDelete, log)
			log.V(1).Info("ImageStreams deleted", "count", deleted, "apiCalls", deleted)
		}
	}

	return targetCount, nil
}

// deleteImageStreamsLegacy provides backward compatibility for imagestream deletion
func (r *ScaleLoadConfigReconciler) deleteImageStreamsLegacy(ctx context.Context, config *scalev1.ScaleLoadConfig,
	imageStreams []imagev1.ImageStream, toDelete int32, log logr.Logger) int32 {

	var deleted int32
	for i := int32(len(imageStreams)) - 1; i >= 0 && deleted < toDelete; i-- {
		imageStream := &imageStreams[i]

		if err := r.Delete(ctx, imageStream); err != nil {
			log.Error(err, "Failed to delete ImageStream", "name", imageStream.Name)
			continue
		}
		r.recordAPICall(config, 1) // Delete operation
		deleted++
	}

	return deleted
}

// generateImageStream creates a realistic ImageStream resource
func (r *ScaleLoadConfigReconciler) generateImageStream(config *scalev1.ScaleLoadConfig, namespace string, index int32) *imagev1.ImageStream {
	name := r.generateUniqueImageStreamName(namespace, int(index))

	return &imagev1.ImageStream{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"scale.openshift.io/managed-by":    config.Name,
				"scale.openshift.io/resource-type": "imagestream",
				"scale.openshift.io/created-by":    "sim-operator",
				"app.kubernetes.io/name":           fmt.Sprintf("sim-app-%d", index),
				"app.kubernetes.io/component":      "image",
			},
		},
		Spec: imagev1.ImageStreamSpec{
			Tags: []imagev1.TagReference{
				{
					Name: "latest",
					From: &corev1.ObjectReference{
						Kind: "DockerImage",
						Name: "registry.redhat.io/ubi8/ubi-minimal:latest", // Use a more stable registry
					},
					ImportPolicy: imagev1.TagImportPolicy{
						Scheduled: false, // CRITICAL: Disable automatic imports to avoid registry operations
						Insecure:  false,
					},
					ReferencePolicy: imagev1.TagReferencePolicy{
						Type: imagev1.LocalTagReferencePolicy, // Keep references local to avoid external lookups
					},
				},
			},
			LookupPolicy: imagev1.ImageLookupPolicy{
				Local: true, // Keep image lookups local to avoid external registry calls
			},
		},
	}
}

// manageBuildConfigs creates and manages BuildConfig resources
func (r *ScaleLoadConfigReconciler) manageBuildConfigs(ctx context.Context,
	config *scalev1.ScaleLoadConfig, namespace string, targetCount int32) (int32, error) {

	log := r.Log.WithName("buildconfig-manager").WithValues("namespace", namespace, "targetCount", targetCount)

	// Check if it's time to perform buildconfig operations based on update frequency
	if !r.shouldPerformResourceOperation(namespace, "buildConfigs", config.Spec.ResourceChurn.BuildConfigs.UpdateFrequencyMin, config.Spec.ResourceChurn.BuildConfigs.UpdateFrequencyMax) {
		log.V(1).Info("Skipping buildconfig operations - not within update frequency window")
		return r.getCurrentResourceCount(ctx, config, namespace, "buildConfigs")
	}

	log.V(1).Info("Performing buildconfig operations within update frequency window")

	// Check maximum limit and adjust target count if needed
	effectiveTargetCount, err := r.checkMaximumLimit(ctx, config, "buildConfigs", targetCount, config.Spec.ResourceChurn.BuildConfigs.Maximum)
	if err != nil {
		return 0, fmt.Errorf("failed to check maximum limit for buildConfigs: %w", err)
	}

	if effectiveTargetCount != targetCount {
		log.Info("BuildConfig creation limited by maximum",
			"requestedCount", targetCount,
			"effectiveCount", effectiveTargetCount,
			"maximum", config.Spec.ResourceChurn.BuildConfigs.Maximum)
		targetCount = effectiveTargetCount
	}

	buildConfigList := &buildv1.BuildConfigList{}
	listOpts := &client.ListOptions{
		Namespace: namespace,
	}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "buildconfig",
	}.ApplyToList(listOpts)

	if err := r.List(ctx, buildConfigList, listOpts); err != nil {
		if isAPIServerTimeoutError(err) {
			log.Info("API server timeout listing buildconfigs, skipping buildconfig management for this cycle",
				"error", err.Error(),
				"namespace", namespace)
			return 0, nil // Return 0 count but no error to continue with other resources
		}
		return 0, fmt.Errorf("failed to list BuildConfigs: %w", err)
	}
	r.recordAPICall(config, 1) // List operation

	currentCount := len(buildConfigList.Items)

	log.V(1).Info("BuildConfig management starting", "current", currentCount, "target", targetCount)

	// Scale up if needed
	if int32(currentCount) < targetCount {
		toCreate := targetCount - int32(currentCount)
		var created int32
		for i := int32(currentCount); i < targetCount; i++ {
			buildConfig := r.generateBuildConfig(config, namespace, i)
			if err := r.Create(ctx, buildConfig); err != nil {
				return int32(currentCount) + created, fmt.Errorf("failed to create BuildConfig: %w", err)
			}
			r.recordAPICall(config, 1) // Create operation
			created++
		}
		log.V(1).Info("BuildConfigs created", "count", toCreate, "apiCalls", created)
	}

	// Scale down if needed
	if int32(currentCount) > targetCount {
		toDelete := int32(currentCount) - targetCount

		// Check if we can perform deletion safely
		if !r.deletionManager.CanPerformDeletion("buildConfigs", config) {
			log.V(1).Info("Cannot perform buildconfig deletion due to safety constraints, skipping",
				"currentCount", currentCount, "targetCount", targetCount)
			return int32(currentCount), nil
		}

		// Use enhanced deletion if safe deletion is enabled
		if config.Spec.ResourceChurn.BuildConfigs.SafeDeletionEnabled {
			// Convert buildconfigs to client.Object slice
			buildConfigObjects := make([]client.Object, len(buildConfigList.Items))
			for i := range buildConfigList.Items {
				buildConfigObjects[i] = &buildConfigList.Items[i]
			}

			if err := r.deletionManager.DeleteResourcesBatched(ctx, config, buildConfigObjects, "buildConfigs", toDelete); err != nil {
				return int32(currentCount), fmt.Errorf("failed to delete buildconfigs with enhanced deletion: %w", err)
			}

			log.V(1).Info("BuildConfigs deleted with enhanced batching", "targetDeleted", toDelete)
		} else {
			// Use legacy deletion for backward compatibility
			deleted := r.deleteBuildConfigsLegacy(ctx, config, buildConfigList.Items, toDelete, log)
			log.V(1).Info("BuildConfigs deleted", "count", deleted, "apiCalls", deleted)
		}
	}

	return targetCount, nil
}

// deleteBuildConfigsLegacy provides backward compatibility for buildconfig deletion
func (r *ScaleLoadConfigReconciler) deleteBuildConfigsLegacy(ctx context.Context, config *scalev1.ScaleLoadConfig,
	buildConfigs []buildv1.BuildConfig, toDelete int32, log logr.Logger) int32 {

	var deleted int32
	for i := int32(len(buildConfigs)) - 1; i >= 0 && deleted < toDelete; i-- {
		buildConfig := &buildConfigs[i]

		if err := r.Delete(ctx, buildConfig); err != nil {
			log.Error(err, "Failed to delete BuildConfig", "name", buildConfig.Name)
			continue
		}
		r.recordAPICall(config, 1) // Delete operation
		deleted++
	}

	return deleted
}

// generateBuildConfig creates a realistic BuildConfig resource
func (r *ScaleLoadConfigReconciler) generateBuildConfig(config *scalev1.ScaleLoadConfig, namespace string, index int32) *buildv1.BuildConfig {
	name := r.generateUniqueBuildConfigName(namespace, int(index))
	imageStreamName := r.generateUniqueImageStreamName(namespace, int(index))

	return &buildv1.BuildConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"scale.openshift.io/managed-by":    config.Name,
				"scale.openshift.io/resource-type": "buildconfig",
				"scale.openshift.io/created-by":    "sim-operator",
				"app.kubernetes.io/name":           fmt.Sprintf("sim-app-%d", index),
				"app.kubernetes.io/component":      "build",
			},
		},
		Spec: buildv1.BuildConfigSpec{
			CommonSpec: buildv1.CommonSpec{
				Source: buildv1.BuildSource{
					Type: buildv1.BuildSourceNone, // CRITICAL: No source to avoid git operations
				},
				Strategy: buildv1.BuildStrategy{
					Type: buildv1.CustomBuildStrategyType, // Use custom strategy to avoid actual docker builds
					CustomStrategy: &buildv1.CustomBuildStrategy{
						From: corev1.ObjectReference{
							Kind: "ImageStreamTag",
							Name: "registry.redhat.io/ubi8/ubi-minimal:latest",
						},
						PullSecret: nil, // No pull secret needed
					},
				},
				Output: buildv1.BuildOutput{
					To: &corev1.ObjectReference{
						Kind: "ImageStreamTag",
						Name: fmt.Sprintf("%s:latest", imageStreamName),
					},
				},
			},
			// CRITICAL: No triggers to prevent automatic builds
			Triggers: []buildv1.BuildTriggerPolicy{}, // Empty triggers = no automatic builds
		},
	}
}

// manageEvents creates realistic Event resources to simulate cluster activity
func (r *ScaleLoadConfigReconciler) manageEvents(ctx context.Context,
	config *scalev1.ScaleLoadConfig, namespace string) (int32, error) {

	log := r.Log.WithName("event-manager").WithValues("namespace", namespace)

	// Calculate how many events to generate based on time and rate
	eventsPerHour := config.Spec.ResourceChurn.Events.EventsPerNodePerHour
	if eventsPerHour <= 0 {
		eventsPerHour = 50 // Default from analysis
	}

	// Generate events at the calculated rate
	timeSinceLastReconcile := time.Since(r.lastReconcileTime)
	if r.lastReconcileTime.IsZero() {
		timeSinceLastReconcile = 1 * time.Minute // Default for first run
	}

	eventsToCreate := int32(float64(eventsPerHour) * timeSinceLastReconcile.Hours())
	if eventsToCreate > 10 {
		eventsToCreate = 10 // Cap to prevent spam
	}

	log.V(1).Info("Event generation starting",
		"eventsPerHour", eventsPerHour,
		"timeSinceLastReconcile", timeSinceLastReconcile.String(),
		"targetEvents", eventsToCreate)

	var createdCount, failedCount, apiCalls int32
	for i := int32(0); i < eventsToCreate; i++ {
		event := r.generateEvent(config, namespace, i)
		if err := r.Create(ctx, event); err != nil {
			// Events often conflict on creation, which is normal
			failedCount++
			log.V(2).Info("Event creation failed (normal)", "error", err.Error())
		} else {
			createdCount++
		}
		apiCalls++
	}

	// Calculate success rate safely to avoid division by zero
	var successRate string
	if eventsToCreate > 0 {
		successRate = fmt.Sprintf("%.1f%%", float64(createdCount)/float64(eventsToCreate)*100)
	} else {
		successRate = "N/A (no events to create)"
	}

	log.V(1).Info("Event generation completed",
		"attempted", eventsToCreate,
		"created", createdCount,
		"failed", failedCount,
		"successRate", successRate,
		"apiCalls", apiCalls)

	return createdCount, nil
}

// generateEvent creates realistic Event resources
func (r *ScaleLoadConfigReconciler) generateEvent(config *scalev1.ScaleLoadConfig, namespace string, index int32) *corev1.Event {
	eventTypes := []scalev1.EventTypeConfig{
		{Type: "Normal", Reason: "Started", Message: "Container started successfully", Weight: 30},
		{Type: "Normal", Reason: "Created", Message: "Created container %s", Weight: 25},
		{Type: "Normal", Reason: "Pulled", Message: "Successfully pulled image", Weight: 20},
		{Type: "Warning", Reason: "FailedMount", Message: "Unable to mount volumes", Weight: 10},
		{Type: "Warning", Reason: "FailedScheduling", Message: "Pod scheduling failed", Weight: 8},
		{Type: "Normal", Reason: "Scheduled", Message: "Successfully assigned pod", Weight: 7},
	}

	// Override with custom event types if provided
	if len(config.Spec.ResourceChurn.Events.EventTypes) > 0 {
		eventTypes = config.Spec.ResourceChurn.Events.EventTypes
	}

	// Select random event type based on weights
	selectedEvent := selectWeightedEventType(eventTypes)

	involvedObject := corev1.ObjectReference{
		Kind:       "Pod",
		Namespace:  namespace,
		Name:       fmt.Sprintf("sim-pod-%d", index),
		APIVersion: "v1",
	}

	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("sim-event-%d-%d", index, time.Now().Unix()),
			Namespace: namespace,
			Labels: map[string]string{
				"scale.openshift.io/managed-by":    config.Name,
				"scale.openshift.io/resource-type": "event",
				"scale.openshift.io/created-by":    "sim-operator",
			},
		},
		InvolvedObject: involvedObject,
		Reason:         selectedEvent.Reason,
		Message:        fmt.Sprintf(selectedEvent.Message, fmt.Sprintf("container-%d", index)),
		Type:           selectedEvent.Type,
		Source: corev1.EventSource{
			Component: "sim-operator",
		},
		FirstTimestamp: metav1.NewTime(time.Now()),
		LastTimestamp:  metav1.NewTime(time.Now()),
		Count:          1,
	}
}

// Helper functions for generating realistic data

func generateAppProperties() string {
	return fmt.Sprintf(`# Application Configuration
app.name=sim-generator-app
app.version=1.0.%d
app.debug=false
app.port=8080
app.threads=%d
app.memory.max=512m
database.url=jdbc:postgresql://db:5432/app
database.pool.size=%d`,
		mathrand.Intn(100), mathrand.Intn(10)+1, mathrand.Intn(20)+5)
}

func generateConfigYAML() string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
metadata:
  name: app-config
spec:
  replicas: %d
  resources:
    requests:
      cpu: %dm
      memory: %dMi
  environment:
    - name: LOG_LEVEL
      value: INFO
    - name: INSTANCE_ID
      value: "%s"`,
		mathrand.Intn(5)+1, mathrand.Intn(500)+100, mathrand.Intn(512)+128, generateRandomString(8))
}

func generateSettingsJSON() string {
	return fmt.Sprintf(`{
  "app": {
    "name": "sim-generator",
    "version": "1.0.%d",
    "environment": "production"
  },
  "features": {
    "enableMetrics": true,
    "enableTracing": %t,
    "cacheSize": %d
  },
  "networking": {
    "timeout": %d,
    "retries": %d
  }
}`, mathrand.Intn(100), mathrand.Intn(2) == 1, mathrand.Intn(1000)+100, mathrand.Intn(30)+5, mathrand.Intn(5)+1)
}

func generateRandomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, length)
	for i := range result {
		result[i] = charset[mathrand.Intn(len(charset))]
	}
	return string(result)
}

// generateUniquePodName creates a unique pod name to avoid conflicts
func (r *ScaleLoadConfigReconciler) generateUniquePodName(namespace string, index int) string {
	// Include timestamp and random suffix to ensure uniqueness
	timestamp := time.Now().Unix()
	randomSuffix := generateRandomString(4)
	return fmt.Sprintf("sim-pod-%d-%d-%s", index, timestamp, randomSuffix)
}

// generateUniqueConfigMapName creates a unique configmap name to avoid conflicts
func (r *ScaleLoadConfigReconciler) generateUniqueConfigMapName(namespace string, index int) string {
	timestamp := time.Now().Unix()
	randomSuffix := generateRandomString(4)
	return fmt.Sprintf("sim-configmap-%d-%d-%s", index, timestamp, randomSuffix)
}

// generateUniqueSecretName creates a unique secret name to avoid conflicts
func (r *ScaleLoadConfigReconciler) generateUniqueSecretName(namespace string, index int) string {
	timestamp := time.Now().Unix()
	randomSuffix := generateRandomString(4)
	return fmt.Sprintf("sim-secret-%d-%d-%s", index, timestamp, randomSuffix)
}

// generateUniqueRouteName creates a unique route name to avoid conflicts
func (r *ScaleLoadConfigReconciler) generateUniqueRouteName(namespace string, index int) string {
	timestamp := time.Now().Unix()
	randomSuffix := generateRandomString(4)
	return fmt.Sprintf("sim-route-%d-%d-%s", index, timestamp, randomSuffix)
}

// generateUniqueServiceName creates a unique service name to avoid conflicts
func (r *ScaleLoadConfigReconciler) generateUniqueServiceName(namespace string, index int) string {
	timestamp := time.Now().Unix()
	randomSuffix := generateRandomString(4)
	return fmt.Sprintf("sim-service-%d-%d-%s", index, timestamp, randomSuffix)
}

// generateUniqueImageStreamName creates a unique imagestream name to avoid conflicts
func (r *ScaleLoadConfigReconciler) generateUniqueImageStreamName(namespace string, index int) string {
	timestamp := time.Now().Unix()
	randomSuffix := generateRandomString(4)
	return fmt.Sprintf("sim-imagestream-%d-%d-%s", index, timestamp, randomSuffix)
}

// generateUniqueBuildConfigName creates a unique buildconfig name to avoid conflicts
func (r *ScaleLoadConfigReconciler) generateUniqueBuildConfigName(namespace string, index int) string {
	timestamp := time.Now().Unix()
	randomSuffix := generateRandomString(4)
	return fmt.Sprintf("sim-buildconfig-%d-%d-%s", index, timestamp, randomSuffix)
}

func generateRandomPassword(length int) string {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		// Fallback to math/rand if crypto/rand fails
		for i := range bytes {
			bytes[i] = byte(mathrand.Intn(256))
		}
	}
	return base64.URLEncoding.EncodeToString(bytes)[:length]
}

func generateRandomAPIKey() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		// Fallback to math/rand if crypto/rand fails
		for i := range bytes {
			bytes[i] = byte(mathrand.Intn(256))
		}
	}
	return base64.URLEncoding.EncodeToString(bytes)
}

func generateSecretConfig() string {
	return fmt.Sprintf(`apiVersion: v1
secret:
  database:
    username: user_%s
    password: %s
    host: db.internal
    port: 5432
  api:
    key: %s
    endpoint: https://api.example.com`,
		generateRandomString(6), generateRandomPassword(16), generateRandomAPIKey())
}

func selectWeightedEventType(events []scalev1.EventTypeConfig) scalev1.EventTypeConfig {
	totalWeight := int32(0)
	for _, event := range events {
		totalWeight += event.Weight
	}

	randomValue := mathrand.Int31n(totalWeight)
	currentWeight := int32(0)

	for _, event := range events {
		currentWeight += event.Weight
		if randomValue < currentWeight {
			return event
		}
	}

	// Fallback to first event
	return events[0]
}

// checkNamespaceStatus checks namespace existence and returns readiness status with phase info
func (r *ScaleLoadConfigReconciler) checkNamespaceStatus(ctx context.Context, namespaceName string) (bool, string) {
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: namespaceName}, namespace); err != nil {
		return false, "NotFound"
	}

	// Consider namespace ready if it exists and doesn't have a terminating phase
	phase := string(namespace.Status.Phase)
	if phase == "" {
		phase = "Unknown"
	}

	// Namespace is ready if it's Active or if phase is empty (newly created)
	ready := namespace.Status.Phase == corev1.NamespaceActive || namespace.Status.Phase == ""
	return ready, phase
}

// isNamespaceReady checks if a namespace exists and is in Active phase
func (r *ScaleLoadConfigReconciler) isNamespaceReady(ctx context.Context, namespaceName string) bool {
	ready, _ := r.checkNamespaceStatus(ctx, namespaceName)
	return ready
}

// performResourceChurn simulates realistic resource update patterns
func (r *ScaleLoadConfigReconciler) performResourceChurn(ctx context.Context,
	config *scalev1.ScaleLoadConfig, resources []client.Object, namespace, resourceType string) int32 {

	if len(resources) == 0 {
		return 0
	}

	// More aggressive resource churn to meet API call targets
	updateChance := 0.4 // 40% chance per reconcile cycle (increased from 10%)
	var updatedCount int32

	for _, resource := range resources {
		if mathrand.Float64() < updateChance {
			// Simulate resource update by adding a timestamp annotation
			if resource.GetAnnotations() == nil {
				resource.SetAnnotations(make(map[string]string))
			}

			annotations := resource.GetAnnotations()
			annotations["scale.openshift.io/last-churn"] = time.Now().Format(time.RFC3339)
			annotations["scale.openshift.io/churn-iteration"] = fmt.Sprintf("%d", mathrand.Intn(1000))
			resource.SetAnnotations(annotations)

			if err := r.Update(ctx, resource); err != nil {
				r.Log.V(1).Info("Failed to update resource for churn",
					"resource", resource.GetName(), "type", resourceType, "error", err)
			} else {
				updatedCount++
				// Record the API call for metrics tracking
				r.recordAPICall(config, 1)
			}
		}
	}

	if updatedCount > 0 {
		r.Log.V(1).Info("Resource churn completed",
			"type", resourceType,
			"namespace", namespace,
			"totalResources", len(resources),
			"updated", updatedCount,
			"updateRate", fmt.Sprintf("%.1f%%", float64(updatedCount)/float64(len(resources))*100))
	}

	return updatedCount
}

// checkMaximumLimit checks if we've reached the maximum limit for a resource type
// Returns the effective target count (may be less than requested if at limit)
func (r *ScaleLoadConfigReconciler) checkMaximumLimit(ctx context.Context,
	config *scalev1.ScaleLoadConfig, resourceType string, requestedCount int32,
	maximumLimit int32) (int32, error) {

	// If maximum is 0, no limit
	if maximumLimit == 0 {
		return requestedCount, nil
	}

	// Count existing resources of this type across all managed namespaces
	var totalExisting int32

	// Use cached list from current reconcile when set to avoid repeated List calls
	namespaces := r.currentManagedNamespaces
	if namespaces == nil {
		var err error
		namespaces, err = r.getManagedNamespaces(ctx, config)
		if err != nil {
			return requestedCount, err
		}
	}

	// Count resources across all namespaces
	for _, ns := range namespaces {
		var count int32
		switch resourceType {
		case "pods":
			count, _ = r.countExistingPods(ctx, config, ns.Name)
		case "configMaps":
			count, _ = r.countExistingConfigMaps(ctx, config, ns.Name)
		case "secrets":
			count, _ = r.countExistingSecrets(ctx, config, ns.Name)
		case "routes":
			count, _ = r.countExistingRoutes(ctx, config, ns.Name)
		case "imageStreams":
			count, _ = r.countExistingImageStreams(ctx, config, ns.Name)
		case "buildConfigs":
			count, _ = r.countExistingBuildConfigs(ctx, config, ns.Name)
		}
		totalExisting += count
	}

	// If we're already at or above the limit, don't create any new resources
	if totalExisting >= maximumLimit {
		return 0, nil
	}

	// If creating the requested count would exceed the limit, reduce it
	if totalExisting+requestedCount > maximumLimit {
		return maximumLimit - totalExisting, nil
	}

	// We're under the limit, create the requested count
	return requestedCount, nil
}

// Helper functions to count existing resources
func (r *ScaleLoadConfigReconciler) countExistingPods(ctx context.Context, config *scalev1.ScaleLoadConfig, namespace string) (int32, error) {
	podList := &corev1.PodList{}
	listOpts := &client.ListOptions{Namespace: namespace}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "pod",
	}.ApplyToList(listOpts)
	if err := r.List(ctx, podList, listOpts); err != nil {
		return 0, err
	}
	return int32(len(podList.Items)), nil
}

func (r *ScaleLoadConfigReconciler) countExistingConfigMaps(ctx context.Context, config *scalev1.ScaleLoadConfig, namespace string) (int32, error) {
	list := &corev1.ConfigMapList{}
	listOpts := &client.ListOptions{Namespace: namespace}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "configmap",
	}.ApplyToList(listOpts)
	if err := r.List(ctx, list, listOpts); err != nil {
		return 0, err
	}
	return int32(len(list.Items)), nil
}

func (r *ScaleLoadConfigReconciler) countExistingSecrets(ctx context.Context, config *scalev1.ScaleLoadConfig, namespace string) (int32, error) {
	list := &corev1.SecretList{}
	listOpts := &client.ListOptions{Namespace: namespace}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "secret",
	}.ApplyToList(listOpts)
	if err := r.List(ctx, list, listOpts); err != nil {
		return 0, err
	}
	return int32(len(list.Items)), nil
}

func (r *ScaleLoadConfigReconciler) countExistingRoutes(ctx context.Context, config *scalev1.ScaleLoadConfig, namespace string) (int32, error) {
	list := &routev1.RouteList{}
	listOpts := &client.ListOptions{Namespace: namespace}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "route",
	}.ApplyToList(listOpts)
	if err := r.List(ctx, list, listOpts); err != nil {
		return 0, err
	}
	return int32(len(list.Items)), nil
}

func (r *ScaleLoadConfigReconciler) countExistingImageStreams(ctx context.Context, config *scalev1.ScaleLoadConfig, namespace string) (int32, error) {
	list := &imagev1.ImageStreamList{}
	listOpts := &client.ListOptions{Namespace: namespace}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "imagestream",
	}.ApplyToList(listOpts)
	if err := r.List(ctx, list, listOpts); err != nil {
		return 0, err
	}
	return int32(len(list.Items)), nil
}

func (r *ScaleLoadConfigReconciler) countExistingBuildConfigs(ctx context.Context, config *scalev1.ScaleLoadConfig, namespace string) (int32, error) {
	list := &buildv1.BuildConfigList{}
	listOpts := &client.ListOptions{Namespace: namespace}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "buildconfig",
	}.ApplyToList(listOpts)
	if err := r.List(ctx, list, listOpts); err != nil {
		return 0, err
	}
	return int32(len(list.Items)), nil
}

// recordAPICall records API calls for simplified rate tracking and metrics
func (r *ScaleLoadConfigReconciler) recordAPICall(config *scalev1.ScaleLoadConfig, callCount int32) {
	now := time.Now()

	// Simplified rate tracking (resets every minute)
	if r.lastRateReset.IsZero() || now.Sub(r.lastRateReset) >= time.Minute {
		r.apiCallsThisMinute = 0
		r.lastRateReset = now
	}
	r.apiCallsThisMinute += callCount

	// Cumulative metrics counter (for accurate reporting)
	r.totalAPICallsMade += int64(callCount)

	// Debug logging every 5000 calls to reduce spam at scale
	if r.totalAPICallsMade%5000 == 0 {
		log := r.Log.WithName("api-call-tracker")
		log.Info("API calls milestone",
			"totalAPICallsMade", r.totalAPICallsMade,
			"apiCallsThisMinute", r.apiCallsThisMinute)
	}

	// Record prometheus metrics
	r.APICallRate.Observe(float64(callCount))
}

// Resource timing tracking for frequency-based operations
var resourceLastOperationTimes = make(map[string]map[string]time.Time) // namespace -> resourceType -> lastTime
var resourceTimingMutex sync.RWMutex

// shouldPerformResourceOperation checks if enough time has passed since last operation for this resource type
func (r *ScaleLoadConfigReconciler) shouldPerformResourceOperation(namespace, resourceType string, minFrequency, maxFrequency int32) bool {
	resourceTimingMutex.RLock()
	defer resourceTimingMutex.RUnlock()

	// Initialize namespace map if it doesn't exist
	if resourceLastOperationTimes[namespace] == nil {
		return true // First time, always perform operation
	}

	lastTime, exists := resourceLastOperationTimes[namespace][resourceType]
	if !exists {
		return true // First time for this resource type, always perform operation
	}

	// Calculate random interval within the specified range
	intervalRange := maxFrequency - minFrequency
	var randomInterval int32
	if intervalRange > 0 {
		randomInterval = minFrequency + mathrand.Int31n(intervalRange)
	} else {
		randomInterval = minFrequency
	}

	timeSinceLastOperation := time.Since(lastTime)
	requiredInterval := time.Duration(randomInterval) * time.Second

	shouldPerform := timeSinceLastOperation >= requiredInterval

	if shouldPerform {
		r.Log.V(1).Info("Resource operation timing check",
			"namespace", namespace,
			"resourceType", resourceType,
			"timeSinceLastOperation", timeSinceLastOperation.String(),
			"requiredInterval", requiredInterval.String(),
			"shouldPerform", shouldPerform)
	}

	return shouldPerform
}

// updateLastResourceOperation updates the last operation time for a resource type in a namespace
func (r *ScaleLoadConfigReconciler) updateLastResourceOperation(namespace, resourceType string) {
	resourceTimingMutex.Lock()
	defer resourceTimingMutex.Unlock()

	// Initialize namespace map if it doesn't exist
	if resourceLastOperationTimes[namespace] == nil {
		resourceLastOperationTimes[namespace] = make(map[string]time.Time)
	}

	resourceLastOperationTimes[namespace][resourceType] = time.Now()

	r.Log.V(1).Info("Updated resource operation timestamp",
		"namespace", namespace,
		"resourceType", resourceType,
		"timestamp", time.Now().Format(time.RFC3339))
}

// getCurrentResourceCount gets the current count of resources without performing any operations
func (r *ScaleLoadConfigReconciler) getCurrentResourceCount(ctx context.Context, config *scalev1.ScaleLoadConfig, namespace, resourceType string) (int32, error) {
	switch resourceType {
	case "routes":
		routeList := &routev1.RouteList{}
		listOpts := &client.ListOptions{Namespace: namespace}
		client.MatchingLabels{
			"scale.openshift.io/managed-by":    config.Name,
			"scale.openshift.io/resource-type": "route",
		}.ApplyToList(listOpts)
		if err := r.List(ctx, routeList, listOpts); err != nil {
			return 0, fmt.Errorf("failed to list routes: %w", err)
		}
		// Note: We still need this LIST call to get current count, but we're not performing any modifications
		r.recordAPICall(config, 1)
		return int32(len(routeList.Items)), nil
	case "configMaps":
		list := &corev1.ConfigMapList{}
		listOpts := &client.ListOptions{Namespace: namespace}
		client.MatchingLabels{
			"scale.openshift.io/managed-by":    config.Name,
			"scale.openshift.io/resource-type": "configmap",
		}.ApplyToList(listOpts)
		if err := r.List(ctx, list, listOpts); err != nil {
			return 0, fmt.Errorf("failed to list configmaps: %w", err)
		}
		r.recordAPICall(config, 1)
		return int32(len(list.Items)), nil
	case "secrets":
		list := &corev1.SecretList{}
		listOpts := &client.ListOptions{Namespace: namespace}
		client.MatchingLabels{
			"scale.openshift.io/managed-by":    config.Name,
			"scale.openshift.io/resource-type": "secret",
		}.ApplyToList(listOpts)
		if err := r.List(ctx, list, listOpts); err != nil {
			return 0, fmt.Errorf("failed to list secrets: %w", err)
		}
		r.recordAPICall(config, 1)
		return int32(len(list.Items)), nil
	case "imageStreams":
		list := &imagev1.ImageStreamList{}
		listOpts := &client.ListOptions{Namespace: namespace}
		client.MatchingLabels{
			"scale.openshift.io/managed-by":    config.Name,
			"scale.openshift.io/resource-type": "imagestream",
		}.ApplyToList(listOpts)
		if err := r.List(ctx, list, listOpts); err != nil {
			return 0, fmt.Errorf("failed to list imagestreams: %w", err)
		}
		r.recordAPICall(config, 1)
		return int32(len(list.Items)), nil
	case "buildConfigs":
		list := &buildv1.BuildConfigList{}
		listOpts := &client.ListOptions{Namespace: namespace}
		client.MatchingLabels{
			"scale.openshift.io/managed-by":    config.Name,
			"scale.openshift.io/resource-type": "buildconfig",
		}.ApplyToList(listOpts)
		if err := r.List(ctx, list, listOpts); err != nil {
			return 0, fmt.Errorf("failed to list buildconfigs: %w", err)
		}
		r.recordAPICall(config, 1)
		return int32(len(list.Items)), nil
	case "pods":
		list := &corev1.PodList{}
		listOpts := &client.ListOptions{Namespace: namespace}
		client.MatchingLabels{
			"scale.openshift.io/managed-by":    config.Name,
			"scale.openshift.io/resource-type": "pod",
		}.ApplyToList(listOpts)
		if err := r.List(ctx, list, listOpts); err != nil {
			return 0, fmt.Errorf("failed to list pods: %w", err)
		}
		r.recordAPICall(config, 1)
		return int32(len(list.Items)), nil
	default:
		return 0, fmt.Errorf("unsupported resource type: %s", resourceType)
	}
}

// getEffectiveAPIRate returns the effective API rate for the given configuration and node count
func (r *ScaleLoadConfigReconciler) getEffectiveAPIRate(config *scalev1.ScaleLoadConfig, nodeCount int) (int32, string) {
	var totalRate int32
	var rateType string

	if config.Spec.LoadProfile.APICallRateStatic != nil {
		totalRate = *config.Spec.LoadProfile.APICallRateStatic
		rateType = "static"
	} else if config.Spec.LoadProfile.APICallRatePerNode != nil {
		totalRate = *config.Spec.LoadProfile.APICallRatePerNode * int32(nodeCount)
		rateType = "per-node"
	} else {
		totalRate = 20 * int32(nodeCount)
		rateType = "default-per-node"
	}

	return totalRate, rateType
}

// managePods creates and manages Pod resources to simulate realistic workloads on KWOK nodes
func (r *ScaleLoadConfigReconciler) managePods(ctx context.Context,
	config *scalev1.ScaleLoadConfig, namespace string, targetCount int32) (int32, error) {

	log := r.Log.WithName("pod-manager").WithValues("namespace", namespace, "targetCount", targetCount)

	// Check if it's time to perform pod operations based on update frequency
	if !r.shouldPerformResourceOperation(namespace, "pods", config.Spec.ResourceChurn.Pods.UpdateFrequencyMin, config.Spec.ResourceChurn.Pods.UpdateFrequencyMax) {
		log.V(1).Info("Skipping pod operations - not within update frequency window")
		return r.getCurrentResourceCount(ctx, config, namespace, "pods")
	}

	log.V(1).Info("Performing pod operations within update frequency window")

	// Check maximum limit and adjust target count if needed
	effectiveTargetCount, err := r.checkMaximumLimit(ctx, config, "pods", targetCount, config.Spec.ResourceChurn.Pods.Maximum)
	if err != nil {
		return 0, fmt.Errorf("failed to check maximum limit for pods: %w", err)
	}

	if effectiveTargetCount != targetCount {
		log.Info("Pod creation limited by maximum",
			"requestedCount", targetCount,
			"effectiveCount", effectiveTargetCount,
			"maximum", config.Spec.ResourceChurn.Pods.Maximum)
		targetCount = effectiveTargetCount
	}

	// List existing Pods managed by this operator
	podList := &corev1.PodList{}
	listOpts := &client.ListOptions{
		Namespace: namespace,
	}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "pod",
	}.ApplyToList(listOpts)

	if err := r.List(ctx, podList, listOpts); err != nil {
		return 0, fmt.Errorf("failed to list Pods: %w", err)
	}
	r.recordAPICall(config, 1) // List operation

	currentCount := len(podList.Items)
	log.V(1).Info("Pod management starting",
		"current", currentCount,
		"target", targetCount,
		"targetCount", targetCount)

	var totalApiCalls, created, deleted int32

	// Scale up pods if needed
	if int32(currentCount) < targetCount {
		toCreate := targetCount - int32(currentCount)
		log.V(1).Info("Creating pods", "count", toCreate)

		for i := int32(0); i < toCreate; i++ {
			// Generate unique pod name to avoid conflicts
			uniqueName := r.generateUniquePodName(namespace, currentCount+int(i))
			pod := r.generatePod(config, namespace, uniqueName)
			if err := r.Create(ctx, pod); err != nil {
				log.Error(err, "Failed to create pod", "pod", pod.Name)
				continue
			}
			created++
			totalApiCalls++
			r.recordAPICall(config, 1)
		}
	}

	// Scale down pods if needed
	if int32(currentCount) > targetCount {
		toDelete := int32(currentCount) - targetCount
		log.V(1).Info("Deleting pods", "count", toDelete)

		for i := int32(len(podList.Items)) - 1; i >= int32(len(podList.Items))-toDelete && i >= 0; i-- {
			pod := &podList.Items[i]
			if err := r.Delete(ctx, pod); err != nil {
				log.Error(err, "Failed to delete pod", "pod", pod.Name)
				continue
			}
			deleted++
			totalApiCalls++
			r.recordAPICall(config, 1)
		}
	}

	// Randomly update some Pods to simulate churn
	objs := make([]client.Object, len(podList.Items))
	for i, item := range podList.Items {
		objs[i] = &item
	}
	updatedCount := r.performResourceChurn(ctx, config, objs, namespace, "pod")
	totalApiCalls += updatedCount

	log.V(1).Info("Pod management completed",
		"targetCount", targetCount,
		"final", targetCount,
		"created", created,
		"deleted", deleted,
		"updated", updatedCount,
		"totalApiCalls", totalApiCalls)

	return targetCount, nil
}

// generatePod creates a Pod with realistic configuration for KWOK nodes
func (r *ScaleLoadConfigReconciler) generatePod(config *scalev1.ScaleLoadConfig, namespace, name string) *corev1.Pod {
	// Get workload type (or use default if none specified)
	workloadType := r.selectPodWorkloadType(config)

	// Generate basic labels
	labels := map[string]string{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "pod",
		"scale.openshift.io/created-by":    "sim-operator",
		"scale.openshift.io/workload-type": workloadType.Name,
		"scale.openshift.io/creation-time": fmt.Sprintf("%d", time.Now().Unix()),
	}

	// Add workload type specific labels
	for k, v := range workloadType.Labels {
		labels[k] = v
	}

	// Generate basic annotations
	annotations := map[string]string{
		"scale.openshift.io/description": "Simulated workload pod for load testing",
	}

	// Add workload type specific annotations
	for k, v := range workloadType.Annotations {
		annotations[k] = v
	}

	// Convert resource strings to resource.Quantity
	resources := corev1.ResourceRequirements{
		Requests: make(corev1.ResourceList),
		Limits:   make(corev1.ResourceList),
	}

	if workloadType.Resources.CPURequest != "" {
		if quantity, err := parseResourceQuantity(workloadType.Resources.CPURequest); err == nil {
			resources.Requests[corev1.ResourceCPU] = quantity
		}
	}
	if workloadType.Resources.MemoryRequest != "" {
		if quantity, err := parseResourceQuantity(workloadType.Resources.MemoryRequest); err == nil {
			resources.Requests[corev1.ResourceMemory] = quantity
		}
	}
	if workloadType.Resources.CPULimit != "" {
		if quantity, err := parseResourceQuantity(workloadType.Resources.CPULimit); err == nil {
			resources.Limits[corev1.ResourceCPU] = quantity
		}
	}
	if workloadType.Resources.MemoryLimit != "" {
		if quantity, err := parseResourceQuantity(workloadType.Resources.MemoryLimit); err == nil {
			resources.Limits[corev1.ResourceMemory] = quantity
		}
	}

	// Determine restart policy
	restartPolicy := corev1.RestartPolicyAlways
	if workloadType.RestartPolicy != "" {
		switch workloadType.RestartPolicy {
		case "OnFailure":
			restartPolicy = corev1.RestartPolicyOnFailure
		case "Never":
			restartPolicy = corev1.RestartPolicyNever
		}
	}

	// Create the pod specification
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: restartPolicy,
			Containers: []corev1.Container{
				{
					Name:            "app",
					Image:           workloadType.Image,
					Resources:       resources,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{"sleep", "3600"}, // Simple long-running command
				},
			},
		},
	}

	// Add tolerations for KWOK nodes if enabled
	if config.Spec.ResourceChurn.Pods.TolerateKwokTaint {
		pod.Spec.Tolerations = []corev1.Toleration{
			{
				Key:      "kwok.x-k8s.io/node",
				Operator: corev1.TolerationOpEqual,
				Value:    "fake",
				Effect:   corev1.TaintEffectNoSchedule,
			},
		}
	}

	// Add node affinity to prefer KWOK nodes
	if config.Spec.ResourceChurn.Pods.NodeAffinityStrategy != "" {
		pod.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
					{
						Weight: 100,
						Preference: corev1.NodeSelectorTerm{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "type",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"kwok"},
								},
							},
						},
					},
				},
			},
		}
	}

	return pod
}

// selectPodWorkloadType selects a workload type based on weights, or returns a default
func (r *ScaleLoadConfigReconciler) selectPodWorkloadType(config *scalev1.ScaleLoadConfig) scalev1.PodWorkloadType {
	workloadTypes := config.Spec.ResourceChurn.Pods.WorkloadTypes

	// If no workload types defined, return default
	if len(workloadTypes) == 0 {
		return scalev1.PodWorkloadType{
			Name:   "default-app",
			Weight: 1,
			Resources: scalev1.PodResourceRequirements{
				CPURequest:    "100m",
				CPULimit:      "500m",
				MemoryRequest: "128Mi",
				MemoryLimit:   "256Mi",
			},
			Image:         "registry.redhat.io/ubi8/ubi-minimal:latest",
			RestartPolicy: "Always",
			Labels: map[string]string{
				"app": "simulated-workload",
			},
		}
	}

	// Calculate total weight
	totalWeight := int32(0)
	for _, wt := range workloadTypes {
		totalWeight += wt.Weight
	}

	if totalWeight == 0 {
		return workloadTypes[0] // Return first one if no weights
	}

	// Select based on weight
	randomValue := mathrand.Int31n(totalWeight)
	currentWeight := int32(0)

	for _, wt := range workloadTypes {
		currentWeight += wt.Weight
		if randomValue < currentWeight {
			return wt
		}
	}

	return workloadTypes[0] // Fallback
}

// parseResourceQuantity parses a resource string into a resource.Quantity
func parseResourceQuantity(s string) (resource.Quantity, error) {
	return resource.ParseQuantity(s)
}

// manageResourceTypesParallel processes all resource types concurrently within a namespace
func (r *ScaleLoadConfigReconciler) manageResourceTypesParallel(ctx context.Context, config *scalev1.ScaleLoadConfig, namespace corev1.Namespace) map[string]int {
	log := r.Log.WithName("resource-parallel").WithValues("namespace", namespace.Name)
	startTime := time.Now()

	// Result collection
	type resourceResult struct {
		resourceType string
		count        int32
		err          error
	}

	resultsChan := make(chan resourceResult, 10) // Buffer for all resource types
	var wg sync.WaitGroup

	// Track which resource types to process
	resourceTypes := []string{}

	log.V(2).Info("Starting parallel resource management",
		"configmapsEnabled", config.Spec.ResourceChurn.ConfigMaps.Enabled,
		"secretsEnabled", config.Spec.ResourceChurn.Secrets.Enabled,
		"routesEnabled", config.Spec.ResourceChurn.Routes.Enabled,
		"imagestreamsEnabled", config.Spec.ResourceChurn.ImageStreams.Enabled,
		"buildconfigsEnabled", config.Spec.ResourceChurn.BuildConfigs.Enabled,
		"eventsEnabled", config.Spec.ResourceChurn.Events.Enabled,
		"podsEnabled", config.Spec.ResourceChurn.Pods.Enabled)

	// ConfigMaps
	if config.Spec.ResourceChurn.ConfigMaps.Enabled {
		if r.shouldCreateResourceForNamespace(namespace, config.Spec.ResourceChurn.ConfigMaps.NamespaceInterval) {
			resourceTypes = append(resourceTypes, "configMaps")
			wg.Add(1)
			go func() {
				defer wg.Done()
				count, err := r.manageConfigMaps(ctx, config, namespace.Name, config.Spec.ResourceChurn.ConfigMaps.Count)
				resultsChan <- resourceResult{"configMaps", count, err}
			}()
		}
	}

	// Secrets
	if config.Spec.ResourceChurn.Secrets.Enabled {
		if r.shouldCreateResourceForNamespace(namespace, config.Spec.ResourceChurn.Secrets.NamespaceInterval) {
			resourceTypes = append(resourceTypes, "secrets")
			wg.Add(1)
			go func() {
				defer wg.Done()
				count, err := r.manageSecrets(ctx, config, namespace.Name, config.Spec.ResourceChurn.Secrets.Count)
				resultsChan <- resourceResult{"secrets", count, err}
			}()
		}
	}

	// Routes
	if config.Spec.ResourceChurn.Routes.Enabled {
		if r.shouldCreateResourceForNamespace(namespace, config.Spec.ResourceChurn.Routes.NamespaceInterval) {
			resourceTypes = append(resourceTypes, "routes")
			wg.Add(1)
			go func() {
				defer wg.Done()
				count, err := r.manageRoutes(ctx, config, namespace.Name, config.Spec.ResourceChurn.Routes.Count)
				resultsChan <- resourceResult{"routes", count, err}
			}()
		}
	}

	// ImageStreams
	if config.Spec.ResourceChurn.ImageStreams.Enabled {
		if r.shouldCreateResourceForNamespace(namespace, config.Spec.ResourceChurn.ImageStreams.NamespaceInterval) {
			resourceTypes = append(resourceTypes, "imageStreams")
			wg.Add(1)
			go func() {
				defer wg.Done()
				count, err := r.manageImageStreams(ctx, config, namespace.Name, config.Spec.ResourceChurn.ImageStreams.Count)
				resultsChan <- resourceResult{"imageStreams", count, err}
			}()
		}
	}

	// BuildConfigs
	if config.Spec.ResourceChurn.BuildConfigs.Enabled {
		if r.shouldCreateResourceForNamespace(namespace, config.Spec.ResourceChurn.BuildConfigs.NamespaceInterval) {
			resourceTypes = append(resourceTypes, "buildConfigs")
			wg.Add(1)
			go func() {
				defer wg.Done()
				count, err := r.manageBuildConfigs(ctx, config, namespace.Name, config.Spec.ResourceChurn.BuildConfigs.Count)
				resultsChan <- resourceResult{"buildConfigs", count, err}
			}()
		}
	}

	// Events (no namespace interval check)
	if config.Spec.ResourceChurn.Events.Enabled {
		resourceTypes = append(resourceTypes, "events")
		wg.Add(1)
		go func() {
			defer wg.Done()
			count, err := r.manageEvents(ctx, config, namespace.Name)
			resultsChan <- resourceResult{"events", count, err}
		}()
	}

	// Pods
	if config.Spec.ResourceChurn.Pods.Enabled {
		if r.shouldCreateResourceForNamespace(namespace, config.Spec.ResourceChurn.Pods.NamespaceInterval) {
			resourceTypes = append(resourceTypes, "pods")
			wg.Add(1)
			go func() {
				defer wg.Done()
				count, err := r.managePods(ctx, config, namespace.Name, config.Spec.ResourceChurn.Pods.Count)
				resultsChan <- resourceResult{"pods", count, err}
			}()
		}
	}

	// Wait for all resource types to complete
	wg.Wait()
	close(resultsChan)

	// Collect results
	resourceCounts := make(map[string]int)
	var errors []error
	successCount := 0

	for result := range resultsChan {
		if result.err != nil {
			log.Error(result.err, "Failed to manage resource type", "resourceType", result.resourceType)
			errors = append(errors, result.err)
		} else {
			resourceCounts[result.resourceType] = int(result.count)
			successCount++
		}
	}

	duration := time.Since(startTime)
	resourceTypesPerSecond := float64(len(resourceTypes)) / duration.Seconds()

	log.V(2).Info("Parallel resource management completed",
		"duration", duration,
		"resourceTypes", len(resourceTypes),
		"successful", successCount,
		"errors", len(errors),
		"resourceTypesPerSecond", fmt.Sprintf("%.1f", resourceTypesPerSecond),
		"finalCounts", resourceCounts)

	return resourceCounts
}

// Enhanced deletion helpers for complex OpenShift resources

// DeletionManager handles batched and tracked deletion operations
type DeletionManager struct {
	reconciler *ScaleLoadConfigReconciler
	mutex      sync.RWMutex
	inFlight   map[string]int32     // resourceType -> count currently being deleted
	lastBatch  map[string]time.Time // resourceType -> timestamp of last batch
	errors     map[string][]string  // resourceType -> recent errors
}

// NewDeletionManager creates a new deletion manager
func NewDeletionManager(reconciler *ScaleLoadConfigReconciler) *DeletionManager {
	return &DeletionManager{
		reconciler: reconciler,
		inFlight:   make(map[string]int32),
		lastBatch:  make(map[string]time.Time),
		errors:     make(map[string][]string),
	}
}

// CanPerformDeletion checks if deletion can proceed based on timing and safety constraints
func (dm *DeletionManager) CanPerformDeletion(resourceType string, config *scalev1.ScaleLoadConfig) bool {
	dm.mutex.RLock()
	defer dm.mutex.RUnlock()

	var resourceConfig scalev1.ResourceTypeConfig
	switch resourceType {
	case "routes":
		resourceConfig = config.Spec.ResourceChurn.Routes
	case "imageStreams":
		resourceConfig = config.Spec.ResourceChurn.ImageStreams
	case "buildConfigs":
		resourceConfig = config.Spec.ResourceChurn.BuildConfigs
	default:
		return true // Allow deletion for other resource types
	}

	// Check if safe deletion is enabled and enforce stricter controls
	if resourceConfig.SafeDeletionEnabled {
		// Don't allow deletion if there are already in-flight deletions
		if dm.inFlight[resourceType] > 0 {
			return false
		}

		// Enforce minimum delay between batches
		minDelay := time.Duration(resourceConfig.DeletionBatchDelay) * time.Second
		if minDelay == 0 {
			minDelay = 30 * time.Second // Conservative default for complex resources
		}

		lastBatch, exists := dm.lastBatch[resourceType]
		if exists && time.Since(lastBatch) < minDelay {
			return false
		}
	}

	return true
}

// DeleteResourcesBatched performs batched deletion with safety controls
func (dm *DeletionManager) DeleteResourcesBatched(ctx context.Context, config *scalev1.ScaleLoadConfig,
	resources []client.Object, resourceType string, targetDeleteCount int32) error {

	dm.mutex.Lock()
	defer dm.mutex.Unlock()

	var resourceConfig scalev1.ResourceTypeConfig
	switch resourceType {
	case "routes":
		resourceConfig = config.Spec.ResourceChurn.Routes
	case "imageStreams":
		resourceConfig = config.Spec.ResourceChurn.ImageStreams
	case "buildConfigs":
		resourceConfig = config.Spec.ResourceChurn.BuildConfigs
	default:
		return fmt.Errorf("unsupported resource type for batched deletion: %s", resourceType)
	}

	log := dm.reconciler.Log.WithName("deletion-manager").WithValues("resourceType", resourceType)

	batchSize := resourceConfig.DeletionBatchSize
	if batchSize <= 0 {
		batchSize = 3 // Conservative default
	}

	batchDelay := time.Duration(resourceConfig.DeletionBatchDelay) * time.Second
	if batchDelay <= 0 {
		batchDelay = 15 * time.Second // Conservative default
	}

	timeout := time.Duration(resourceConfig.DeletionTimeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second // 5 minute default
	}

	log.Info("Starting batched deletion",
		"totalResources", len(resources),
		"targetDeleteCount", targetDeleteCount,
		"batchSize", batchSize,
		"batchDelay", batchDelay.String(),
		"timeout", timeout.String())

	deleted := int32(0)
	resourcesLen := int32(len(resources))

	// Ensure we don't try to delete more than available
	if targetDeleteCount > resourcesLen {
		targetDeleteCount = resourcesLen
	}

	for i := resourcesLen - 1; i >= 0 && deleted < targetDeleteCount; {
		// Calculate batch end
		batchStart := i - batchSize + 1
		if batchStart < 0 {
			batchStart = 0
		}

		currentBatchSize := i - batchStart + 1
		if currentBatchSize > int32(targetDeleteCount-deleted) {
			currentBatchSize = targetDeleteCount - deleted
			batchStart = i - currentBatchSize + 1
		}

		log.V(1).Info("Processing deletion batch",
			"batchStart", batchStart,
			"batchEnd", i,
			"batchSize", currentBatchSize,
			"deleted", deleted,
			"remaining", targetDeleteCount-deleted)

		// Track in-flight deletions
		dm.inFlight[resourceType] += currentBatchSize

		// Delete current batch
		batchCtx, cancel := context.WithTimeout(ctx, timeout)
		batchDeleted := dm.deleteBatch(batchCtx, config, resources[batchStart:i+1], resourceType, currentBatchSize)
		cancel()

		// Update counters
		deleted += batchDeleted
		dm.inFlight[resourceType] -= currentBatchSize
		i = batchStart - 1

		// Update last batch timestamp
		dm.lastBatch[resourceType] = time.Now()

		log.V(1).Info("Batch deletion completed",
			"batchDeleted", batchDeleted,
			"totalDeleted", deleted,
			"remaining", targetDeleteCount-deleted)

		// Wait between batches (except for last batch)
		if deleted < targetDeleteCount && i >= 0 {
			log.V(1).Info("Waiting between deletion batches", "delay", batchDelay.String())
			time.Sleep(batchDelay)
		}
	}

	log.Info("Batched deletion completed",
		"totalDeleted", deleted,
		"targetDeleteCount", targetDeleteCount,
		"success", deleted >= targetDeleteCount)

	return nil
}

// deleteBatch deletes a single batch of resources
func (dm *DeletionManager) deleteBatch(ctx context.Context, config *scalev1.ScaleLoadConfig,
	batch []client.Object, resourceType string, targetCount int32) int32 {

	log := dm.reconciler.Log.WithName("batch-deleter").WithValues("resourceType", resourceType, "batchSize", len(batch))
	var deleted int32

	for i, resource := range batch {
		if deleted >= targetCount {
			break
		}

		select {
		case <-ctx.Done():
			log.Info("Batch deletion cancelled due to timeout", "deleted", deleted, "remaining", len(batch)-i)
			return deleted
		default:
		}

		if err := dm.deleteResourceSafely(ctx, config, resource, resourceType); err != nil {
			dm.recordDeletionError(resourceType, fmt.Sprintf("failed to delete %s: %v", resource.GetName(), err))
			log.Error(err, "Failed to delete resource", "name", resource.GetName())
			continue
		}

		deleted++
		dm.reconciler.recordAPICall(config, 1)
		log.V(2).Info("Resource deleted successfully", "name", resource.GetName())
	}

	return deleted
}

// deleteResourceSafely deletes a single resource with appropriate deletion policy
func (dm *DeletionManager) deleteResourceSafely(ctx context.Context, config *scalev1.ScaleLoadConfig,
	resource client.Object, resourceType string) error {

	// Use background deletion for complex resources to avoid blocking API server
	deletePolicy := metav1.DeletePropagationBackground

	var resourceConfig scalev1.ResourceTypeConfig
	switch resourceType {
	case "routes":
		resourceConfig = config.Spec.ResourceChurn.Routes
	case "imageStreams":
		resourceConfig = config.Spec.ResourceChurn.ImageStreams
	case "buildConfigs":
		resourceConfig = config.Spec.ResourceChurn.BuildConfigs
	}

	// Use async deletion if enabled
	if resourceConfig.AsyncDeletion {
		// Create a separate context to prevent timeout propagation
		deleteCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		deleteOpts := &client.DeleteOptions{
			PropagationPolicy: &deletePolicy,
		}

		return dm.reconciler.Delete(deleteCtx, resource, deleteOpts)
	}

	// Synchronous deletion (default)
	deleteOpts := &client.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}

	return dm.reconciler.Delete(ctx, resource, deleteOpts)
}

// recordDeletionError records a deletion error for tracking
func (dm *DeletionManager) recordDeletionError(resourceType, errorMsg string) {
	dm.mutex.Lock()
	defer dm.mutex.Unlock()

	if dm.errors[resourceType] == nil {
		dm.errors[resourceType] = make([]string, 0)
	}

	// Keep only last 5 errors per resource type
	if len(dm.errors[resourceType]) >= 5 {
		dm.errors[resourceType] = dm.errors[resourceType][1:]
	}

	dm.errors[resourceType] = append(dm.errors[resourceType], errorMsg)
}

// GetDeletionStatus returns current deletion status for status reporting
func (dm *DeletionManager) GetDeletionStatus() scalev1.ResourceDeletionStatus {
	dm.mutex.RLock()
	defer dm.mutex.RUnlock()

	status := scalev1.ResourceDeletionStatus{
		PendingDeletions:  make(map[string]int32), // not populated; kept for API compatibility
		LastDeletionBatch: make(map[string]*metav1.Time),
		DeletionErrors:    make(map[string][]string),
		InFlightDeletions: make(map[string]int32),
	}

	// Copy last batch timestamps
	for resourceType, timestamp := range dm.lastBatch {
		t := metav1.NewTime(timestamp)
		status.LastDeletionBatch[resourceType] = &t
	}

	// Copy errors
	for resourceType, errors := range dm.errors {
		if len(errors) > 0 {
			status.DeletionErrors[resourceType] = make([]string, len(errors))
			copy(status.DeletionErrors[resourceType], errors)
		}
	}

	// Copy in-flight counts
	for resourceType, count := range dm.inFlight {
		if count > 0 {
			status.InFlightDeletions[resourceType] = count
		}
	}

	return status
}

// isAPIServerTimeoutError checks if an error is due to API server being unable to handle requests
// This commonly happens with complex OpenShift resources like Routes, ImageStreams, and BuildConfigs
func isAPIServerTimeoutError(err error) bool {
	if err == nil {
		return false
	}

	errorMessage := err.Error()

	// Check for common API server timeout/overload patterns
	timeoutPatterns := []string{
		"the server is currently unable to handle the request",
		"context deadline exceeded",
		"timeout",
		"connection refused",
		"connection reset",
		"server closed the connection",
	}

	for _, pattern := range timeoutPatterns {
		if strings.Contains(strings.ToLower(errorMessage), pattern) {
			return true
		}
	}

	// Check for HTTP status code 503 (Service Unavailable) or 504 (Gateway Timeout)
	if errors.IsServiceUnavailable(err) || errors.IsTimeout(err) {
		return true
	}

	return false
}
