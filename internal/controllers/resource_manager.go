package controllers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	mathrand "math/rand"
	"time"

	buildv1 "github.com/openshift/api/build/v1"
	imagev1 "github.com/openshift/api/image/v1"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	scalev1 "github.com/jtaleric/sim-operator/api/v1"
)

// manageNamespaceResources creates and manages resources within a namespace
func (r *ScaleLoadConfigReconciler) manageNamespaceResources(ctx context.Context,
	config *scalev1.ScaleLoadConfig, namespace string) (map[string]int, error) {

	log := r.Log.WithName("resource-manager").WithValues("namespace", namespace)
	resourceCounts := make(map[string]int)

	// ResourcesPerNamespace configuration available but not used in current implementation
	// resourcesPerNs := int32(5) // Default based on must-gather analysis
	// if config.Spec.LoadProfile.ResourcesPerNamespace != nil {
	//	resourcesPerNs = *config.Spec.LoadProfile.ResourcesPerNamespace
	// }

	// Manage each resource type
	if config.Spec.ResourceChurn.ConfigMaps.Enabled {
		count, err := r.manageConfigMaps(ctx, config, namespace, config.Spec.ResourceChurn.ConfigMaps.Count)
		if err != nil {
			log.Error(err, "Failed to manage ConfigMaps")
		} else {
			resourceCounts["configMaps"] = int(count)
		}
	}

	if config.Spec.ResourceChurn.Secrets.Enabled {
		count, err := r.manageSecrets(ctx, config, namespace, config.Spec.ResourceChurn.Secrets.Count)
		if err != nil {
			log.Error(err, "Failed to manage Secrets")
		} else {
			resourceCounts["secrets"] = int(count)
		}
	}

	if config.Spec.ResourceChurn.Routes.Enabled {
		count, err := r.manageRoutes(ctx, config, namespace, config.Spec.ResourceChurn.Routes.Count)
		if err != nil {
			log.Error(err, "Failed to manage Routes")
		} else {
			resourceCounts["routes"] = int(count)
		}
	}

	if config.Spec.ResourceChurn.ImageStreams.Enabled {
		count, err := r.manageImageStreams(ctx, config, namespace, config.Spec.ResourceChurn.ImageStreams.Count)
		if err != nil {
			log.Error(err, "Failed to manage ImageStreams")
		} else {
			resourceCounts["imageStreams"] = int(count)
		}
	}

	if config.Spec.ResourceChurn.BuildConfigs.Enabled {
		count, err := r.manageBuildConfigs(ctx, config, namespace, config.Spec.ResourceChurn.BuildConfigs.Count)
		if err != nil {
			log.Error(err, "Failed to manage BuildConfigs")
		} else {
			resourceCounts["buildConfigs"] = int(count)
		}
	}

	if config.Spec.ResourceChurn.Events.Enabled {
		count, err := r.manageEvents(ctx, config, namespace)
		if err != nil {
			log.Error(err, "Failed to manage Events")
		} else {
			resourceCounts["events"] = int(count)
		}
	}

	return resourceCounts, nil
}

// manageConfigMaps creates and manages ConfigMap resources
func (r *ScaleLoadConfigReconciler) manageConfigMaps(ctx context.Context,
	config *scalev1.ScaleLoadConfig, namespace string, targetCount int32) (int32, error) {

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

	// Scale up if needed
	if int32(currentCount) < targetCount {
		for i := int32(currentCount); i < targetCount; i++ {
			configMap := r.generateConfigMap(config, namespace, i)
			if err := r.Create(ctx, configMap); err != nil {
				return int32(currentCount), fmt.Errorf("failed to create ConfigMap: %w", err)
			}
		}
	}

	// Scale down if needed
	if int32(currentCount) > targetCount {
		for i := int32(len(configMapList.Items)) - 1; i >= targetCount; i-- {
			if err := r.Delete(ctx, &configMapList.Items[i]); err != nil {
				return int32(currentCount), fmt.Errorf("failed to delete ConfigMap: %w", err)
			}
		}
	}

	// Randomly update some ConfigMaps to simulate churn
	objs := make([]client.Object, len(configMapList.Items))
	for i, item := range configMapList.Items {
		objs[i] = &item
	}
	r.performResourceChurn(ctx, config, objs, namespace, "configmap")

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

	// Scale up if needed
	if int32(currentCount) < targetCount {
		for i := int32(currentCount); i < targetCount; i++ {
			secret := r.generateSecret(config, namespace, i)
			if err := r.Create(ctx, secret); err != nil {
				return int32(currentCount), fmt.Errorf("failed to create Secret: %w", err)
			}
		}
	}

	// Scale down if needed
	if int32(currentCount) > targetCount {
		for i := int32(len(secretList.Items)) - 1; i >= targetCount; i-- {
			if err := r.Delete(ctx, &secretList.Items[i]); err != nil {
				return int32(currentCount), fmt.Errorf("failed to delete Secret: %w", err)
			}
		}
	}

	// Simulate secret rotation
	objs := make([]client.Object, len(secretList.Items))
	for i, item := range secretList.Items {
		objs[i] = &item
	}
	r.performResourceChurn(ctx, config, objs, namespace, "secret")

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
			route := r.generateRoute(config, namespace, i)
			if err := r.Create(ctx, route); err != nil {
				return int32(currentCount), fmt.Errorf("failed to create Route: %w", err)
			}
		}
	}

	// Scale down if needed
	if int32(currentCount) > targetCount {
		for i := int32(len(routeList.Items)) - 1; i >= targetCount; i-- {
			if err := r.Delete(ctx, &routeList.Items[i]); err != nil {
				return int32(currentCount), fmt.Errorf("failed to delete Route: %w", err)
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

	var createdCount int32
	for i := int32(0); i < eventsToCreate; i++ {
		event := r.generateEvent(config, namespace, i)
		if err := r.Create(ctx, event); err != nil {
			// Events often conflict on creation, which is normal
			continue
		}
		createdCount++
	}

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
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
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

// performResourceChurn simulates realistic resource update patterns
func (r *ScaleLoadConfigReconciler) performResourceChurn(ctx context.Context,
	config *scalev1.ScaleLoadConfig, resources []client.Object, namespace, resourceType string) {

	if len(resources) == 0 {
		return
	}

	// Randomly select resources to update based on configuration
	updateChance := 0.1 // 10% chance per reconcile cycle

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
			}
		}
	}
}
