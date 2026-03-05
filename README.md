# Sim Operator

## Overview

The sim Operator is a sophisticated OpenShift operator designed to generate realistic cluster load patterns on OpenShift clusters using KWOK (Kubernetes WithOut Kubelet) fake nodes. This operator creates authentic resource churn and API load patterns that accurately simulate production OpenShift cluster behavior for scale testing and performance validation.

## Key Features

### 🎯 **Realistic Load Patterns**
- Based on analysis of 120+ node production OpenShift clusters
- Generates authentic resource density
- Creates realistic resource types: ConfigMaps, Secrets, Routes, ImageStreams, BuildConfigs, Pods, Events
- Implements production-like API call patterns and frequencies

### 🌐 **Node Annotation Simulation**
- Simulates OVN/networking annotation updates (k8s.ovn.org/*)
- Machine config annotation churn (machineconfiguration.openshift.io/*)
- Cloud provider annotation patterns
- Configurable update intervals matching real cluster behavior

## Architecture

### Core Components

1. **ScaleLoadConfigReconciler**: Main controller that watches KWOK nodes and orchestrates load generation
2. **ResourceManager**: Handles creation, update, and deletion of OpenShift resources
3. **NodeAnnotationManager**: Simulates realistic node annotation churn patterns
4. **StatusManager**: Tracks metrics and maintains operator status
5. **MetricsCollector**: Exposes Prometheus metrics for observability

### Resource Scaling Formula

Based on must-gather analysis of production clusters:

```
Target Namespaces = ceil(KWOK_Node_Count × Namespaces_Per_Node)
Resources Per Namespace = 3-5 per resource type (configurable, default 3 per type)
API Call Rate = 20 calls/minute/node (configurable via apiCallRatePerNode)
```

## Maximum Field Quick Reference

All resource types support the `maximum` field for cluster-wide resource limits:

| Resource Type | Field Location | Default | Description |
|---------------|----------------|---------|-------------|
| **Namespaces** | `resourceChurn.namespaces.maximum` | `0` | Total namespaces across cluster |
| **Pods** | `resourceChurn.pods.maximum` | `0` | Total pods across all namespaces |
| **ConfigMaps** | `resourceChurn.configMaps.maximum` | `0` | Total configMaps across all namespaces |
| **Secrets** | `resourceChurn.secrets.maximum` | `0` | Total secrets across all namespaces |
| **Routes** | `resourceChurn.routes.maximum` | `0` | Total routes across all namespaces |
| **ImageStreams** | `resourceChurn.imageStreams.maximum` | `0` | Total imageStreams across all namespaces |
| **BuildConfigs** | `resourceChurn.buildConfigs.maximum` | `0` | Total buildConfigs across all namespaces |

**Key Rules:**
- `maximum: 0` = No limit (default behavior)
- `maximum: N` = Stop creating when N total resources exist
- Resources continue churning within limits
- Limits checked before each resource creation

## Quick Start

### 1. Deploy the Operator

```bash
# Install CRDs and operator
oc apply -k config/default/

# Verify deployment
oc get pods -n sim-operator-system
```

### 2. Deploy KWOK Nodes

```bash
# Example KWOK nodes with proper labeling
kubectl apply -f - <<EOF
apiVersion: v1
kind: Node
metadata:
  name: kwok-node-1
  labels:
    type: kwok
    kubernetes.io/hostname: kwok-node-1
spec:
  providerID: kwok://kwok-node-1
status:
  conditions:
  - type: Ready
    status: "True"
  allocatable:
    cpu: 8
    memory: 32Gi
    pods: 250
EOF
```

### 3. Create Load Configuration

```bash
# Apply sample configuration
oc apply -f config/samples/scale_v1_scaleloadconfig.yaml

# Monitor load generation
oc get scaleloadconfigs -o wide
```

## Configuration Examples

### Recommended Configuration
```yaml
loadProfile:
  namespacesPerNode: "0.6"     # Based on 120-node cluster analysis (realistic)
  apiCallRatePerNode: 50       # Balanced API load with auto-optimized reconciling
```

### Light Load Configuration
```yaml
loadProfile:
  namespacesPerNode: "0.2"     # Light namespace density 
  apiCallRatePerNode: 20       # Conservative API rate
```

### Heavy Load Configuration  
```yaml
loadProfile:
  namespacesPerNode: "1.0"     # High namespace density
  apiCallRatePerNode: 100      # Aggressive API rate for stress testing
```

## API Rate Limiting

The operator implements per-minute API rate limiting to prevent overwhelming the Kubernetes API server during load generation.

### Rate Limit Calculation

The operator estimates API calls needed for each reconcile cycle:

```
Estimated API Calls = Target Namespaces × 5 calls per namespace
Target Namespaces = KWOK Nodes × namespacesPerNode
```

**Example with 500 KWOK nodes:**
- Target Namespaces: `500 × 0.6 = 300`
- Estimated API Calls: `300 × 5 = 1500 calls`

### Rate Limit Configuration

Configure API rate limits using these fields (in priority order):

```yaml
loadProfile:
  # Option 1: Static rate (recommended) - fixed calls per minute
  apiCallRateStatic: 2000
  
  # Option 2: Per-node rate - scales with KWOK node count  
  apiCallRatePerNode: 20
  
```

### Rate Limiting Behavior

- **Counter Reset**: API call counter resets every 60 seconds
- **Pre-flight Check**: Operator checks estimated calls against limit before starting operations
- **Backoff**: When rate limit reached, operator skips the cycle and waits 1 minute
- **Logging**: Rate limit events logged at DEBUG level with current counter and estimated calls

**Example rate limit log:**
```
Rate limit reached, skipping resource management this cycle
{"estimatedCalls": 1500, "currentCounter": 916}
```

This indicates 916 API calls were already made this minute, and the requested 1500 additional calls would exceed the configured limit.

## Comprehensive Configuration Guide

### ScaleLoadConfig Reference

The `ScaleLoadConfig` custom resource provides extensive configuration options for simulating realistic cluster load patterns.

#### Basic Configuration

```yaml
apiVersion: scale.openshift.io/v1
kind: ScaleLoadConfig
metadata:
  name: example-load
spec:
  # Enable/disable the entire load generation
  enabled: true
  
  # Target specific nodes (typically KWOK nodes)
  kwokNodeSelector:
    type: "kwok"
    # Additional selectors can be added
    environment: "test"
```

#### Load Configuration

Controls the overall scale and behavior of load generation:

```yaml
loadProfile:
  # Namespace density - how many namespaces per node
  namespacesPerNode: "0.5"  # String to support decimals (0.5 = 1 namespace per 2 nodes)
  
  # API rate limiting (REQUIRED - choose one method)
  # This controls reconcile frequency, concurrency, and overall load intensity
  apiCallRatePerNode: 50    # API calls per minute per node (scales with node count)
  # OR
  # apiCallRateStatic: 2000 # Fixed API calls per minute (doesn't scale)
```

**Configuration Guidelines:**
- **namespacesPerNode**: Based on must-gather analysis showing ~0.6 namespaces per node
  - Light load: 0.2-0.3 namespaces per node
  - Medium load: 0.4-0.5 namespaces per node  
  - Realistic load: 0.6 namespaces per node (recommended)
  - Heavy load: 0.8-1.0+ namespaces per node

**API Rate Controls Everything Else:**
- **Reconcile Frequency**: Auto-calculated based on API capacity (typically 5-120 seconds)
- **Concurrency**: Auto-calculated to prevent API server overload
- **Load Intensity**: Higher API rates = more aggressive resource churn

#### Namespace Configuration

Controls how generated namespaces are configured:

```yaml
namespaceConfig:
  namespacePrefix: "app-workload-"  # Prefix for generated namespace names
  
  # Labels applied to all generated namespaces
  labels:
    scale.openshift.io/load-test: "true"
    environment: "production"
    workload-type: "application"
    team: "platform"
  
  # Annotations applied to all generated namespaces
  annotations:
    scale.openshift.io/description: "Simulated application workload namespace"
    scale.openshift.io/created-by: "sim-operator"
  
  # Resource quotas (optional) - applied to each namespace
  resourceQuota:
    cpu: "2000m"      # 2 CPU cores
    memory: "4Gi"     # 4GB memory
    storage: "10Gi"   # 10GB storage
    pods: 100         # Maximum 100 pods per namespace
```

#### Resource Churn Configuration

The heart of the simulator - controls what resources are created and how they change over time.

##### Pod Churn (Application Lifecycle)
```yaml
resourceChurn:
  pods:
    enabled: true
    count: 50                    # Pods per namespace
    
    # Update frequency controls how often pods are modified
    updateFrequencyMin: 120      # Minimum 2 minutes between updates
    updateFrequencyMax: 300      # Maximum 5 minutes between updates
    
    # Churn behavior
    deleteRecreateChance: "0.5"  # 50% chance to delete+recreate vs update
    
    # Scheduling configuration
    tolerateKwokTaint: true      # Allow scheduling on KWOK nodes
    nodeAffinityStrategy: "round-robin"  # Pod distribution strategy
    # Options: "round-robin", "random", "single-node", "zone-balanced"
    
    # Performance tuning
    namespaceInterval: 1         # Update pods in every Nth namespace per reconcile
                                # 1=every namespace, 2=every other namespace, etc.
    
    # Resource limits
    maximum: 0                   # Maximum total pods across all namespaces
                                # 0 = no limit, >0 = stop creating when limit reached
                                # Resources continue to churn, just no new creation beyond limit
```

**Pod Churn Behavior:**
- `namespaceInterval: 1` - Most aggressive, updates all namespaces every reconcile
- `namespaceInterval: 3` - Updates 1/3 of namespaces per reconcile, cycles through all
- `deleteRecreateChance: "0.8"` - 80% of changes are pod deletions+recreations (deployment simulation)
- `deleteRecreateChance: "0.2"` - 80% of changes are updates (rolling update simulation)

##### Namespace Churn (Tenant Lifecycle)
```yaml
resourceChurn:
  namespaces:
    enabled: true
    churnPercentage: 5           # Percentage of namespaces to churn per cycle
    churnIntervalSeconds: 300    # How often to perform namespace churn (5 minutes)
    preserveOldestNamespaces: 10 # Always keep the N oldest namespaces (stability)
    maximum: 50                  # Maximum total namespaces across cluster (0 = no limit)
```

**Namespace Churn Example:**
With 100 namespaces and `churnPercentage: 5`:
- Every 5 minutes, 5 namespaces (5%) are deleted
- 5 new namespaces are created to replace them
- The 10 oldest namespaces are never deleted (preserved for stability)
- Simulates tenant onboarding/offboarding in multi-tenant clusters

**Namespace Maximum Limit:**
```yaml
namespaces:
  enabled: true
  maximum: 50                    # Stop creating namespaces when 50 total exist
  churnPercentage: 5            # Continue churning existing namespaces
  # With 100 KWOK nodes requesting 60 namespaces:
  # Only 50 namespaces will be created due to maximum limit
  # Namespace churn will still occur within those 50 namespaces
```

##### ConfigMap Churn (Configuration Management)
```yaml
resourceChurn:
  configMaps:
    enabled: true
    count: 7                     # ConfigMaps per namespace
    
    # Update timing
    updateFrequencyMin: 900      # 15 minutes minimum
    updateFrequencyMax: 3600     # 1 hour maximum
    
    # Churn behavior
    deleteRecreateChance: "0.05" # 5% chance to recreate vs update
    
    # Performance tuning
    namespaceInterval: 1         # Update ConfigMaps in every Nth namespace per reconcile
    maximum: 100                 # Maximum total configMaps across all namespaces (0 = no limit)
```

##### Secret Churn (Credential Rotation)
```yaml
resourceChurn:
  secrets:
    enabled: true
    count: 7                     # Secrets per namespace
    updateFrequencyMin: 1800     # 30 minutes (cert rotations, credential updates)
    updateFrequencyMax: 7200     # 2 hours maximum
    deleteRecreateChance: "0.02" # 2% recreation rate (rare secret recreation)
    namespaceInterval: 1
    maximum: 50                  # Maximum total secrets across all namespaces (0 = no limit)
```

##### Route Churn (Ingress Configuration)
```yaml
resourceChurn:
  routes:
    enabled: true
    count: 3                     # Default count per namespace
    updateFrequencyMin: 120      # 2 minutes minimum
    updateFrequencyMax: 600      # 10 minutes maximum
    deleteRecreateChance: "0.1"  # 10% recreation rate
    namespaceInterval: 1         # Update routes in every namespace
    maximum: 0                   # No cluster-wide limit (0 = unlimited)
```

##### ImageStream Churn (Container Image Management)
```yaml
resourceChurn:
  imageStreams:
    enabled: true
    count: 3                     # Default count per namespace
    updateFrequencyMin: 120      # 2 minutes minimum
    updateFrequencyMax: 600      # 10 minutes maximum
    deleteRecreateChance: "0.1"  # 10% recreation rate
    namespaceInterval: 1         # Update imageStreams in every namespace
    maximum: 0                   # No cluster-wide limit (0 = unlimited)
```

##### BuildConfig Churn (CI/CD Pipeline Configuration)
```yaml
resourceChurn:
  buildConfigs:
    enabled: true
    count: 3                     # Default count per namespace
    updateFrequencyMin: 120      # 2 minutes minimum
    updateFrequencyMax: 600      # 10 minutes maximum
    deleteRecreateChance: "0.1"  # 10% recreation rate
    namespaceInterval: 1         # Update buildConfigs in every namespace
    maximum: 0                   # No cluster-wide limit (0 = unlimited)
```

> **ℹ️ Note**: All resource types now use consistent defaults: enabled=true, count=3, updateFrequency 120-600 seconds, and support cluster-wide maximum limits.

##### Event Generation (Cluster Activity Simulation)
```yaml
resourceChurn:
  events:
    enabled: true
    eventsPerNodePerHour: 50     # Events generated per KWOK node per hour
    
    # Event type distribution (weights determine probability)
    eventTypes:
    # Normal operational events (80% of events)
    - type: "Normal"
      reason: "Scheduled"
      message: "Successfully assigned pod %s to node"
      weight: 25                 # 25/100 = 25% of all events
    
    - type: "Normal"
      reason: "Started"
      message: "Started container %s"
      weight: 20                 # 20% of events
    
    - type: "Normal"
      reason: "Pulled"
      message: "Container image pulled successfully"
      weight: 18
    
    - type: "Normal"
      reason: "Created"
      message: "Created container %s"
      weight: 15
    
    # Scaling events
    - type: "Normal"
      reason: "ScalingReplicaSet"
      message: "Scaled up replica set %s to %d"
      weight: 8
    
    - type: "Normal"
      reason: "SuccessfulCreate"
      message: "Created pod %s"
      weight: 6
    
    # Warning events (20% of events - realistic failure rates)
    - type: "Warning"
      reason: "BackOff"
      message: "Back-off restarting failed container"
      weight: 4                  # 4% of events
    
    - type: "Warning"
      reason: "FailedMount"
      message: "Unable to attach or mount volumes"
      weight: 2
    
    - type: "Warning"
      reason: "FailedScheduling"
      message: "pod has unbound immediate PersistentVolumeClaims"
      weight: 2
```

#### Node Annotation Churn

Simulates realistic infrastructure automation patterns:

```yaml
annotationChurn:
  enabled: true
  
  # OpenShift networking annotations (OVN-Kubernetes patterns)
  networkingAnnotations: true   # Updates k8s.ovn.org/* annotations
  
  # Machine configuration annotations (node lifecycle management)
  machineConfigAnnotations: true # Updates machineconfiguration.openshift.io/* annotations
  
  # Timing controls
  updateIntervalMin: 600        # 10 minutes minimum between updates
  updateIntervalMax: 1800       # 30 minutes maximum between updates
```

**Annotation Patterns Simulated:**
- **Networking**: IP allocations, network policy updates, OVN subnet assignments
- **Machine Config**: Node configuration updates, OS updates, kubelet config changes
- **Cloud Provider**: Instance metadata, zone assignments, storage attachments

#### Maximum Resource Limits

The `maximum` field provides fine-grained control over resource creation while maintaining churn behavior across **all resource types** including namespaces, pods, configMaps, secrets, routes, imageStreams, buildConfigs, and events.

```yaml
resourceChurn:
  # Namespace limits - controls total namespaces across the cluster
  namespaces:
    enabled: true
    maximum: 100                # Stop creating namespaces when 100 total exist
    churnPercentage: 5          # Continue churning within existing namespaces
  
  # Pod limits - controls total pods across all namespaces
  pods:
    enabled: true
    count: 50                   # Pods per namespace
    maximum: 1000               # Stop creating pods when 1000 total exist
  
  # ConfigMap limits - controls total configMaps across all namespaces
  configMaps:
    enabled: true
    count: 10                   # ConfigMaps per namespace
    maximum: 200                # Stop creating configMaps when 200 total exist
  
  # Secret limits - controls total secrets across all namespaces
  secrets:
    enabled: true
    count: 5                    # Secrets per namespace
    maximum: 150                # Stop creating secrets when 150 total exist
  
  # Route limits - controls total routes across all namespaces
  routes:
    enabled: true
    count: 1                    # Routes per namespace
    maximum: 10                 # Stop creating routes when 10 total exist
  
  # ImageStream limits - controls total imageStreams across all namespaces
  imageStreams:
    enabled: true
    count: 2                    # ImageStreams per namespace
    maximum: 50                 # Stop creating imageStreams when 50 total exist
  
  # BuildConfig limits - controls total buildConfigs across all namespaces
  buildConfigs:
    enabled: true
    count: 1                    # BuildConfigs per namespace
    maximum: 25                 # Stop creating buildConfigs when 25 total exist
```

**How Maximum Limits Work:**

1. **Cross-Namespace Counting**: The operator counts existing resources of each type across **all managed namespaces**
2. **Pre-Creation Check**: Before creating new resources, the total count is checked against the maximum limit
3. **Dynamic Adjustment**: If the requested count would exceed the maximum, it's reduced to stay within limits
4. **Continued Churn**: Existing resources continue to be updated, deleted, and recreated according to their churn patterns
5. **No New Creation**: No new resources are created beyond the maximum limit
6. **Namespace-Specific**: For namespaces, the maximum applies to the total namespace count across the entire cluster

**Maximum Field Values:**
- `maximum: 0` - **No limit** (default behavior, create resources in all applicable namespaces)
- `maximum: 50` - **Hard limit** of 50 total resources of this type across all namespaces
- Applies to: namespaces, pods, configMaps, secrets, routes, imageStreams, buildConfigs

**Resource Priority Logic:**
When maximum limits are reached, the operator follows this priority:
1. **First-come, first-served**: Resources are created in namespaces in order of processing
2. **Early namespaces**: Lower-indexed namespaces are more likely to receive resources
3. **Churn continues**: All existing resources continue normal churn behavior regardless of limits

**Example Scenarios:**

```yaml
# Scenario 1: Conservative pod testing
pods:
  enabled: true
  count: 20                     # 20 pods per namespace
  maximum: 100                  # But never more than 100 pods total
  # With 250 namespaces: Only first 5 namespaces get pods (5 × 20 = 100)
  # Remaining 245 namespaces get 0 pods due to maximum limit

# Scenario 2: Limited route testing  
routes:
  enabled: true
  count: 2                      # 2 routes per namespace
  maximum: 10                   # But never more than 10 routes total
  # With 100 namespaces: Only first 5 namespaces get routes (5 × 2 = 10)
  # Routes in those 5 namespaces continue to churn (update/recreate)

# Scenario 3: No limits (default behavior)
configMaps:
  enabled: true
  count: 5
  maximum: 0                    # 0 = no limit, create in all namespaces

# Scenario 4: Namespace limits for controlled multi-tenant testing
namespaces:
  enabled: true
  maximum: 25                   # Limit total namespaces across cluster
  churnPercentage: 10          # 10% namespace turnover per cycle
  # With 100 KWOK nodes normally creating 60 namespaces:
  # Only 25 namespaces will be created due to maximum
  # Active churn will occur within those 25 namespaces
```

**Benefits of Maximum Limits:**
- **Controlled Testing**: Test resource behavior without overwhelming the cluster
- **Graduated Load**: Start with small maximums and increase gradually  
- **Resource Protection**: Prevent runaway resource creation
- **Realistic Patterns**: Some resources naturally have cluster-wide limits
- **Churn Focus**: Test resource update/delete patterns without excessive creation
- **Multi-Resource Testing**: Set different limits for different resource types to simulate real constraints
- **Cluster Stability**: Prevent resource exhaustion during large-scale testing

#### Maximum Field Best Practices

**Graduated Testing Approach:**
```yaml
# Phase 1: Initial testing with very low limits
namespaces:
  maximum: 10
pods:
  maximum: 50
configMaps:
  maximum: 20

# Phase 2: Moderate testing after validation
namespaces:
  maximum: 50  
pods:
  maximum: 500
configMaps:
  maximum: 200

# Phase 3: Production-scale testing
namespaces:
  maximum: 0     # Remove limits after proving stability
pods:
  maximum: 0
configMaps:
  maximum: 0
```

**Resource Type Recommendations:**
- **Namespaces**: Start with `maximum: 25-50` for controlled multi-tenant testing
- **Pods**: Use `maximum: 100-1000` for container orchestration testing  
- **ConfigMaps**: Set `maximum: 50-500` for configuration management testing
- **Secrets**: Keep `maximum: 25-200` for security testing (secrets are sensitive)
- **Routes**: Very conservative `maximum: 5-25` (routes can impact cluster networking)
- **ImageStreams/BuildConfigs**: `maximum: 10-50` (these can cause cluster instability)

**Monitoring Maximum Limits:**
Check the operator logs for these messages:
- `"Resource creation limited by maximum"` - Hit the limit, no new resources created
- `"Target reduced by maximum limit"` - Requested count was reduced to fit within limit
- `"Final effective count"` - Shows actual resources created vs requested

#### Cleanup Configuration

Controls how resources are removed when the operator is disabled:

```yaml
cleanupConfig:
  enabled: true                 # Enable automatic cleanup
  gracefulDeletes: true         # Use graceful deletion (honors termination grace periods)
  cleanupDelaySeconds: 30       # Wait time before starting cleanup
  orphanCleanup: true          # Clean up resources even if operator is deleted
```

#### Performance Tuning Guidelines

##### Small Clusters (< 50 nodes)
```yaml
loadProfile:
  namespacesPerNode: "0.2"      # 1 namespace per 5 nodes
  apiCallRatePerNode: 10        # Conservative API rate

resourceChurn:
  pods:
    namespaceInterval: 3        # Update 1/3 of namespaces per cycle
    updateFrequencyMin: 300     # 5-minute minimum intervals
  
  configMaps:
    namespaceInterval: 2
  
  secrets:
    namespaceInterval: 5        # Very conservative
```

##### Medium Clusters (50-200 nodes)
```yaml
loadProfile:
  namespacesPerNode: "0.5"      # 1 namespace per 2 nodes
  apiCallRatePerNode: 25

resourceChurn:
  pods:
    namespaceInterval: 2        # Moderate pod churn
    updateFrequencyMin: 180     # 3-minute intervals
```

##### Large Clusters (200+ nodes)
```yaml
loadProfile:
  namespacesPerNode: "0.8"      # High density
  apiCallRatePerNode: 50

resourceChurn:
  pods:
    namespaceInterval: 1        # Maximum churn
    updateFrequencyMin: 120     # 2-minute intervals
```

### Configuration Examples

#### Maximum Limits Example (Controlled Resource Testing)
```yaml
apiVersion: scale.openshift.io/v1
kind: ScaleLoadConfig
metadata:
  name: controlled-limits
spec:
  enabled: true
  kwokNodeSelector:
    type: "kwok"
  
  loadProfile:
    namespacesPerNode: "0.5"     # Would normally create 50 namespaces for 100 nodes
    apiCallRatePerNode: 30
  
  # Controlled namespace creation
  resourceChurn:
    namespaces:
      enabled: true
      maximum: 25                # Limit to 25 namespaces total (half of normal)
      churnPercentage: 10       # Active churn within those 25 namespaces
      churnIntervalSeconds: 300
      preserveOldestNamespaces: 5
    
    # Conservative pod limits for stable testing
    pods:
      enabled: true
      count: 10                  # 10 pods per namespace requested
      maximum: 100               # But max 100 pods total across all namespaces
      # With 25 namespaces requesting 250 pods total:
      # Only 100 pods will be created (average 4 per namespace)
      # Pod churn continues within those 100 pods
    
    # Moderate ConfigMap limits
    configMaps:
      enabled: true
      count: 5                   # 5 configMaps per namespace requested  
      maximum: 50                # Max 50 configMaps total
      # With 25 namespaces: Only first 10 namespaces get full allocation
    
    # Conservative Secret limits
    secrets:
      enabled: true
      count: 3                   # 3 secrets per namespace requested
      maximum: 30                # Max 30 secrets total
      # Secrets distributed across first 10 namespaces
    
    # Very limited Route testing
    routes:
      enabled: true
      count: 1                   # 1 route per namespace requested
      maximum: 5                 # Only 5 routes total for stability testing
    
    # Disable potentially unstable resources
    imageStreams:
      enabled: false
    buildConfigs:
      enabled: false
    
    # Events continue normally (no maximum needed)
    events:
      enabled: true
      eventsPerNodePerHour: 25   # Reduced event rate
```

#### Aggressive Pod Churn (Deployment-Heavy Environment)
```yaml
apiVersion: scale.openshift.io/v1
kind: ScaleLoadConfig
metadata:
  name: deployment-heavy
spec:
  enabled: true
  kwokNodeSelector:
    type: "kwok"
  
  loadProfile:
    namespacesPerNode: "0.3"
    apiCallRatePerNode: 40
  
  resourceChurn:
    pods:
      enabled: true
      count: 30
      updateFrequencyMin: 60     # Very frequent deployments
      updateFrequencyMax: 180
      deleteRecreateChance: "0.8" # 80% delete+recreate (like deployments)
      namespaceInterval: 1
    
    # Minimal other resources to focus on pod churn
    configMaps:
      enabled: true
      count: 3
      maximum: 50                 # Limit total configMaps for focused testing
      namespaceInterval: 3
    
    secrets:
      enabled: true 
      count: 2
      maximum: 25                 # Limit total secrets for focused testing
      namespaceInterval: 5
    
    # Disabled for stability
    routes:
      enabled: false
      count: 0
    imageStreams:
      enabled: false
      count: 0
    buildConfigs:
      enabled: false
      count: 0
```

#### Multi-Tenant Platform Simulation
```yaml
apiVersion: scale.openshift.io/v1
kind: ScaleLoadConfig
metadata:
  name: multi-tenant
spec:
  enabled: true
  kwokNodeSelector:
    type: "kwok"
  
  loadProfile:
    namespacesPerNode: "1.0"     # High tenant density
    apiCallRatePerNode: 30
  
  namespaceConfig:
    namespacePrefix: "tenant-"
    labels:
      scale.openshift.io/load-test: "true"
      platform: "multi-tenant"
    resourceQuota:
      cpu: "1000m"
      memory: "2Gi"
      pods: 50
  
  resourceChurn:
    # Active namespace churn (tenant onboarding/offboarding)
    namespaces:
      enabled: true
      churnPercentage: 8         # 8% tenant turnover per cycle
      churnIntervalSeconds: 600  # 10-minute cycles
      preserveOldestNamespaces: 20
      maximum: 100               # Limit to 100 tenant namespaces maximum
    
    # Moderate pod activity
    pods:
      enabled: true
      count: 25
      updateFrequencyMin: 300
      deleteRecreateChance: "0.3"
      namespaceInterval: 2
    
    # Configuration management
    configMaps:
      enabled: true
      count: 5
      maximum: 300                # Allow more configMaps for multi-tenant scenarios
      namespaceInterval: 2
    
    secrets:
      enabled: true
      count: 3
      maximum: 200                # Allow more secrets for multi-tenant scenarios
      namespaceInterval: 3
    
    # Disabled for stability
    routes:
      enabled: false
      count: 0
    imageStreams:
      enabled: false
      count: 0
    buildConfigs:
      enabled: false
      count: 0
```

#### CI/CD Pipeline Simulation
```yaml
apiVersion: scale.openshift.io/v1
kind: ScaleLoadConfig  
metadata:
  name: cicd-pipeline
spec:
  enabled: true
  kwokNodeSelector:
    type: "kwok"
  
  loadProfile:
    namespacesPerNode: "0.4"
    apiCallRatePerNode: 35
  
  resourceChurn:
    # Focus on deployment activity instead of builds (builds cause instability)
    pods:
      enabled: true
      count: 20
      updateFrequencyMin: 180    # Very frequent deployments (3 minutes)
      updateFrequencyMax: 300    # 5 minutes max
      deleteRecreateChance: "0.9" # Mostly recreations (new deployments)
      namespaceInterval: 1
    
    # Configuration updates for deployment pipelines
    configMaps:
      enabled: true
      count: 8                   # CI/CD config, environment configs, scripts
      maximum: 150               # Limit total CI/CD configs
      updateFrequencyMin: 600    # 10-minute intervals (pipeline config changes)
      updateFrequencyMax: 1800   # 30 minutes max
      namespaceInterval: 1
    
    secrets:
      enabled: true
      count: 5                   # CI/CD credentials, registry secrets, etc.
      maximum: 100               # Limit total CI/CD secrets
      updateFrequencyMin: 1200   # 20-minute intervals (credential rotations)
      updateFrequencyMax: 3600   # 1 hour max
      namespaceInterval: 1
    
    # Disabled for stability (builds cause cluster instability)
    buildConfigs:
      enabled: false             # DISABLED - causes cluster instability
      count: 0
    
    imageStreams:
      enabled: false             # DISABLED - causes cluster instability
      count: 0
    
    routes:
      enabled: false             # DISABLED - causes cluster instability
      count: 0
```

## Monitoring and Observability

### Prometheus Metrics

The operator exposes comprehensive metrics:

```
# Node and namespace counts
kwok_load_generator_nodes_total
kwok_load_generator_namespaces_total

# Performance metrics
kwok_load_generator_api_calls_duration_seconds
kwok_load_generator_reconcile_duration_seconds
kwok_load_generator_errors_total
```

### Status Information

```bash
# Check operator status
oc describe scaleloadconfig production-load

# View generated resources
oc get namespaces -l scale.openshift.io/managed-by=production-load

# Monitor resource counts
oc get configmaps,secrets,routes,imagestreams,buildconfigs \
  -n openshift-fake-example-123456 \
  -l scale.openshift.io/managed-by=production-load
```

## Performance Characteristics

### Scaling Behavior

| KWOK Nodes | Namespaces | Resources | API Calls/min |
|------------|------------|-----------|---------------|
| 10         | 6          | 30        | 200           |
| 50         | 30         | 150       | 1,000         |
| 100        | 60         | 300       | 2,000         |
| 500        | 300        | 1,500     | 10,000        |

## License
This project is licensed under the Apache License 2.0 - see the LICENSE file for details.
