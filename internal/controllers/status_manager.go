package controllers

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	scalev1 "github.com/jtaleric/sim-operator/api/v1"
)

// updateStatus updates the ScaleLoadConfig status with current state
func (r *ScaleLoadConfigReconciler) updateStatus(ctx context.Context, config *scalev1.ScaleLoadConfig,
	kwokNodeCount int, namespaceCount int, resourceCounts map[string]int) (ctrl.Result, error) {

	log := r.Log.WithName("status-manager")

	// Get the latest version of the resource to avoid conflicts
	latestConfig := &scalev1.ScaleLoadConfig{}
	if err := r.Get(ctx, types.NamespacedName{Name: config.Name, Namespace: config.Namespace}, latestConfig); err != nil {
		log.Error(err, "Failed to fetch latest ScaleLoadConfig for status update")
		return ctrl.Result{}, err
	}

	// Calculate metrics
	metrics := r.calculateMetrics(latestConfig, kwokNodeCount, namespaceCount, resourceCounts)

	// Update status on the latest version
	latestConfig.Status.ObservedGeneration = latestConfig.Generation
	latestConfig.Status.KwokNodeCount = int32(kwokNodeCount)
	latestConfig.Status.GeneratedNamespaces = int32(namespaceCount)
	latestConfig.Status.LastReconcileTime = &metav1.Time{Time: time.Now()}
	latestConfig.Status.Metrics = metrics

	// Update resource counts
	latestConfig.Status.TotalResources = scalev1.ResourceCounts{
		ConfigMaps:   int32(resourceCounts["configMaps"]),
		Secrets:      int32(resourceCounts["secrets"]),
		Routes:       int32(resourceCounts["routes"]),
		ImageStreams: int32(resourceCounts["imageStreams"]),
		BuildConfigs: int32(resourceCounts["buildConfigs"]),
		Events:       int32(resourceCounts["events"]),
	}

	// Update conditions
	latestConfig.Status.Conditions = r.updateConditions(latestConfig, kwokNodeCount)

	// Update Prometheus metrics
	r.updatePrometheusMetrics(latestConfig)

	if err := r.Status().Update(ctx, latestConfig); err != nil {
		log.Error(err, "Failed to update ScaleLoadConfig status")
		return ctrl.Result{}, err
	}

	log.V(1).Info("Updated ScaleLoadConfig status",
		"kwokNodes", kwokNodeCount,
		"namespaces", namespaceCount,
		"totalResources", getTotalResourceCount(resourceCounts))

	return ctrl.Result{}, nil
}

// calculateMetrics computes current performance metrics
func (r *ScaleLoadConfigReconciler) calculateMetrics(config *scalev1.ScaleLoadConfig,
	kwokNodeCount int, namespaceCount int, resourceCounts map[string]int) scalev1.LoadGenerationMetrics {

	now := time.Now()
	timeSinceLastReconcile := now.Sub(r.lastReconcileTime)
	if r.lastReconcileTime.IsZero() {
		timeSinceLastReconcile = 1 * time.Minute
	}

	// Use default API call rate
	_ = float64(20.0) // Default API call rate for metrics

	// Estimate actual API calls based on resource operations
	totalResources := getTotalResourceCount(resourceCounts)
	estimatedAPICalls := float64(totalResources) * 0.1 // Estimate 10% of resources get API calls per minute

	// Calculate rates
	resourceCreationRate := float64(totalResources) / timeSinceLastReconcile.Minutes()
	resourceUpdateRate := resourceCreationRate * 0.3    // Estimate 30% get updated
	resourceDeletionRate := resourceCreationRate * 0.05 // Estimate 5% get deleted

	return scalev1.LoadGenerationMetrics{
		APICallsPerMinute:    strconv.FormatFloat(estimatedAPICalls, 'f', 2, 64),
		AverageReconcileTime: strconv.FormatInt(timeSinceLastReconcile.Milliseconds(), 10),
		ErrorRate:            strconv.FormatFloat(0.0, 'f', 2, 64), // TODO: Track actual errors
		ResourceCreationRate: strconv.FormatFloat(resourceCreationRate, 'f', 2, 64),
		ResourceUpdateRate:   strconv.FormatFloat(resourceUpdateRate, 'f', 2, 64),
		ResourceDeletionRate: strconv.FormatFloat(resourceDeletionRate, 'f', 2, 64),
	}
}

// updateConditions updates the status conditions based on current state
func (r *ScaleLoadConfigReconciler) updateConditions(config *scalev1.ScaleLoadConfig,
	kwokNodeCount int) []metav1.Condition {

	now := metav1.NewTime(time.Now())
	conditions := []metav1.Condition{}

	// Ready condition
	readyCondition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "LoadGenerationActive",
		Message:            fmt.Sprintf("Successfully generating load for %d KWOK nodes", kwokNodeCount),
	}

	if !config.Spec.Enabled {
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = "LoadGenerationDisabled"
		readyCondition.Message = "Load generation is disabled"
	} else if kwokNodeCount == 0 {
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = "NoKwokNodes"
		readyCondition.Message = "No KWOK nodes found matching selector"
	}

	conditions = append(conditions, readyCondition)

	// Scaling condition
	scalingCondition := metav1.Condition{
		Type:               "Scaling",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "ResourcesScaling",
		Message:            "Resources are scaling with KWOK node count",
	}

	if kwokNodeCount == 0 {
		scalingCondition.Status = metav1.ConditionFalse
		scalingCondition.Reason = "NoScaling"
		scalingCondition.Message = "No scaling activities due to zero KWOK nodes"
	}

	conditions = append(conditions, scalingCondition)

	// Degraded condition (check for issues)
	degradedCondition := metav1.Condition{
		Type:               "Degraded",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: now,
		Reason:             "OperatingNormally",
		Message:            "Load generation is operating normally",
	}

	// Check for potential issues
	if errorRate, err := strconv.ParseFloat(config.Status.Metrics.ErrorRate, 64); err == nil && errorRate > 10.0 {
		degradedCondition.Status = metav1.ConditionTrue
		degradedCondition.Reason = "HighErrorRate"
		degradedCondition.Message = fmt.Sprintf("High error rate: %.1f%%", errorRate)
	}

	conditions = append(conditions, degradedCondition)

	return conditions
}

// updatePrometheusMetrics updates the Prometheus metrics
func (r *ScaleLoadConfigReconciler) updatePrometheusMetrics(config *scalev1.ScaleLoadConfig) {
	r.KwokNodeCount.Set(float64(config.Status.KwokNodeCount))
	r.GeneratedNamespaces.Set(float64(config.Status.GeneratedNamespaces))
}

// handleDeletion cleans up resources when ScaleLoadConfig is deleted
func (r *ScaleLoadConfigReconciler) handleDeletion(ctx context.Context, namespacedName types.NamespacedName) (ctrl.Result, error) {
	log := r.Log.WithName("cleanup-manager").WithValues("config", namespacedName)

	// Clean up any managed namespaces
	if err := r.cleanupManagedNamespaces(ctx, namespacedName.Name); err != nil {
		log.Error(err, "Failed to cleanup managed namespaces")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Clean up resource managers
	for ns := range r.resourceManagers {
		delete(r.resourceManagers, ns)
	}

	log.Info("Cleaned up resources for deleted ScaleLoadConfig")
	return ctrl.Result{}, nil
}

// handleConfigDeletion handles deletion of ScaleLoadConfig with finalizers
func (r *ScaleLoadConfigReconciler) handleConfigDeletion(ctx context.Context,
	config *scalev1.ScaleLoadConfig) (ctrl.Result, error) {

	log := r.Log.WithName("config-deletion").WithValues("config", config.Name)

	// Perform cleanup
	if config.Spec.CleanupConfig.Enabled {
		if err := r.cleanupManagedNamespaces(ctx, config.Name); err != nil {
			log.Error(err, "Failed to cleanup managed namespaces during deletion")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}

		// Wait for cleanup delay if configured
		if config.Spec.CleanupConfig.CleanupDelaySeconds > 0 {
			delay := time.Duration(config.Spec.CleanupConfig.CleanupDelaySeconds) * time.Second
			if time.Since(config.DeletionTimestamp.Time) < delay {
				log.Info("Waiting for cleanup delay", "remaining", delay-time.Since(config.DeletionTimestamp.Time))
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
		}
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(config, "scale.openshift.io/cleanup")
	if err := r.Update(ctx, config); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("ScaleLoadConfig deletion completed")
	return ctrl.Result{}, nil
}

// cleanupManagedNamespaces removes all namespaces managed by a specific config
func (r *ScaleLoadConfigReconciler) cleanupManagedNamespaces(ctx context.Context, configName string) error {
	log := r.Log.WithName("namespace-cleanup")

	namespaceList := &corev1.NamespaceList{}
	labelSelector := client.MatchingLabels{
		"scale.openshift.io/managed-by": configName,
	}

	if err := r.List(ctx, namespaceList, labelSelector); err != nil {
		return fmt.Errorf("failed to list managed namespaces: %w", err)
	}

	for _, ns := range namespaceList.Items {
		if err := r.Delete(ctx, &ns); err != nil {
			log.Error(err, "Failed to delete managed namespace", "namespace", ns.Name)
			continue
		}
		log.V(1).Info("Deleted managed namespace", "namespace", ns.Name)

		// Clean up resource manager
		delete(r.resourceManagers, ns.Name)
	}

	return nil
}

// getTotalResourceCount calculates total resources across all types
func getTotalResourceCount(resourceCounts map[string]int) int {
	total := 0
	for _, count := range resourceCounts {
		total += count
	}
	return total
}
