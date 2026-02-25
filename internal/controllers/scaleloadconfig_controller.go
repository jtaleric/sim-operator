package controllers

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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
	lastMetricsReset  time.Time
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

	// Manage namespaces and resources
	log.V(1).Info("Starting load resource management",
		"targetNamespaces", targetNamespaces,
		"kwokNodes", len(kwokNodes))

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

	log.Info("Load resource management completed",
		"namespaceCount", namespaceCount,
		"totalResources", totalResources,
		"reconcileDuration", time.Since(startTime).String())

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

	// Perform namespace churn if enabled
	if config.Spec.ResourceChurn.Namespaces.Enabled {
		if err := r.performNamespaceChurn(ctx, config); err != nil {
			log.Error(err, "Failed to perform namespace churn")
		}
	}

	// Perform additional API calls to meet target rate if needed
	r.ensureAPICallRate(ctx, config, len(kwokNodes))

	// Calculate next reconcile interval based on load profile
	nextReconcile := r.calculateReconcileInterval(config)
	log.V(1).Info("Next reconcile scheduled", "interval", nextReconcile)

	return ctrl.Result{RequeueAfter: nextReconcile}, nil
}

// getKwokNodes retrieves nodes matching the KWOK selector
func (r *ScaleLoadConfigReconciler) getKwokNodes(ctx context.Context, selector map[string]string) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}

	if len(selector) == 0 {
		selector = map[string]string{"type": "kwok"}
	}

	labelSelector := labels.SelectorFromSet(selector)
	listOpts := &client.ListOptions{
		LabelSelector: labelSelector,
	}

	if err := r.List(ctx, nodeList, listOpts); err != nil {
		return nil, fmt.Errorf("failed to list KWOK nodes: %w", err)
	}

	return nodeList.Items, nil
}

// calculateTargetNamespaces computes how many namespaces should exist based on node count and profile
func (r *ScaleLoadConfigReconciler) calculateTargetNamespaces(config *scalev1.ScaleLoadConfig, nodeCount int) int {
	if config.Spec.LoadProfile.NamespacesPerNode != nil {
		if namespacesPerNode, err := parseFloat(*config.Spec.LoadProfile.NamespacesPerNode); err == nil {
			return int(math.Ceil(float64(nodeCount) * namespacesPerNode))
		}
		return int(math.Ceil(float64(nodeCount) * 0.6)) // fallback
	}

	// Default profiles based on must-gather analysis
	switch config.Spec.LoadProfile.Profile {
	case "development":
		return int(math.Ceil(float64(nodeCount) * 0.2)) // Light load
	case "staging":
		return int(math.Ceil(float64(nodeCount) * 0.4)) // Medium load
	case "extreme":
		return int(math.Ceil(float64(nodeCount) * 1.0)) // Heavy load
	default: // "production"
		return int(math.Ceil(float64(nodeCount) * 0.6)) // Based on 126 nodes -> 72 namespaces
	}
}

// manageLoadResources creates/updates/deletes namespaces and their resources
func (r *ScaleLoadConfigReconciler) manageLoadResources(ctx context.Context, config *scalev1.ScaleLoadConfig,
	kwokNodes []corev1.Node, targetNamespaces int) (int, map[string]int, error) {

	log := r.Log.WithName("resource-manager")
	resourceCounts := make(map[string]int)
	var namespacesCreated, namespacesDeleted int

	// Get existing managed namespaces
	existingNamespaces, err := r.getManagedNamespaces(ctx, config)
	if err != nil {
		return 0, resourceCounts, fmt.Errorf("failed to get managed namespaces: %w", err)
	}
	r.recordAPICall(config, 1) // List namespaces operation

	currentNamespaceCount := len(existingNamespaces)
	log.V(1).Info("Namespace management starting",
		"current", currentNamespaceCount,
		"target", targetNamespaces,
		"kwokNodes", len(kwokNodes))

	// Scale up namespaces if needed
	if currentNamespaceCount < targetNamespaces {
		namespacesToCreate := targetNamespaces - currentNamespaceCount
		log.V(1).Info("Scaling up namespaces", "current", currentNamespaceCount, "target", targetNamespaces, "toCreate", namespacesToCreate)

		if err := r.createNamespaces(ctx, config, kwokNodes, namespacesToCreate); err != nil {
			return currentNamespaceCount, resourceCounts, fmt.Errorf("failed to create namespaces: %w", err)
		}
		namespacesCreated = namespacesToCreate

		// Re-fetch to get updated count
		existingNamespaces, err = r.getManagedNamespaces(ctx, config)
		if err != nil {
			return currentNamespaceCount, resourceCounts, err
		}
		currentNamespaceCount = len(existingNamespaces)
		log.V(1).Info("Namespaces created successfully", "created", namespacesCreated, "newTotal", currentNamespaceCount)
	}

	// Scale down namespaces if needed
	if currentNamespaceCount > targetNamespaces {
		namespacesToDelete := currentNamespaceCount - targetNamespaces
		log.V(1).Info("Scaling down namespaces", "current", currentNamespaceCount, "target", targetNamespaces, "toDelete", namespacesToDelete)

		if err := r.deleteNamespaces(ctx, config, existingNamespaces, namespacesToDelete); err != nil {
			return currentNamespaceCount, resourceCounts, fmt.Errorf("failed to delete namespaces: %w", err)
		}
		namespacesDeleted = namespacesToDelete
		currentNamespaceCount = targetNamespaces
		log.V(1).Info("Namespaces deleted successfully", "deleted", namespacesDeleted, "newTotal", currentNamespaceCount)
	}

	// Get the current list of managed namespaces (including newly created ones)
	currentNamespaces, err := r.getManagedNamespaces(ctx, config)
	if err != nil {
		return currentNamespaceCount, resourceCounts, fmt.Errorf("failed to get current namespaces: %w", err)
	}

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

// calculateReconcileInterval determines how often to reconcile based on load profile
func (r *ScaleLoadConfigReconciler) calculateReconcileInterval(config *scalev1.ScaleLoadConfig) time.Duration {
	switch config.Spec.LoadProfile.Profile {
	case "development":
		return 30 * time.Second // Less frequent for light load
	case "staging":
		return 15 * time.Second // Moderate frequency
	case "extreme":
		return 5 * time.Second // High frequency for heavy load
	default: // "production"
		return 10 * time.Second // Much more aggressive to spread API call load
	}
}

// ensureAPICallRate makes additional API calls to meet the configured target rate
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

// makeAdditionalAPICalls performs simple API operations to meet rate target
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

// generateRandomString creates a random string for unique naming

// manageNamespacesParallel processes multiple namespaces concurrently for better performance
func (r *ScaleLoadConfigReconciler) manageNamespacesParallel(ctx context.Context, config *scalev1.ScaleLoadConfig, namespaces []corev1.Namespace) map[string]int {
	log := r.Log.WithName("parallel-manager")

	// Configure concurrency based on load profile and number of namespaces
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

	resultsChan <- namespaceResult{
		namespace:      namespace.Name,
		resourceCounts: counts,
		apiCalls:       0, // TODO: Track API calls per namespace
		err:            err,
	}
}

// calculateOptimalConcurrency determines the optimal number of concurrent goroutines
func (r *ScaleLoadConfigReconciler) calculateOptimalConcurrency(config *scalev1.ScaleLoadConfig, namespaceCount int) int {
	// Base concurrency on load profile
	var baseConcurrency int
	switch config.Spec.LoadProfile.Profile {
	case "development":
		baseConcurrency = 10 // Conservative for light load
	case "staging":
		baseConcurrency = 25 // Moderate parallelism
	case "extreme":
		baseConcurrency = 100 // Aggressive parallelism
	default: // "production"
		baseConcurrency = 50 // Balanced approach
	}

	// Adjust based on number of namespaces
	if namespaceCount < baseConcurrency {
		// Don't create more goroutines than namespaces
		return namespaceCount
	}

	// Cap maximum concurrency to avoid overwhelming API server
	maxConcurrency := 100
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

	// Watch ScaleLoadConfig resources
	// Build and return the controller
	return ctrl.NewControllerManagedBy(mgr).
		For(&scalev1.ScaleLoadConfig{}).
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
