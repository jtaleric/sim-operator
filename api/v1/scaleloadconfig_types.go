package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ScaleLoadConfigSpec defines the desired state of ScaleLoadConfig
type ScaleLoadConfigSpec struct {
	// Enabled controls whether load generation is active
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// KwokNodeSelector defines labels to identify KWOK nodes
	// +kubebuilder:default={"type":"kwok"}
	KwokNodeSelector map[string]string `json:"kwokNodeSelector,omitempty"`

	// LoadProfile defines the intensity and pattern of load generation
	LoadProfile LoadProfile `json:"loadProfile"`

	// NamespaceConfig controls simulated namespace creation and resource density
	NamespaceConfig NamespaceConfig `json:"namespaceConfig"`

	// AnnotationChurn controls node annotation update patterns
	AnnotationChurn AnnotationChurnConfig `json:"annotationChurn"`

	// ResourceChurn controls resource creation/update/deletion patterns
	ResourceChurn ResourceChurnConfig `json:"resourceChurn"`

	// CleanupConfig controls resource cleanup when KWOK nodes are removed
	CleanupConfig CleanupConfig `json:"cleanupConfig"`
}

// LoadProfile defines the overall load characteristics
type LoadProfile struct {
	// Profile selects predefined load patterns
	// +kubebuilder:validation:Enum=development;staging;production;extreme
	// +kubebuilder:default=production
	Profile string `json:"profile,omitempty"`

	// NamespacesPerNode controls namespace density (overrides profile defaults)
	// Based on must-gather analysis: ~0.6 namespaces per node (72/126)
	// +kubebuilder:default="0.6"
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	NamespacesPerNode *string `json:"namespacesPerNode,omitempty"`

	// ResourcesPerNamespace controls resource density per namespace
	// +kubebuilder:default=5
	ResourcesPerNamespace *int32 `json:"resourcesPerNamespace,omitempty"`

	// APICallRate controls the frequency of API operations (calls per minute per node)
	// +kubebuilder:default=20
	APICallRate *int32 `json:"apiCallRate,omitempty"`
}

// NamespaceConfig controls namespace patterns
type NamespaceConfig struct {
	// NamespacePrefix for generated namespaces
	// +kubebuilder:default="openshift-fake-"
	NamespacePrefix string `json:"namespacePrefix,omitempty"`

	// Labels to apply to generated namespaces
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations to apply to generated namespaces
	Annotations map[string]string `json:"annotations,omitempty"`

	// ResourceQuota settings for generated namespaces
	ResourceQuota *NamespaceResourceQuota `json:"resourceQuota,omitempty"`
}

// NamespaceResourceQuota defines resource limits for generated namespaces
type NamespaceResourceQuota struct {
	// CPU limits (in millicores)
	CPU string `json:"cpu,omitempty"`
	// Memory limits
	Memory string `json:"memory,omitempty"`
	// Storage limits
	Storage string `json:"storage,omitempty"`
	// Pod count limits
	Pods int32 `json:"pods,omitempty"`
}

// AnnotationChurnConfig controls node annotation update patterns
type AnnotationChurnConfig struct {
	// Enabled controls whether annotation churn is active
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// NetworkingAnnotations simulates OVN/networking annotation churn
	// +kubebuilder:default=true
	NetworkingAnnotations bool `json:"networkingAnnotations,omitempty"`

	// MachineConfigAnnotations simulates machine config annotation churn
	// +kubebuilder:default=true
	MachineConfigAnnotations bool `json:"machineConfigAnnotations,omitempty"`

	// UpdateIntervalMin minimum interval between annotation updates (seconds)
	// +kubebuilder:default=60
	UpdateIntervalMin int32 `json:"updateIntervalMin,omitempty"`

	// UpdateIntervalMax maximum interval between annotation updates (seconds)
	// +kubebuilder:default=300
	UpdateIntervalMax int32 `json:"updateIntervalMax,omitempty"`
}

// ResourceChurnConfig controls resource lifecycle patterns
type ResourceChurnConfig struct {
	// ConfigMaps controls ConfigMap resource patterns
	ConfigMaps ResourceTypeConfig `json:"configMaps,omitempty"`

	// Secrets controls Secret resource patterns
	Secrets ResourceTypeConfig `json:"secrets,omitempty"`

	// Routes controls Route resource patterns
	Routes ResourceTypeConfig `json:"routes,omitempty"`

	// ImageStreams controls ImageStream resource patterns
	ImageStreams ResourceTypeConfig `json:"imageStreams,omitempty"`

	// BuildConfigs controls BuildConfig resource patterns
	BuildConfigs ResourceTypeConfig `json:"buildConfigs,omitempty"`

	// Events controls Event generation patterns
	Events EventsConfig `json:"events,omitempty"`
}

// ResourceTypeConfig defines behavior for specific resource types
type ResourceTypeConfig struct {
	// Enabled controls whether this resource type is generated
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// Count per namespace
	// +kubebuilder:default=1
	Count int32 `json:"count,omitempty"`

	// NamespaceInterval controls how often this resource is created relative to namespaces
	// For example, interval=10 means create this resource in every 10th namespace
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	NamespaceInterval int32 `json:"namespaceInterval,omitempty"`

	// UpdateFrequencyMin minimum time between updates (seconds)
	// +kubebuilder:default=300
	UpdateFrequencyMin int32 `json:"updateFrequencyMin,omitempty"`

	// UpdateFrequencyMax maximum time between updates (seconds)
	// +kubebuilder:default=1800
	UpdateFrequencyMax int32 `json:"updateFrequencyMax,omitempty"`

	// DeleteRecreateChance probability of delete+recreate vs update (0.0-1.0)
	// +kubebuilder:default="0.1"
	// +kubebuilder:validation:Pattern=`^(0(\.[0-9]+)?|1(\.0+)?)$`
	DeleteRecreateChance string `json:"deleteRecreateChance,omitempty"`
}

// EventsConfig controls Event resource generation
type EventsConfig struct {
	// Enabled controls whether events are generated
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// EventsPerNodePerHour controls event generation rate
	// +kubebuilder:default=50
	EventsPerNodePerHour int32 `json:"eventsPerNodePerHour,omitempty"`

	// EventTypes defines types of events to generate
	EventTypes []EventTypeConfig `json:"eventTypes,omitempty"`
}

// EventTypeConfig defines configuration for specific event types
type EventTypeConfig struct {
	// Type of the event (e.g., "Normal", "Warning")
	Type string `json:"type"`

	// Reason for the event
	Reason string `json:"reason"`

	// Message template for the event
	Message string `json:"message"`

	// Weight for random selection (higher = more frequent)
	Weight int32 `json:"weight"`
}

// CleanupConfig controls cleanup behavior
type CleanupConfig struct {
	// Enabled controls whether cleanup is performed
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// GracefulDeletes uses graceful deletion for resources
	// +kubebuilder:default=true
	GracefulDeletes bool `json:"gracefulDeletes,omitempty"`

	// CleanupDelaySeconds delay before starting cleanup after node removal
	// +kubebuilder:default=60
	CleanupDelaySeconds int32 `json:"cleanupDelaySeconds,omitempty"`

	// OrphanCleanup removes resources for nodes that no longer exist
	// +kubebuilder:default=true
	OrphanCleanup bool `json:"orphanCleanup,omitempty"`
}

// ScaleLoadConfigStatus defines the observed state of ScaleLoadConfig
type ScaleLoadConfigStatus struct {
	// ObservedGeneration reflects the generation of the most recently observed spec
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// KwokNodeCount is the current count of KWOK nodes being managed
	KwokNodeCount int32 `json:"kwokNodeCount"`

	// GeneratedNamespaces is the current count of generated namespaces
	GeneratedNamespaces int32 `json:"generatedNamespaces"`

	// TotalResources tracks counts of generated resources by type
	TotalResources ResourceCounts `json:"totalResources"`

	// LastReconcileTime is the timestamp of the last successful reconcile
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`

	// Conditions represent the latest available observations of the load config state
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Metrics contains performance metrics for the load generation
	Metrics LoadGenerationMetrics `json:"metrics,omitempty"`
}

// ResourceCounts tracks counts of different resource types
type ResourceCounts struct {
	// ConfigMaps count
	ConfigMaps int32 `json:"configMaps"`

	// Secrets count
	Secrets int32 `json:"secrets"`

	// Routes count
	Routes int32 `json:"routes"`

	// ImageStreams count
	ImageStreams int32 `json:"imageStreams"`

	// BuildConfigs count
	BuildConfigs int32 `json:"buildConfigs"`

	// Events count (approximate, events may be auto-cleaned by Kubernetes)
	Events int32 `json:"events"`
}

// LoadGenerationMetrics contains performance metrics
type LoadGenerationMetrics struct {
	// APICallsPerMinute current rate of API calls
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	APICallsPerMinute string `json:"apiCallsPerMinute"`

	// AverageReconcileTime in milliseconds
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	AverageReconcileTime string `json:"averageReconcileTimeMs"`

	// ErrorRate percentage of failed operations
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	ErrorRate string `json:"errorRate"`

	// ResourceCreationRate resources created per minute
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	ResourceCreationRate string `json:"resourceCreationRate"`

	// ResourceUpdateRate resources updated per minute
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	ResourceUpdateRate string `json:"resourceUpdateRate"`

	// ResourceDeletionRate resources deleted per minute
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	ResourceDeletionRate string `json:"resourceDeletionRate"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="KWOK Nodes",type="integer",JSONPath=".status.kwokNodeCount"
//+kubebuilder:printcolumn:name="Namespaces",type="integer",JSONPath=".status.generatedNamespaces"
//+kubebuilder:printcolumn:name="Profile",type="string",JSONPath=".spec.loadProfile.profile"
//+kubebuilder:printcolumn:name="Enabled",type="boolean",JSONPath=".spec.enabled"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ScaleLoadConfig is the Schema for the scaleloadconfigs API
type ScaleLoadConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ScaleLoadConfigSpec   `json:"spec,omitempty"`
	Status ScaleLoadConfigStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ScaleLoadConfigList contains a list of ScaleLoadConfig
type ScaleLoadConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ScaleLoadConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ScaleLoadConfig{}, &ScaleLoadConfigList{})
}
