# Sim Operator

## Overview

The sim Operator is a sophisticated OpenShift operator designed to generate realistic cluster load patterns on OpenShift clusters using KWOK (Kubernetes WithOut Kubelet) fake nodes. This operator creates authentic resource churn and API load patterns that accurately simulate production OpenShift cluster behavior for scale testing and performance validation.

## Key Features

### 🎯 **Realistic Load Patterns**
- Based on analysis of 120+ node production OpenShift clusters
- Generates authentic resource density
- Creates realistic resource types: ConfigMaps, Secrets, Routes, ImageStreams, BuildConfigs
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
Resources Per Namespace = 5 (ConfigMap + Secret + Route + ImageStream + BuildConfig)
API Call Rate = 20 calls/minute/node (configurable)
```

## Quick Start

### 1. Deploy the Operator

```bash
# Install CRDs and operator
oc apply -k config/default/

# Verify deployment
oc get pods -n kwok-load-generator-system
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
# Apply production load profile
oc apply -f config/samples/scale_v1_scaleloadconfig_production.yaml

# Monitor load generation
oc get scaleloadconfigs -o wide
```

## Load Profiles

### Production Profile (Recommended)
```yaml
loadProfile:
  profile: "production"
  namespacesPerNode: 0.6     # Based on 120-node cluster analysis
  resourcesPerNamespace: 5   # Realistic OpenShift resource density
  apiCallRate: 20           # Balanced API load
```

### Development Profile
```yaml
loadProfile:
  profile: "development"
  namespacesPerNode: 0.2     # Light load for development
  resourcesPerNamespace: 3   # Reduced complexity
  apiCallRate: 10           # Lower frequency
```

### Extreme Profile
```yaml
loadProfile:
  profile: "extreme"
  namespacesPerNode: 1.0     # Heavy load for stress testing
  resourcesPerNamespace: 10  # High resource density
  apiCallRate: 50           # Aggressive API calls
```

## Configuration Examples

### Custom Resource Pattern
```yaml
apiVersion: scale.openshift.io/v1
kind: ScaleLoadConfig
metadata:
  name: custom-load
spec:
  loadProfile:
    profile: production
    namespacesPerNode: 0.8
  
  resourceChurn:
    configMaps:
      count: 2
      updateFrequencyMin: 300
      deleteRecreateChance: 0.15
    
    secrets:
      count: 1
      updateFrequencyMin: 600
      deleteRecreateChance: 0.05
```

### Annotation Churn Configuration
```yaml
annotationChurn:
  enabled: true
  networkingAnnotations: true      # OVN/networking patterns
  machineConfigAnnotations: true   # Machine config patterns
  updateIntervalMin: 60           # 1-5 minute intervals
  updateIntervalMax: 300
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
