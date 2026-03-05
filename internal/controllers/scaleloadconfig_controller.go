package controllers

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	scalev1 "github.com/jtaleric/sim-operator/api/v1"
)

// ScaleLoadConfigReconciler reconciles a ScaleLoadConfig object
type ScaleLoadConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger

	// Metrics for observability
	KwokNodeCount       prometheus.Gauge
	GeneratedNamespaces prometheus.Gauge
	APICallRate         prometheus.Histogram
	ReconcileTime       prometheus.Histogram
	ErrorCount          prometheus.Counter

	// Internal state for load generation
	lastReconcileTime time.Time
	resourceManagers  map[string]*ResourceManager

	// Simplified API rate control
	targetAPICallsPerMinute int32
	apiCallsThisMinute      int32
	lastRateReset           time.Time

	// Cumulative API call tracking for metrics
	totalAPICallsMade int64

	// Logging optimization
	reconcileCounter int
	statusLogCounter int // used to log status every N reconciles

	// Cached managed namespaces for current reconcile (avoids repeated getManagedNamespaces in checkMaximumLimit)
	currentManagedNamespaces []corev1.Namespace

	// Enhanced deletion manager for complex resources
	deletionManager *DeletionManager
}

// ResourceManager handles lifecycle of resources for a specific namespace
type ResourceManager struct {
	namespace        string
	associatedNode   string
	lastUpdate       time.Time
	resourceCounters map[string]int
	updateTimers     map[string]time.Time
}

//+kubebuilder:rbac:groups=scale.openshift.io,resources=scaleloadconfigs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=scale.openshift.io,resources=scaleloadconfigs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=scale.openshift.io,resources=scaleloadconfigs/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=image.openshift.io,resources=imagestreams,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=build.openshift.io,resources=buildconfigs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements the main reconciliation loop
func (r *ScaleLoadConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("scaleloadconfig", req.NamespacedName)
	startTime := time.Now()

	// Add timeout to prevent infinite reconcile loops
	timeout := 5 * time.Minute
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	defer func() {
		duration := time.Since(startTime)
		r.ReconcileTime.Observe(duration.Seconds())
		log.V(1).Info("Reconcile completed", "duration", duration.String())
	}()

	// Fetch the ScaleLoadConfig instance
	config := &scalev1.ScaleLoadConfig{}
	if err := r.Get(ctx, req.NamespacedName, config); err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("ScaleLoadConfig deleted, cleaning up resources")
			return r.handleDeletion(ctx, req.NamespacedName)
		}
		r.ErrorCount.Inc()
		log.Error(err, "Unable to fetch ScaleLoadConfig")
		return ctrl.Result{}, err
	}

	// Initialize resource managers if needed
	if r.resourceManagers == nil {
		r.resourceManagers = make(map[string]*ResourceManager)
	}

	// Add finalizer for cleanup
	if !controllerutil.ContainsFinalizer(config, "scale.openshift.io/cleanup") {
		controllerutil.AddFinalizer(config, "scale.openshift.io/cleanup")
		if err := r.Update(ctx, config); err != nil {
			r.ErrorCount.Inc()
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Handle deletion
	if !config.DeletionTimestamp.IsZero() {
		return r.handleConfigDeletion(ctx, config)
	}

	// Skip reconciliation if disabled
	if !config.Spec.Enabled {
		log.Info("Scale load generation is disabled")
		return r.updateStatus(ctx, config, 0, 0, make(map[string]int))
	}

	// Get KWOK nodes
	kwokNodes, err := r.getKwokNodes(ctx, config.Spec.KwokNodeSelector)
	if err != nil {
		r.ErrorCount.Inc()
		log.Error(err, "Failed to get KWOK nodes")
		return ctrl.Result{}, err
	}
	r.recordAPICall(config, 1) // List nodes operation

	log.V(1).Info("Found KWOK nodes", "count", len(kwokNodes))
	r.KwokNodeCount.Set(float64(len(kwokNodes)))

	// Early status update with current node count to prevent stale status
	if err := r.updateNodeCountStatus(ctx, config, len(kwokNodes)); err != nil {
		log.Error(err, "Failed to update node count status early, continuing")
	}

	// Calculate target namespace count based on load profile
	targetNamespaces := r.calculateTargetNamespaces(config, len(kwokNodes))

	// Log effective API rate
	effectiveRate, rateType := r.getEffectiveAPIRate(config, len(kwokNodes))
	log.Info("API rate configuration",
		"effectiveRate", effectiveRate,
		"rateType", rateType,
		"nodeCount", len(kwokNodes),
		"ratePerSecond", fmt.Sprintf("%.1f", float64(effectiveRate)/60.0))

	log.V(1).Info("Target namespace calculation", "kwokNodes", len(kwokNodes), "targetNamespaces", targetNamespaces)

	// Note: Removed restrictive pre-flight API rate limiting to allow actual configured rates
	// Resource managers will handle rate limiting individually with more accurate tracking

	// Manage namespaces and resources with timeout protection
	log.V(1).Info("Starting load resource management",
		"targetNamespaces", targetNamespaces,
		"kwokNodes", len(kwokNodes))

	// Check if we're approaching timeout
	if time.Since(startTime) > 4*time.Minute {
		log.Info("Approaching reconcile timeout, skipping resource management this cycle")
		// Return early with updated node count status
		return r.calculateNextReconcileResult(config, len(kwokNodes))
	}

	// Check if we should throttle operations to avoid exceeding target API rate
	if r.shouldThrottleOperations(config, len(kwokNodes)) {
		log.Info("Throttling this reconcile cycle to control API rate")
		// Return early with current status to avoid excessive API calls
		_, err := r.updateStatus(ctx, config, len(kwokNodes), 0, make(map[string]int))
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.calculateReconcileInterval(config, len(kwokNodes))}, nil
	}

	namespaceCount, resourceCounts, err := r.manageLoadResources(ctx, config, kwokNodes, targetNamespaces)
	if err != nil {
		r.ErrorCount.Inc()
		log.Error(err, "Failed to manage load resources")
		return ctrl.Result{}, err
	}

	// Update last reconcile time for rate limiting
	r.lastReconcileTime = startTime

	// Calculate total API operations for this reconcile
	totalResources := 0
	for _, count := range resourceCounts {
		totalResources += count
	}

	// Periodic summary logging (every 6 reconciles = ~1 minute)
	r.reconcileCounter++
	if r.reconcileCounter%6 == 0 {
		log.Info("Load generation summary",
			"namespaces", namespaceCount,
			"totalResources", totalResources,
			"reconcileDuration", time.Since(startTime).String(),
			"effectiveAPIRate", fmt.Sprintf("%.0f calls/min", float64(effectiveRate)))
	}

	// Update node annotations for networking churn
	if config.Spec.AnnotationChurn.Enabled {
		if err := r.updateNodeAnnotations(ctx, config, kwokNodes); err != nil {
			log.Error(err, "Failed to update node annotations, continuing")
		}
	}

	// Update status
	_, err = r.updateStatus(ctx, config, len(kwokNodes), namespaceCount, resourceCounts)
	if err != nil {
		r.ErrorCount.Inc()
		return ctrl.Result{}, err
	}

	// Perform orphan cleanup if enabled
	if config.Spec.CleanupConfig.OrphanCleanup {
		if err := r.performOrphanCleanup(ctx, config); err != nil {
			log.Error(err, "Failed to perform orphan cleanup, continuing")
		}
	}

	// Perform namespace churn if enabled
	if config.Spec.ResourceChurn.Namespaces.Enabled {
		if err := r.performNamespaceChurn(ctx, config); err != nil {
			log.Error(err, "Failed to perform namespace churn")
		}
	}

	// Perform additional API calls to meet target rate if needed
	r.ensureAPICallRate(ctx, config, len(kwokNodes))

	// Calculate next reconcile interval based on API rate capacity
	nextReconcile := r.calculateReconcileInterval(config, len(kwokNodes))
	log.V(1).Info("Next reconcile scheduled", "interval", nextReconcile)

	return ctrl.Result{RequeueAfter: nextReconcile}, nil
}

// getKwokNodes retrieves nodes matching the KWOK selector with pagination support
func (r *ScaleLoadConfigReconciler) getKwokNodes(ctx context.Context, selector map[string]string) ([]corev1.Node, error) {
	log := r.Log.WithName("node-lister")

	if len(selector) == 0 {
		selector = map[string]string{"type": "kwok"}
	}

	labelSelector := labels.SelectorFromSet(selector)
	var allNodes []corev1.Node

	// Use pagination to handle large node lists
	pageSize := int64(500) // Process in chunks of 500 nodes
	continueToken := ""

	for {
		nodeList := &corev1.NodeList{}
		listOpts := &client.ListOptions{
			LabelSelector: labelSelector,
			Raw: &metav1.ListOptions{
				TimeoutSeconds: int64ptr(120), // Increased to 2 minutes for large clusters
				Limit:          pageSize,
				Continue:       continueToken,
			},
		}

		log.V(2).Info("Listing KWOK nodes with pagination",
			"selector", selector,
			"pageSize", pageSize,
			"continue", continueToken != "",
			"currentTotal", len(allNodes))

		startTime := time.Now()
		err := r.List(ctx, nodeList, listOpts)
		duration := time.Since(startTime)

		if err != nil {
			log.Error(err, "Failed to list KWOK nodes",
				"selector", selector,
				"duration", duration,
				"page", continueToken != "")
			return nil, fmt.Errorf("failed to list KWOK nodes: %w", err)
		}

		// Add nodes from this page
		allNodes = append(allNodes, nodeList.Items...)

		log.V(1).Info("Retrieved KWOK nodes page",
			"pageCount", len(nodeList.Items),
			"totalSoFar", len(allNodes),
			"duration", duration,
			"hasMore", nodeList.Continue != "")

		// Check if we need to continue paginating
		if nodeList.Continue == "" {
			break // No more pages
		}
		continueToken = nodeList.Continue

		// Prevent infinite loops
		if len(allNodes) > 10000 {
			log.Error(nil, "Too many nodes detected, stopping pagination", "count", len(allNodes))
			break
		}
	}

	log.Info("Successfully listed all KWOK nodes", "totalCount", len(allNodes))

	return allNodes, nil
}

// Helper function to create int64 pointer
func int64ptr(i int64) *int64 {
	return &i
}

// calculateTargetNamespaces computes how many namespaces should exist based on node count and density
func (r *ScaleLoadConfigReconciler) calculateTargetNamespaces(config *scalev1.ScaleLoadConfig, nodeCount int) int {
	// Default to 0.6 based on must-gather analysis (72 namespaces on 126 nodes)
	namespacesPerNodeStr := "0.6"
	if config.Spec.LoadProfile.NamespacesPerNode != nil {
		namespacesPerNodeStr = *config.Spec.LoadProfile.NamespacesPerNode
	}

	if namespacesPerNode, err := parseFloat(namespacesPerNodeStr); err == nil {
		return int(math.Ceil(float64(nodeCount) * namespacesPerNode))
	}

	// Fallback if parsing fails
	return int(math.Ceil(float64(nodeCount) * 0.6))
}

// manageLoadResources creates/updates/deletes namespaces and their resources
func (r *ScaleLoadConfigReconciler) manageLoadResources(ctx context.Context, config *scalev1.ScaleLoadConfig,
	kwokNodes []corev1.Node, targetNamespaces int) (int, map[string]int, error) {

	log := r.Log.WithName("resource-manager")
	resourceCounts := make(map[string]int)
	var namespacesCreated, namespacesDeleted int

	// Get existing managed namespaces, separated by status
	activeNamespaces, terminatingNamespaces, err := r.getManagedNamespacesWithStatus(ctx, config)
	if err != nil {
		return 0, resourceCounts, fmt.Errorf("failed to get managed namespaces: %w", err)
	}
	r.recordAPICall(config, 1) // List namespaces operation

	// Cache for checkMaximumLimit to avoid repeated getManagedNamespaces per resource type per namespace
	allManaged := make([]corev1.Namespace, 0, len(activeNamespaces)+len(terminatingNamespaces))
	allManaged = append(allManaged, activeNamespaces...)
	allManaged = append(allManaged, terminatingNamespaces...)
	r.currentManagedNamespaces = allManaged
	defer func() { r.currentManagedNamespaces = nil }()

	currentActiveCount := len(activeNamespaces)
	terminatingCount := len(terminatingNamespaces)
	// Count terminating namespaces toward total to avoid creating replacements too early
	currentNamespaceCount := currentActiveCount + terminatingCount

	log.V(1).Info("Namespace status",
		"active", currentActiveCount,
		"terminating", terminatingCount,
		"total", currentNamespaceCount,
		"target", targetNamespaces)

	// Check maximum limit for namespaces if namespace churn is enabled
	effectiveTarget := targetNamespaces
	if config.Spec.ResourceChurn.Namespaces.Enabled && config.Spec.ResourceChurn.Namespaces.Maximum > 0 {
		if currentNamespaceCount >= int(config.Spec.ResourceChurn.Namespaces.Maximum) {
			effectiveTarget = currentNamespaceCount // Don't create more, maintain current count
			log.Info("Namespace creation limited by maximum",
				"requestedTarget", targetNamespaces,
				"effectiveTarget", effectiveTarget,
				"current", currentNamespaceCount,
				"maximum", config.Spec.ResourceChurn.Namespaces.Maximum)
		} else if targetNamespaces > int(config.Spec.ResourceChurn.Namespaces.Maximum) {
			effectiveTarget = int(config.Spec.ResourceChurn.Namespaces.Maximum)
			log.Info("Namespace target reduced by maximum limit",
				"requestedTarget", targetNamespaces,
				"effectiveTarget", effectiveTarget,
				"maximum", config.Spec.ResourceChurn.Namespaces.Maximum)
		}
	}

	log.V(1).Info("Namespace management starting",
		"current", currentNamespaceCount,
		"requestedTarget", targetNamespaces,
		"effectiveTarget", effectiveTarget,
		"kwokNodes", len(kwokNodes))

	// Scale up namespaces if needed
	if currentNamespaceCount < effectiveTarget {
		namespacesToCreate := effectiveTarget - currentNamespaceCount
		log.V(1).Info("Scaling up namespaces", "current", currentNamespaceCount, "target", effectiveTarget, "toCreate", namespacesToCreate)

		if err := r.createNamespaces(ctx, config, kwokNodes, namespacesToCreate); err != nil {
			return currentNamespaceCount, resourceCounts, fmt.Errorf("failed to create namespaces: %w", err)
		}
		namespacesCreated = namespacesToCreate

		// Re-fetch to get updated count (including new namespaces)
		activeNamespaces, terminatingNamespaces, err = r.getManagedNamespacesWithStatus(ctx, config)
		if err != nil {
			return currentNamespaceCount, resourceCounts, err
		}
		currentActiveCount = len(activeNamespaces)
		terminatingCount = len(terminatingNamespaces)
		currentNamespaceCount = currentActiveCount + terminatingCount
		log.V(1).Info("Namespaces created successfully", "created", namespacesCreated, "newActive", currentActiveCount, "stillTerminating", terminatingCount, "newTotal", currentNamespaceCount)
	}

	// Scale down namespaces if needed
	// Only consider excess ACTIVE namespaces for deletion (don't retry terminating ones)
	if currentActiveCount > effectiveTarget {
		namespacesToDelete := currentActiveCount - effectiveTarget
		log.V(1).Info("Scaling down namespaces",
			"currentActive", currentActiveCount,
			"terminating", terminatingCount,
			"total", currentNamespaceCount,
			"target", effectiveTarget,
			"toDelete", namespacesToDelete)

		if err := r.deleteNamespaces(ctx, config, activeNamespaces, namespacesToDelete); err != nil {
			return currentNamespaceCount, resourceCounts, fmt.Errorf("failed to delete namespaces: %w", err)
		}
		namespacesDeleted = namespacesToDelete
		// Update counts: some active namespaces are now terminating
		currentActiveCount -= namespacesToDelete
		terminatingCount += namespacesToDelete
		currentNamespaceCount = currentActiveCount + terminatingCount
		log.V(1).Info("Namespaces deletion initiated",
			"deleted", namespacesDeleted,
			"newActive", currentActiveCount,
			"nowTerminating", terminatingCount,
			"newTotal", currentNamespaceCount)
	}

	// Get the current list of active namespaces for resource processing (skip terminating ones)
	currentNamespaces := activeNamespaces

	// Manage resources within namespaces - PARALLEL PROCESSING
	resourceCounts = r.manageNamespacesParallel(ctx, config, currentNamespaces)

	// Calculate total resource operations
	totalResourceOperations := 0
	for _, count := range resourceCounts {
		totalResourceOperations += count
	}

	log.V(1).Info("Load resource management completed",
		"finalNamespaces", currentNamespaceCount,
		"namespacesCreated", namespacesCreated,
		"namespacesDeleted", namespacesDeleted,
		"totalResourceOperations", totalResourceOperations,
		"resourceBreakdown", resourceCounts,
		"namespacesProcessed", len(currentNamespaces))

	return currentNamespaceCount, resourceCounts, nil
}

// getManagedNamespaces gets namespaces managed by this operator
func (r *ScaleLoadConfigReconciler) getManagedNamespaces(ctx context.Context, config *scalev1.ScaleLoadConfig) ([]corev1.Namespace, error) {
	namespaceList := &corev1.NamespaceList{}

	labelSelector := labels.SelectorFromSet(map[string]string{
		"scale.openshift.io/managed-by": config.Name,
	})

	listOpts := &client.ListOptions{
		LabelSelector: labelSelector,
	}

	if err := r.List(ctx, namespaceList, listOpts); err != nil {
		return nil, err
	}

	return namespaceList.Items, nil
}

// getManagedNamespacesWithStatus gets namespaces managed by this operator and separates by status
func (r *ScaleLoadConfigReconciler) getManagedNamespacesWithStatus(ctx context.Context, config *scalev1.ScaleLoadConfig) (active, terminating []corev1.Namespace, err error) {
	allNamespaces, err := r.getManagedNamespaces(ctx, config)
	if err != nil {
		return nil, nil, err
	}

	for _, ns := range allNamespaces {
		if ns.Status.Phase == corev1.NamespaceTerminating {
			terminating = append(terminating, ns)
		} else {
			active = append(active, ns)
		}
	}

	return active, terminating, nil
}

// createNamespaces creates new namespaces with proper labeling
func (r *ScaleLoadConfigReconciler) createNamespaces(ctx context.Context, config *scalev1.ScaleLoadConfig,
	kwokNodes []corev1.Node, count int) error {

	log := r.Log.WithName("namespace-creator")
	prefix := config.Spec.NamespaceConfig.NamespacePrefix
	if prefix == "" {
		prefix = "openshift-fake-"
	}

	// Get current namespace count to continue indexing sequence
	existingNamespaces, err := r.getManagedNamespaces(ctx, config)
	if err != nil {
		return fmt.Errorf("failed to get existing namespaces for indexing: %w", err)
	}
	startIndex := len(existingNamespaces)

	for i := 0; i < count; i++ {
		// Generate unique namespace name
		namespaceName := fmt.Sprintf("%s%s-%d", prefix, generateRandomString(6), time.Now().Unix()%10000)

		// Select associated node (for resource locality simulation)
		associatedNode := ""
		if len(kwokNodes) > 0 {
			associatedNode = kwokNodes[rand.Intn(len(kwokNodes))].Name
		}

		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespaceName,
				Labels: map[string]string{
					"scale.openshift.io/managed-by":      config.Name,
					"scale.openshift.io/associated-node": associatedNode,
					"scale.openshift.io/created-by":      "sim-operator",
					"scale.openshift.io/namespace-index": fmt.Sprintf("%d", startIndex+i),
				},
			},
		}

		// Add custom labels and annotations
		if config.Spec.NamespaceConfig.Labels != nil {
			for k, v := range config.Spec.NamespaceConfig.Labels {
				namespace.Labels[k] = v
			}
		}

		if config.Spec.NamespaceConfig.Annotations != nil {
			if namespace.Annotations == nil {
				namespace.Annotations = make(map[string]string)
			}
			for k, v := range config.Spec.NamespaceConfig.Annotations {
				namespace.Annotations[k] = v
			}
		}

		if err := r.Create(ctx, namespace); err != nil {
			return fmt.Errorf("failed to create namespace %s: %w", namespaceName, err)
		}
		r.recordAPICall(config, 1) // Create namespace operation

		log.V(1).Info("Created namespace", "name", namespaceName, "associatedNode", associatedNode)

		// Initialize resource manager
		r.resourceManagers[namespaceName] = &ResourceManager{
			namespace:        namespaceName,
			associatedNode:   associatedNode,
			lastUpdate:       time.Now(),
			resourceCounters: make(map[string]int),
			updateTimers:     make(map[string]time.Time),
		}
	}

	return nil
}

// deleteNamespaces removes the specified number of namespaces
func (r *ScaleLoadConfigReconciler) deleteNamespaces(ctx context.Context, config *scalev1.ScaleLoadConfig,
	namespaces []corev1.Namespace, count int) error {

	log := r.Log.WithName("namespace-deleter")

	// Sort by creation time to delete oldest first
	sort.Slice(namespaces, func(i, j int) bool {
		return namespaces[i].CreationTimestamp.Before(&namespaces[j].CreationTimestamp)
	})

	for i := 0; i < count && i < len(namespaces); i++ {
		ns := namespaces[i]

		if config.Spec.CleanupConfig.GracefulDeletes {
			gracePeriod := int64(30)
			deleteOpts := &client.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
			}
			if err := r.Delete(ctx, &ns, deleteOpts); err != nil {
				return fmt.Errorf("failed to delete namespace %s: %w", ns.Name, err)
			}
		} else {
			if err := r.Delete(ctx, &ns); err != nil {
				return fmt.Errorf("failed to delete namespace %s: %w", ns.Name, err)
			}
		}
		r.recordAPICall(config, 1) // Delete namespace operation

		// Clean up resource manager
		delete(r.resourceManagers, ns.Name)

		log.V(1).Info("Deleted namespace", "name", ns.Name)
	}

	return nil
}

// calculateReconcileInterval determines how often to reconcile based on API rate capacity
func (r *ScaleLoadConfigReconciler) calculateReconcileInterval(config *scalev1.ScaleLoadConfig, nodeCount int) time.Duration {
	// Fast reconcile for node detection scenarios
	if nodeCount == 0 {
		return 5 * time.Second // Faster for cleanup and node detection
	}

	// For active workload management, use a balanced approach
	// that prioritizes responsiveness over pure API rate optimization

	if nodeCount <= 100 {
		return 5 * time.Second // Small clusters - very responsive
	} else if nodeCount <= 1000 {
		return 10 * time.Second // Medium clusters - responsive
	} else {
		return 15 * time.Second // Large clusters - still responsive but slightly slower
	}
}

// ensureAPICallRate makes additional API calls to meet the configured target rate.
// This creates synthetic load (e.g. List with Limit: 1) when real workload is below target;
// it is intentional for rate-based testing. To avoid synthetic load, configure a lower target rate.
func (r *ScaleLoadConfigReconciler) ensureAPICallRate(ctx context.Context, config *scalev1.ScaleLoadConfig, nodeCount int) {
	now := time.Now()

	// Calculate target rate
	effectiveRate, _ := r.getEffectiveAPIRate(config, nodeCount)
	r.targetAPICallsPerMinute = effectiveRate

	// Reset counter every minute
	if r.lastRateReset.IsZero() || now.Sub(r.lastRateReset) >= time.Minute {
		r.apiCallsThisMinute = 0
		r.lastRateReset = now
	}

	// Calculate how many more API calls needed this minute
	elapsedSeconds := now.Sub(r.lastRateReset).Seconds()
	expectedCallsByNow := int32(float64(r.targetAPICallsPerMinute) * (elapsedSeconds / 60.0))
	callsNeeded := expectedCallsByNow - r.apiCallsThisMinute

	if callsNeeded <= 0 {
		// If we're significantly over target, warn and skip additional calls
		if r.apiCallsThisMinute > r.targetAPICallsPerMinute*2 {
			r.Log.WithName("rate-controller").Info("API call rate significantly above target - skipping additional calls to prevent overload",
				"current", r.apiCallsThisMinute,
				"target", r.targetAPICallsPerMinute,
				"overagePercent", int(float64(r.apiCallsThisMinute-r.targetAPICallsPerMinute)/float64(r.targetAPICallsPerMinute)*100))
		}
		return // Already meeting or exceeding target
	}

	log := r.Log.WithName("rate-controller")
	log.V(1).Info("Making additional API calls to meet target",
		"target", r.targetAPICallsPerMinute,
		"currentThisMinute", r.apiCallsThisMinute,
		"expected", expectedCallsByNow,
		"needed", callsNeeded)

	// Make additional API calls using simple operations
	r.makeAdditionalAPICalls(ctx, config, callsNeeded)
}

// makeAdditionalAPICalls performs simple API operations (List with Limit: 1) to meet the configured rate target.
// This is synthetic load; see ensureAPICallRate for behavior and alternatives.
func (r *ScaleLoadConfigReconciler) makeAdditionalAPICalls(ctx context.Context, config *scalev1.ScaleLoadConfig, count int32) {
	log := r.Log.WithName("rate-controller")

	// Remove the artificial 100 call limit - we need to hit the target!
	// Use batches to avoid overwhelming the API server
	batchSize := int32(500) // Process in batches of 500
	totalCalls := count

	log.Info("Making additional API calls", "needed", count, "batchSize", batchSize)

	for remaining := totalCalls; remaining > 0; {
		currentBatch := remaining
		if currentBatch > batchSize {
			currentBatch = batchSize
		}

		// Make batch of simple API calls
		for i := int32(0); i < currentBatch; i++ {
			// Use simple List operations with small limits
			namespaceList := &corev1.NamespaceList{}
			if err := r.List(ctx, namespaceList, &client.ListOptions{Limit: 1}); err == nil {
				r.recordAPICall(config, 1)
			} else {
				log.V(2).Info("API call failed", "error", err.Error())
			}
		}

		remaining -= currentBatch
		log.V(1).Info("Completed batch", "batchSize", currentBatch, "remaining", remaining)
	}

	log.Info("Additional API calls completed", "totalRequested", count)
}

// performNamespaceChurn deletes and recreates namespaces to generate API churn
func (r *ScaleLoadConfigReconciler) performNamespaceChurn(ctx context.Context, config *scalev1.ScaleLoadConfig) error {
	log := r.Log.WithName("namespace-churn")

	// Check if enough time has passed since last churn
	if !r.lastReconcileTime.IsZero() && time.Since(r.lastReconcileTime) < time.Duration(config.Spec.ResourceChurn.Namespaces.ChurnIntervalSeconds)*time.Second {
		return nil // Too soon to churn
	}

	// Get existing managed namespaces
	existingNamespaces, err := r.getManagedNamespaces(ctx, config)
	if err != nil {
		return fmt.Errorf("failed to get existing namespaces for churn: %w", err)
	}
	r.recordAPICall(config, 1) // List namespaces operation

	if len(existingNamespaces) == 0 {
		log.V(1).Info("No namespaces to churn")
		return nil
	}

	// Calculate how many namespaces to churn
	churnPercentage := config.Spec.ResourceChurn.Namespaces.ChurnPercentage
	if churnPercentage == 0 {
		churnPercentage = 5 // Default 5%
	}

	namespaceCount := len(existingNamespaces)
	namespacesToChurn := int((int64(namespaceCount) * int64(churnPercentage)) / 100)
	if namespacesToChurn < 1 && namespaceCount > 0 {
		namespacesToChurn = 1 // Always churn at least one if we have namespaces
	}

	// Preserve oldest namespaces for stability
	preserveCount := config.Spec.ResourceChurn.Namespaces.PreserveOldestNamespaces
	if preserveCount == 0 {
		preserveCount = 10
	}

	if namespacesToChurn >= namespaceCount-int(preserveCount) {
		namespacesToChurn = namespaceCount - int(preserveCount)
		if namespacesToChurn < 0 {
			namespacesToChurn = 0
		}
	}

	if namespacesToChurn == 0 {
		log.V(1).Info("No namespaces available for churning after preservation rules",
			"total", namespaceCount, "preserve", preserveCount)
		return nil
	}

	log.Info("Starting namespace churn",
		"totalNamespaces", namespaceCount,
		"churnPercentage", churnPercentage,
		"namespacesToChurn", namespacesToChurn,
		"preserveCount", preserveCount)

	// Select namespaces to churn (skip preserved ones)
	candidateNamespaces := existingNamespaces[preserveCount:]
	if len(candidateNamespaces) < namespacesToChurn {
		namespacesToChurn = len(candidateNamespaces)
	}

	// Randomly select namespaces to churn from candidates
	churned := 0
	for i := 0; i < namespacesToChurn && i < len(candidateNamespaces); i++ {
		ns := candidateNamespaces[i]

		log.V(1).Info("Churning namespace", "namespace", ns.Name)

		// Delete the namespace
		if err := r.Delete(ctx, &ns); err != nil {
			log.Error(err, "Failed to delete namespace for churn", "namespace", ns.Name)
			continue
		}
		r.recordAPICall(config, 1) // Delete operation

		// Create a replacement namespace immediately
		newNamespace := r.generateNamespace(config, ns.Name+"-new")
		if err := r.Create(ctx, newNamespace); err != nil {
			log.Error(err, "Failed to create replacement namespace", "namespace", newNamespace.Name)
			continue
		}
		r.recordAPICall(config, 1) // Create operation

		churned++
	}

	log.Info("Namespace churn completed",
		"requested", namespacesToChurn,
		"churned", churned,
		"apiCalls", churned*2) // 2 API calls per churn (delete + create)

	return nil
}

// generateNamespace creates a new namespace with standard labels and config
func (r *ScaleLoadConfigReconciler) generateNamespace(config *scalev1.ScaleLoadConfig, namespaceName string) *corev1.Namespace {
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespaceName,
			Labels: map[string]string{
				"scale.openshift.io/managed-by": config.Name,
				"scale.openshift.io/created-by": "sim-operator",
				"scale.openshift.io/churned":    "true", // Mark as churned namespace
				"scale.openshift.io/churn-time": fmt.Sprintf("%d", time.Now().Unix()),
			},
		},
	}

	// Add custom labels and annotations
	if config.Spec.NamespaceConfig.Labels != nil {
		for k, v := range config.Spec.NamespaceConfig.Labels {
			namespace.Labels[k] = v
		}
	}

	if config.Spec.NamespaceConfig.Annotations != nil {
		if namespace.Annotations == nil {
			namespace.Annotations = make(map[string]string)
		}
		for k, v := range config.Spec.NamespaceConfig.Annotations {
			namespace.Annotations[k] = v
		}
	}

	return namespace
}

// updateNodeCountStatus provides early status update for node count to prevent stale status
func (r *ScaleLoadConfigReconciler) updateNodeCountStatus(ctx context.Context, config *scalev1.ScaleLoadConfig, nodeCount int) error {
	// Retry logic to handle concurrent modifications
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		// Get fresh copy to avoid conflicts
		freshConfig := &scalev1.ScaleLoadConfig{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(config), freshConfig); err != nil {
			return fmt.Errorf("failed to get fresh config for status update: %w", err)
		}

		// Only update node count, preserve other fields
		freshConfig.Status.KwokNodeCount = int32(nodeCount)
		now := metav1.NewTime(time.Now())
		freshConfig.Status.LastReconcileTime = &now

		// Quick status update with retry
		if err := r.Status().Update(ctx, freshConfig); err != nil {
			// If it's a conflict error and we have retries left, try again
			if strings.Contains(err.Error(), "object has been modified") && attempt < maxRetries-1 {
				time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond) // Exponential backoff
				continue
			}
			return fmt.Errorf("failed to update node count status after %d attempts: %w", attempt+1, err)
		}

		return nil // Success
	}

	return fmt.Errorf("failed to update node count status after %d retries", maxRetries)
}

// calculateNextReconcileResult returns appropriate reconcile result when skipping full processing
func (r *ScaleLoadConfigReconciler) calculateNextReconcileResult(config *scalev1.ScaleLoadConfig, nodeCount int) (ctrl.Result, error) {
	nextReconcile := r.calculateReconcileInterval(config, nodeCount)
	return ctrl.Result{RequeueAfter: nextReconcile}, nil
}

// manageNamespacesParallel processes multiple namespaces concurrently for better performance
func (r *ScaleLoadConfigReconciler) manageNamespacesParallel(ctx context.Context, config *scalev1.ScaleLoadConfig, namespaces []corev1.Namespace) map[string]int {
	log := r.Log.WithName("parallel-manager")

	// Implement smart namespace batching based on cluster size
	maxNamespacesPerReconcile := r.calculateOptimalBatchSize(len(namespaces))
	if len(namespaces) > maxNamespacesPerReconcile {
		log.Info("Applying namespace batching for optimal performance",
			"totalNamespaces", len(namespaces),
			"processingThisCycle", maxNamespacesPerReconcile,
			"batchingStrategy", "performance-optimized")
		namespaces = namespaces[:maxNamespacesPerReconcile]
	}

	// Configure concurrency based on API rate capacity and number of namespaces
	maxConcurrency := r.calculateOptimalConcurrency(config, len(namespaces))
	log.Info("Starting parallel namespace processing",
		"namespaces", len(namespaces),
		"maxConcurrency", maxConcurrency)

	// Create semaphore for controlling concurrency
	semaphore := make(chan struct{}, maxConcurrency)

	// Results collection
	resultsChan := make(chan namespaceResult, len(namespaces))
	var wg sync.WaitGroup

	// Start time for performance measurement
	startTime := time.Now()

	// Process namespaces in parallel
	for _, ns := range namespaces {
		// Check if namespace is ready before starting goroutine
		if !r.isNamespaceReady(ctx, ns.Name) {
			log.V(1).Info("Namespace not ready, skipping", "namespace", ns.Name)
			continue
		}

		wg.Add(1)
		go func(namespace corev1.Namespace) {
			defer wg.Done()

			// Acquire semaphore (rate limiting)
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// Process single namespace
			r.processNamespaceWithResult(ctx, config, namespace, resultsChan)
		}(ns)
	}

	// Wait for all goroutines to complete
	wg.Wait()
	close(resultsChan)

	// Aggregate results from all namespaces
	aggregatedCounts := make(map[string]int)
	var totalAPIcalls, successfulNamespaces, failedNamespaces int32

	for result := range resultsChan {
		if result.err != nil {
			log.Error(result.err, "Failed to manage namespace resources", "namespace", result.namespace)
			failedNamespaces++
		} else {
			successfulNamespaces++
			// Aggregate resource counts
			for resourceType, count := range result.resourceCounts {
				aggregatedCounts[resourceType] += count
				log.V(2).Info("Aggregating resource count",
					"namespace", result.namespace,
					"resourceType", resourceType,
					"count", count,
					"newTotal", aggregatedCounts[resourceType])
			}
			totalAPIcalls += result.apiCalls
		}
	}

	duration := time.Since(startTime)
	namespacesPerSecond := float64(len(namespaces)) / duration.Seconds()

	log.Info("Parallel namespace processing completed",
		"duration", duration,
		"totalNamespaces", len(namespaces),
		"successful", successfulNamespaces,
		"failed", failedNamespaces,
		"concurrency", maxConcurrency,
		"namespacesPerSecond", fmt.Sprintf("%.1f", namespacesPerSecond),
		"totalAPIcalls", totalAPIcalls,
		"aggregatedCounts", aggregatedCounts)

	// Log individual resource type totals for debugging
	log.Info("Final aggregated resource counts for status update",
		"configMaps", aggregatedCounts["configMaps"],
		"secrets", aggregatedCounts["secrets"],
		"pods", aggregatedCounts["pods"],
		"routes", aggregatedCounts["routes"],
		"imageStreams", aggregatedCounts["imageStreams"],
		"buildConfigs", aggregatedCounts["buildConfigs"],
		"events", aggregatedCounts["events"])

	return aggregatedCounts
}

// namespaceResult holds the result of processing a single namespace
type namespaceResult struct {
	namespace      string
	resourceCounts map[string]int
	apiCalls       int32
	err            error
}

// processNamespaceWithResult processes a single namespace and sends results to channel
func (r *ScaleLoadConfigReconciler) processNamespaceWithResult(ctx context.Context, config *scalev1.ScaleLoadConfig, namespace corev1.Namespace, resultsChan chan<- namespaceResult) {
	startTime := time.Now()

	counts, err := r.manageNamespaceResources(ctx, config, namespace)

	duration := time.Since(startTime)
	log := r.Log.WithName("namespace-worker")
	log.V(2).Info("Namespace processing completed",
		"namespace", namespace.Name,
		"duration", duration,
		"resourceCounts", counts,
		"success", err == nil)

	// Approximate API calls from resource operations (list + create/update/delete per resource type)
	approxAPICalls := int32(0)
	for _, c := range counts {
		approxAPICalls += int32(c)
	}
	resultsChan <- namespaceResult{
		namespace:      namespace.Name,
		resourceCounts: counts,
		apiCalls:       approxAPICalls,
		err:            err,
	}
}

// calculateOptimalBatchSize determines optimal namespace batch size for performance
func (r *ScaleLoadConfigReconciler) calculateOptimalBatchSize(totalNamespaces int) int {
	// For high performance, process more namespaces per reconcile
	// but ensure we don't exceed reconcile timeout (5 minutes)

	if totalNamespaces <= 100 {
		return totalNamespaces // Process all if small
	} else if totalNamespaces <= 500 {
		return 150 // Medium batches for medium clusters
	} else {
		return 200 // Larger batches for large clusters
	}
}

// calculateOptimalConcurrency determines the optimal number of concurrent goroutines based on API rate
func (r *ScaleLoadConfigReconciler) calculateOptimalConcurrency(config *scalev1.ScaleLoadConfig, namespaceCount int) int {
	// Conservative concurrency for rate-limited environment to prevent resource conflicts
	// Lower concurrency reduces conflicts and helps stay within API rate targets

	baseConcurrency := 5 // Start with low base concurrency for rate-limited environment

	// Scale conservatively with namespace count to prevent conflicts
	if namespaceCount > 50 {
		baseConcurrency = 8
	}
	if namespaceCount > 100 {
		baseConcurrency = 12
	}
	if namespaceCount > 200 {
		baseConcurrency = 15
	}

	// Don't create more goroutines than namespaces
	if namespaceCount < baseConcurrency {
		return namespaceCount
	}

	// Cap at conservative maximum to prevent resource conflicts
	maxConcurrency := 20
	if baseConcurrency > maxConcurrency {
		return maxConcurrency
	}

	return baseConcurrency
}

// parseFloat parses a string as float64
func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

// SetupWithManager sets up the controller with the Manager
func (r *ScaleLoadConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Initialize metrics
	r.initializeMetrics()

	// Initialize deletion manager for complex resources
	r.deletionManager = NewDeletionManager(r)

	// Watch ScaleLoadConfig resources and Node changes for immediate response
	return ctrl.NewControllerManagedBy(mgr).
		For(&scalev1.ScaleLoadConfig{}).
		Watches(&corev1.Node{}, &NodeEventHandler{Client: mgr.GetClient()}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1, // Single threaded for simplicity
		}).
		Complete(r)
}

// initializeMetrics sets up Prometheus metrics
func (r *ScaleLoadConfigReconciler) initializeMetrics() {
	r.KwokNodeCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kwok_load_generator_nodes_total",
		Help: "Current number of KWOK nodes being monitored",
	})

	r.GeneratedNamespaces = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "kwok_load_generator_namespaces_total",
		Help: "Current number of generated namespaces",
	})

	r.APICallRate = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "kwok_load_generator_api_calls_duration_seconds",
		Help:    "Time taken for API calls",
		Buckets: prometheus.DefBuckets,
	})

	r.ReconcileTime = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "kwok_load_generator_reconcile_duration_seconds",
		Help:    "Time taken for reconcile loops",
		Buckets: prometheus.DefBuckets,
	})

	r.ErrorCount = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "kwok_load_generator_errors_total",
		Help: "Total number of errors encountered",
	})

	// Register metrics
	prometheus.MustRegister(r.KwokNodeCount, r.GeneratedNamespaces, r.APICallRate, r.ReconcileTime, r.ErrorCount)
}

// performOrphanCleanup removes resources (especially pods) that are stuck on deleted KWOK nodes
func (r *ScaleLoadConfigReconciler) performOrphanCleanup(ctx context.Context, config *scalev1.ScaleLoadConfig) error {
	log := r.Log.WithName("orphan-cleanup")

	// Get all existing KWOK nodes to build a list of valid node names
	kwokNodes, err := r.getKwokNodes(ctx, config.Spec.KwokNodeSelector)
	if err != nil {
		return fmt.Errorf("failed to get KWOK nodes for orphan cleanup: %w", err)
	}

	validNodeNames := make(map[string]bool)
	for _, node := range kwokNodes {
		validNodeNames[node.Name] = true
	}

	log.V(1).Info("Starting orphan cleanup", "validKwokNodes", len(validNodeNames))

	// Get managed namespaces
	managedNamespaces, err := r.getManagedNamespaces(ctx, config)
	if err != nil {
		return fmt.Errorf("failed to get managed namespaces for orphan cleanup: %w", err)
	}

	var orphanPodsFound, orphanPodsDeleted int

	// Check each managed namespace for orphaned pods
	for _, namespace := range managedNamespaces {
		podList := &corev1.PodList{}
		listOpts := &client.ListOptions{
			Namespace: namespace.Name,
		}

		if err := r.List(ctx, podList, listOpts); err != nil {
			log.Error(err, "Failed to list pods in namespace", "namespace", namespace.Name)
			continue
		}

		for _, pod := range podList.Items {
			// Check if pod is assigned to a node that no longer exists
			if pod.Spec.NodeName != "" && !validNodeNames[pod.Spec.NodeName] {
				orphanPodsFound++

				// Check if this pod was created by sim-operator (has our labels)
				if labels := pod.Labels; labels != nil {
					if managedBy, exists := labels["scale.openshift.io/managed-by"]; exists && managedBy == config.Name {
						log.Info("Force deleting orphaned pod",
							"namespace", pod.Namespace,
							"pod", pod.Name,
							"deletedNode", pod.Spec.NodeName)

						// Force delete the pod immediately
						gracePeriod := int64(0)
						deleteOpts := &client.DeleteOptions{
							GracePeriodSeconds: &gracePeriod,
						}

						if err := r.Delete(ctx, &pod, deleteOpts); err != nil {
							log.Error(err, "Failed to force delete orphaned pod",
								"namespace", pod.Namespace, "pod", pod.Name)
						} else {
							orphanPodsDeleted++
							r.recordAPICall(config, 1) // Count the delete operation
						}
					}
				}
			}
		}
	}

	if orphanPodsFound > 0 {
		log.Info("Orphan cleanup completed",
			"orphanPodsFound", orphanPodsFound,
			"orphanPodsDeleted", orphanPodsDeleted,
			"validKwokNodes", len(validNodeNames))
	}

	return nil
}

// NodeEventHandler handles Node events to trigger ScaleLoadConfig reconciliation
type NodeEventHandler struct {
	Client client.Client
}

// Create handles node creation events
func (h *NodeEventHandler) Create(ctx context.Context, evt event.CreateEvent, q workqueue.RateLimitingInterface) {
	// Only care about KWOK nodes
	if node, ok := evt.Object.(*corev1.Node); ok {
		if labels := node.GetLabels(); labels != nil {
			if nodeType, exists := labels["type"]; exists && nodeType == "kwok" {
				// Enqueue all ScaleLoadConfigs for reconciliation
				h.enqueueAllConfigs(ctx, q)
			}
		}
	}
}

// Update handles node update events
func (h *NodeEventHandler) Update(ctx context.Context, evt event.UpdateEvent, q workqueue.RateLimitingInterface) {
	// Check if this affects KWOK nodes
	if node, ok := evt.ObjectNew.(*corev1.Node); ok {
		if labels := node.GetLabels(); labels != nil {
			if nodeType, exists := labels["type"]; exists && nodeType == "kwok" {
				h.enqueueAllConfigs(ctx, q)
			}
		}
	}
}

// Delete handles node deletion events
func (h *NodeEventHandler) Delete(ctx context.Context, evt event.DeleteEvent, q workqueue.RateLimitingInterface) {
	// Only care about KWOK nodes
	if node, ok := evt.Object.(*corev1.Node); ok {
		if labels := node.GetLabels(); labels != nil {
			if nodeType, exists := labels["type"]; exists && nodeType == "kwok" {
				// Enqueue all ScaleLoadConfigs for reconciliation
				h.enqueueAllConfigs(ctx, q)
			}
		}
	}
}

// Generic handles other events
func (h *NodeEventHandler) Generic(ctx context.Context, evt event.GenericEvent, q workqueue.RateLimitingInterface) {
	// Handle generic events for KWOK nodes
	if node, ok := evt.Object.(*corev1.Node); ok {
		if labels := node.GetLabels(); labels != nil {
			if nodeType, exists := labels["type"]; exists && nodeType == "kwok" {
				h.enqueueAllConfigs(ctx, q)
			}
		}
	}
}

// enqueueAllConfigs adds all ScaleLoadConfigs to the reconcile queue so node changes trigger reconciliation for every config.
func (h *NodeEventHandler) enqueueAllConfigs(ctx context.Context, q workqueue.RateLimitingInterface) {
	if h.Client == nil {
		return
	}
	configList := &scalev1.ScaleLoadConfigList{}
	if err := h.Client.List(ctx, configList); err != nil {
		return
	}
	for i := range configList.Items {
		config := &configList.Items[i]
		q.Add(reconcile.Request{NamespacedName: types.NamespacedName{Name: config.Name, Namespace: config.Namespace}})
	}
}

// shouldThrottleOperations checks if we're exceeding API rate targets and should slow down
func (r *ScaleLoadConfigReconciler) shouldThrottleOperations(config *scalev1.ScaleLoadConfig, nodeCount int) bool {
	now := time.Now()

	// Calculate target rate
	effectiveRate, _ := r.getEffectiveAPIRate(config, nodeCount)

	// Reset counter every minute
	if r.lastRateReset.IsZero() || now.Sub(r.lastRateReset) >= time.Minute {
		r.apiCallsThisMinute = 0
		r.lastRateReset = now
		return false // Fresh minute, don't throttle
	}

	// Check if we're significantly over target (more than 150% of target)
	if r.apiCallsThisMinute > effectiveRate*3/2 {
		r.Log.V(1).Info("Throttling operations due to high API rate",
			"currentRate", r.apiCallsThisMinute,
			"targetRate", effectiveRate,
			"overage", r.apiCallsThisMinute-effectiveRate)
		return true
	}

	return false
}
