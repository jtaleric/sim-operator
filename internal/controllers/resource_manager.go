package controllers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	mathrand "math/rand"
	"strconv"
	"time"

	buildv1 "github.com/openshift/api/build/v1"
	imagev1 "github.com/openshift/api/image/v1"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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

	log.Info("Starting resource management for namespace", 
		"phase", phase,
		"configmapsEnabled", config.Spec.ResourceChurn.ConfigMaps.Enabled,
		"secretsEnabled", config.Spec.ResourceChurn.Secrets.Enabled,
		"routesEnabled", config.Spec.ResourceChurn.Routes.Enabled,
		"imagestreamsEnabled", config.Spec.ResourceChurn.ImageStreams.Enabled,
		"buildconfigsEnabled", config.Spec.ResourceChurn.BuildConfigs.Enabled,
		"eventsEnabled", config.Spec.ResourceChurn.Events.Enabled)

	// ResourcesPerNamespace configuration available but not used in current implementation
	// resourcesPerNs := int32(5) // Default based on must-gather analysis
	// if config.Spec.LoadProfile.ResourcesPerNamespace != nil {
	//	resourcesPerNs = *config.Spec.LoadProfile.ResourcesPerNamespace
	// }

	// Manage each resource type
	if config.Spec.ResourceChurn.ConfigMaps.Enabled {
		if r.shouldCreateResourceForNamespace(namespace, config.Spec.ResourceChurn.ConfigMaps.NamespaceInterval) {
			count, err := r.manageConfigMaps(ctx, config, namespace.Name, config.Spec.ResourceChurn.ConfigMaps.Count)
			if err != nil {
				log.Error(err, "Failed to manage ConfigMaps")
			} else {
				resourceCounts["configMaps"] = int(count)
			}
		}
	}

	if config.Spec.ResourceChurn.Secrets.Enabled {
		if r.shouldCreateResourceForNamespace(namespace, config.Spec.ResourceChurn.Secrets.NamespaceInterval) {
			count, err := r.manageSecrets(ctx, config, namespace.Name, config.Spec.ResourceChurn.Secrets.Count)
			if err != nil {
				log.Error(err, "Failed to manage Secrets")
			} else {
				resourceCounts["secrets"] = int(count)
			}
		}
	}

	if config.Spec.ResourceChurn.Routes.Enabled {
		if r.shouldCreateResourceForNamespace(namespace, config.Spec.ResourceChurn.Routes.NamespaceInterval) {
			count, err := r.manageRoutes(ctx, config, namespace.Name, config.Spec.ResourceChurn.Routes.Count)
			if err != nil {
				log.Error(err, "Failed to manage Routes")
			} else {
				resourceCounts["routes"] = int(count)
			}
		} else {
			log.V(2).Info("Skipping Routes creation based on namespace interval", "interval", config.Spec.ResourceChurn.Routes.NamespaceInterval)
		}
	}

	if config.Spec.ResourceChurn.ImageStreams.Enabled {
		if r.shouldCreateResourceForNamespace(namespace, config.Spec.ResourceChurn.ImageStreams.NamespaceInterval) {
			count, err := r.manageImageStreams(ctx, config, namespace.Name, config.Spec.ResourceChurn.ImageStreams.Count)
			if err != nil {
				log.Error(err, "Failed to manage ImageStreams")
			} else {
				resourceCounts["imageStreams"] = int(count)
			}
		}
	}

	if config.Spec.ResourceChurn.BuildConfigs.Enabled {
		if r.shouldCreateResourceForNamespace(namespace, config.Spec.ResourceChurn.BuildConfigs.NamespaceInterval) {
			count, err := r.manageBuildConfigs(ctx, config, namespace.Name, config.Spec.ResourceChurn.BuildConfigs.Count)
			if err != nil {
				log.Error(err, "Failed to manage BuildConfigs")
			} else {
				resourceCounts["buildConfigs"] = int(count)
			}
		}
	}

	if config.Spec.ResourceChurn.Events.Enabled {
		// Events don't have namespace interval as they're not created per namespace in the same way
		count, err := r.manageEvents(ctx, config, namespace.Name)
		if err != nil {
			log.Error(err, "Failed to manage Events")
		} else {
			resourceCounts["events"] = int(count)
		}
	}

	duration := time.Since(startTime)
	
	// Calculate totals from individual resource managers
	for resourceType, count := range resourceCounts {
		if resourceType == "events" {
			totalCreated += int32(count) // Events are only created, not updated
		}
	}

	log.Info("Namespace resource management completed", 
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

	currentCount := len(configMapList.Items)
	var created, deleted, apiCalls int32
	apiCalls++ // List operation

	log.Info("ConfigMap management starting", "current", currentCount, "target", targetCount)

	// Scale up if needed
	if int32(currentCount) < targetCount {
		toCreate := targetCount - int32(currentCount)
		for i := int32(currentCount); i < targetCount; i++ {
			configMap := r.generateConfigMap(config, namespace, i)
			if err := r.Create(ctx, configMap); err != nil {
				log.Error(err, "Failed to create ConfigMap", "name", configMap.Name, "created", created)
				return int32(currentCount) + created, fmt.Errorf("failed to create ConfigMap: %w", err)
			}
			created++
			apiCalls++
		}
		log.Info("ConfigMaps created", "count", toCreate, "apiCalls", created)
	}

	// Scale down if needed
	if int32(currentCount) > targetCount {
		toDelete := int32(currentCount) - targetCount
		for i := int32(len(configMapList.Items)) - 1; i >= targetCount; i-- {
			if err := r.Delete(ctx, &configMapList.Items[i]); err != nil {
				log.Error(err, "Failed to delete ConfigMap", "name", configMapList.Items[i].Name, "deleted", deleted)
				return int32(currentCount) - deleted, fmt.Errorf("failed to delete ConfigMap: %w", err)
			}
			deleted++
			apiCalls++
		}
		log.Info("ConfigMaps deleted", "count", toDelete, "apiCalls", deleted)
	}

	// Randomly update some ConfigMaps to simulate churn
	objs := make([]client.Object, len(configMapList.Items))
	for i, item := range configMapList.Items {
		objs[i] = &item
	}
	updatedCount := r.performResourceChurn(ctx, config, objs, namespace, "configmap")
	apiCalls += updatedCount

	log.Info("ConfigMap management completed", 
		"final", targetCount, 
		"created", created, 
		"deleted", deleted, 
		"updated", updatedCount,
		"totalApiCalls", apiCalls)

	return targetCount, nil
}

// generateConfigMap creates a realistic ConfigMap resource
func (r *ScaleLoadConfigReconciler) generateConfigMap(config *scalev1.ScaleLoadConfig, namespace string, index int32) *corev1.ConfigMap {
	name := fmt.Sprintf("load-config-%d", index)

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
				"app.kubernetes.io/name":           fmt.Sprintf("load-app-%d", index),
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

	currentCount := len(secretList.Items)
	var created, deleted, apiCalls int32
	apiCalls++ // List operation

	log.Info("Secret management starting", "current", currentCount, "target", targetCount)

	// Scale up if needed
	if int32(currentCount) < targetCount {
		toCreate := targetCount - int32(currentCount)
		for i := int32(currentCount); i < targetCount; i++ {
			secret := r.generateSecret(config, namespace, i)
			if err := r.Create(ctx, secret); err != nil {
				log.Error(err, "Failed to create Secret", "name", secret.Name, "created", created)
				return int32(currentCount) + created, fmt.Errorf("failed to create Secret: %w", err)
			}
			created++
			apiCalls++
		}
		log.Info("Secrets created", "count", toCreate, "apiCalls", created)
	}

	// Scale down if needed
	if int32(currentCount) > targetCount {
		toDelete := int32(currentCount) - targetCount
		for i := int32(len(secretList.Items)) - 1; i >= targetCount; i-- {
			if err := r.Delete(ctx, &secretList.Items[i]); err != nil {
				log.Error(err, "Failed to delete Secret", "name", secretList.Items[i].Name, "deleted", deleted)
				return int32(currentCount) - deleted, fmt.Errorf("failed to delete Secret: %w", err)
			}
			deleted++
			apiCalls++
		}
		log.Info("Secrets deleted", "count", toDelete, "apiCalls", deleted)
	}

	// Simulate secret rotation
	objs := make([]client.Object, len(secretList.Items))
	for i, item := range secretList.Items {
		objs[i] = &item
	}
	updatedCount := r.performResourceChurn(ctx, config, objs, namespace, "secret")
	apiCalls += updatedCount

	log.Info("Secret management completed", 
		"final", targetCount, 
		"created", created, 
		"deleted", deleted, 
		"updated", updatedCount,
		"totalApiCalls", apiCalls)

	return targetCount, nil
}

// generateSecret creates a realistic Secret resource
func (r *ScaleLoadConfigReconciler) generateSecret(config *scalev1.ScaleLoadConfig, namespace string, index int32) *corev1.Secret {
	name := fmt.Sprintf("load-secret-%d", index)

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
				"app.kubernetes.io/name":           fmt.Sprintf("load-app-%d", index),
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

	routeList := &routev1.RouteList{}
	listOpts := &client.ListOptions{
		Namespace: namespace,
	}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "route",
	}.ApplyToList(listOpts)

	if err := r.List(ctx, routeList, listOpts); err != nil {
		return 0, fmt.Errorf("failed to list Routes: %w", err)
	}

	currentCount := len(routeList.Items)

	// Scale up if needed
	if int32(currentCount) < targetCount {
		for i := int32(currentCount); i < targetCount; i++ {
			// First create the Service that the Route will reference
			service := r.generateService(config, namespace, i)
			if err := r.Create(ctx, service); err != nil {
				// Check if the service already exists
				if !errors.IsAlreadyExists(err) {
					return int32(currentCount), fmt.Errorf("failed to create Service: %w", err)
				}
			}

			// Then create the Route
			route := r.generateRoute(config, namespace, i)
			if err := r.Create(ctx, route); err != nil {
				return int32(currentCount), fmt.Errorf("failed to create Route: %w", err)
			}
		}
	}

	// Scale down if needed
	if int32(currentCount) > targetCount {
		for i := int32(len(routeList.Items)) - 1; i >= targetCount; i-- {
			// Delete the Route first
			if err := r.Delete(ctx, &routeList.Items[i]); err != nil {
				return int32(currentCount), fmt.Errorf("failed to delete Route: %w", err)
			}

			// Then delete the corresponding Service
			serviceName := fmt.Sprintf("load-service-%d", i)
			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: namespace,
				},
			}
			if err := r.Delete(ctx, service); err != nil {
				// Log the error but don't fail the operation if the service doesn't exist
				if !errors.IsNotFound(err) {
					return int32(currentCount), fmt.Errorf("failed to delete Service: %w", err)
				}
			}
		}
	}

	return targetCount, nil
}

// generateRoute creates a realistic Route resource
func (r *ScaleLoadConfigReconciler) generateRoute(config *scalev1.ScaleLoadConfig, namespace string, index int32) *routev1.Route {
	name := fmt.Sprintf("load-route-%d", index)
	serviceName := fmt.Sprintf("load-service-%d", index)

	return &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"scale.openshift.io/managed-by":    config.Name,
				"scale.openshift.io/resource-type": "route",
				"scale.openshift.io/created-by":    "sim-operator",
				"app.kubernetes.io/name":           fmt.Sprintf("load-app-%d", index),
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
	name := fmt.Sprintf("load-service-%d", index)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"scale.openshift.io/managed-by":    config.Name,
				"scale.openshift.io/resource-type": "service",
				"scale.openshift.io/created-by":    "sim-operator",
				"app.kubernetes.io/name":           fmt.Sprintf("load-app-%d", index),
				"app.kubernetes.io/component":      "backend",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app.kubernetes.io/name": fmt.Sprintf("load-app-%d", index),
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

	imageStreamList := &imagev1.ImageStreamList{}
	listOpts := &client.ListOptions{
		Namespace: namespace,
	}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "imagestream",
	}.ApplyToList(listOpts)

	if err := r.List(ctx, imageStreamList, listOpts); err != nil {
		return 0, fmt.Errorf("failed to list ImageStreams: %w", err)
	}

	currentCount := len(imageStreamList.Items)

	// Scale up if needed
	if int32(currentCount) < targetCount {
		for i := int32(currentCount); i < targetCount; i++ {
			imageStream := r.generateImageStream(config, namespace, i)
			if err := r.Create(ctx, imageStream); err != nil {
				return int32(currentCount), fmt.Errorf("failed to create ImageStream: %w", err)
			}
		}
	}

	// Scale down if needed
	if int32(currentCount) > targetCount {
		for i := int32(len(imageStreamList.Items)) - 1; i >= targetCount; i-- {
			if err := r.Delete(ctx, &imageStreamList.Items[i]); err != nil {
				return int32(currentCount), fmt.Errorf("failed to delete ImageStream: %w", err)
			}
		}
	}

	return targetCount, nil
}

// generateImageStream creates a realistic ImageStream resource
func (r *ScaleLoadConfigReconciler) generateImageStream(config *scalev1.ScaleLoadConfig, namespace string, index int32) *imagev1.ImageStream {
	name := fmt.Sprintf("load-image-%d", index)

	return &imagev1.ImageStream{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"scale.openshift.io/managed-by":    config.Name,
				"scale.openshift.io/resource-type": "imagestream",
				"scale.openshift.io/created-by":    "sim-operator",
				"app.kubernetes.io/name":           fmt.Sprintf("load-app-%d", index),
				"app.kubernetes.io/component":      "image",
			},
		},
		Spec: imagev1.ImageStreamSpec{
			Tags: []imagev1.TagReference{
				{
					Name: "latest",
					From: &corev1.ObjectReference{
						Kind: "DockerImage",
						Name: "quay.io/cloud-bulldozer/sampleapp:latest",
					},
					ImportPolicy: imagev1.TagImportPolicy{
						Scheduled: true,
					},
				},
			},
		},
	}
}

// manageBuildConfigs creates and manages BuildConfig resources
func (r *ScaleLoadConfigReconciler) manageBuildConfigs(ctx context.Context,
	config *scalev1.ScaleLoadConfig, namespace string, targetCount int32) (int32, error) {

	buildConfigList := &buildv1.BuildConfigList{}
	listOpts := &client.ListOptions{
		Namespace: namespace,
	}
	client.MatchingLabels{
		"scale.openshift.io/managed-by":    config.Name,
		"scale.openshift.io/resource-type": "buildconfig",
	}.ApplyToList(listOpts)

	if err := r.List(ctx, buildConfigList, listOpts); err != nil {
		return 0, fmt.Errorf("failed to list BuildConfigs: %w", err)
	}

	currentCount := len(buildConfigList.Items)

	// Scale up if needed
	if int32(currentCount) < targetCount {
		for i := int32(currentCount); i < targetCount; i++ {
			buildConfig := r.generateBuildConfig(config, namespace, i)
			if err := r.Create(ctx, buildConfig); err != nil {
				return int32(currentCount), fmt.Errorf("failed to create BuildConfig: %w", err)
			}
		}
	}

	// Scale down if needed
	if int32(currentCount) > targetCount {
		for i := int32(len(buildConfigList.Items)) - 1; i >= targetCount; i-- {
			if err := r.Delete(ctx, &buildConfigList.Items[i]); err != nil {
				return int32(currentCount), fmt.Errorf("failed to delete BuildConfig: %w", err)
			}
		}
	}

	return targetCount, nil
}

// generateBuildConfig creates a realistic BuildConfig resource
func (r *ScaleLoadConfigReconciler) generateBuildConfig(config *scalev1.ScaleLoadConfig, namespace string, index int32) *buildv1.BuildConfig {
	name := fmt.Sprintf("load-build-%d", index)
	imageStreamName := fmt.Sprintf("load-image-%d", index)

	return &buildv1.BuildConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"scale.openshift.io/managed-by":    config.Name,
				"scale.openshift.io/resource-type": "buildconfig",
				"scale.openshift.io/created-by":    "sim-operator",
				"app.kubernetes.io/name":           fmt.Sprintf("load-app-%d", index),
				"app.kubernetes.io/component":      "build",
			},
		},
		Spec: buildv1.BuildConfigSpec{
			CommonSpec: buildv1.CommonSpec{
				Source: buildv1.BuildSource{
					Git: &buildv1.GitBuildSource{
						URI: "https://github.com/cloud-bulldozer/sampleapp.git",
					},
				},
				Strategy: buildv1.BuildStrategy{
					DockerStrategy: &buildv1.DockerBuildStrategy{},
				},
				Output: buildv1.BuildOutput{
					To: &corev1.ObjectReference{
						Kind: "ImageStreamTag",
						Name: fmt.Sprintf("%s:latest", imageStreamName),
					},
				},
			},
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

	log.Info("Event generation starting", 
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

	log.Info("Event generation completed", 
		"attempted", eventsToCreate,
		"created", createdCount,
		"failed", failedCount,
		"successRate", fmt.Sprintf("%.1f%%", float64(createdCount)/float64(eventsToCreate)*100),
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
		Name:       fmt.Sprintf("load-pod-%d", index),
		APIVersion: "v1",
	}

	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("load-event-%d-%d", index, time.Now().Unix()),
			Namespace: namespace,
			Labels: map[string]string{
				"scale.openshift.io/managed-by": config.Name,
				"scale.openshift.io/created-by": "sim-operator",
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
app.name=load-generator-app
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
    "name": "load-generator",
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

func generateRandomPassword(length int) string {
	bytes := make([]byte, length)
	rand.Read(bytes)
	return base64.URLEncoding.EncodeToString(bytes)[:length]
}

func generateRandomAPIKey() string {
	bytes := make([]byte, 32)
	rand.Read(bytes)
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

	// Randomly select resources to update based on configuration
	updateChance := 0.1 // 10% chance per reconcile cycle
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
			}
		}
	}

	if updatedCount > 0 {
		r.Log.Info("Resource churn completed", 
			"type", resourceType, 
			"namespace", namespace,
			"totalResources", len(resources),
			"updated", updatedCount,
			"updateRate", fmt.Sprintf("%.1f%%", float64(updatedCount)/float64(len(resources))*100))
	}

	return updatedCount
}
