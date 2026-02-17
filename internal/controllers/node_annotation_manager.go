package controllers

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	scalev1 "github.com/jtaleric/sim-operator/api/v1"
)

// updateNodeAnnotations simulates realistic node annotation churn patterns
// based on observed patterns in OpenShift clusters from must-gather analysis
func (r *ScaleLoadConfigReconciler) updateNodeAnnotations(ctx context.Context,
	config *scalev1.ScaleLoadConfig, kwokNodes []corev1.Node) error {

	log := r.Log.WithName("node-annotation-manager")

	// Check if enough time has passed since last update
	minInterval := time.Duration(config.Spec.AnnotationChurn.UpdateIntervalMin) * time.Second
	maxInterval := time.Duration(config.Spec.AnnotationChurn.UpdateIntervalMax) * time.Second

	for _, node := range kwokNodes {
		// Determine if this node should be updated based on timing
		shouldUpdate := r.shouldUpdateNodeAnnotations(node, minInterval, maxInterval)
		if !shouldUpdate {
			continue
		}

		nodeToUpdate := node.DeepCopy()
		updated := false

		// Apply networking annotation churn (simulates OVN/networking controllers)
		if config.Spec.AnnotationChurn.NetworkingAnnotations {
			if r.updateNetworkingAnnotations(nodeToUpdate) {
				updated = true
			}
		}

		// Apply machine config annotation churn (simulates machine-config-daemon)
		if config.Spec.AnnotationChurn.MachineConfigAnnotations {
			if r.updateMachineConfigAnnotations(nodeToUpdate) {
				updated = true
			}
		}

		// Apply general cluster annotations
		if r.updateClusterAnnotations(nodeToUpdate) {
			updated = true
		}

		if updated {
			if err := r.updateNodeWithRetry(ctx, node.Name, nodeToUpdate); err != nil {
				log.Error(err, "Failed to update node annotations", "node", node.Name)
				continue
			}
			log.V(1).Info("Updated node annotations", "node", node.Name)
		}
	}

	return nil
}

// shouldUpdateNodeAnnotations determines if a node's annotations should be updated
func (r *ScaleLoadConfigReconciler) shouldUpdateNodeAnnotations(node corev1.Node,
	minInterval, maxInterval time.Duration) bool {

	// Check for last update annotation
	lastUpdateStr, exists := node.Annotations["scale.openshift.io/last-annotation-update"]
	if !exists {
		return true // First update
	}

	lastUpdate, err := time.Parse(time.RFC3339, lastUpdateStr)
	if err != nil {
		return true // Invalid timestamp, update
	}

	// Random interval between min and max
	randomInterval := minInterval + time.Duration(rand.Int63n(int64(maxInterval-minInterval)))

	return time.Since(lastUpdate) >= randomInterval
}

// updateNetworkingAnnotations simulates OVN/networking annotation updates
// Based on patterns observed in must-gather analysis
func (r *ScaleLoadConfigReconciler) updateNetworkingAnnotations(node *corev1.Node) bool {
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	updated := false
	now := time.Now()

	// Simulate OVN annotations (based on must-gather patterns)
	ovnAnnotations := map[string]func() string{
		"k8s.ovn.org/host-cidrs": func() string {
			return fmt.Sprintf("[\"%s/19\"]", generateRandomIP())
		},
		"k8s.ovn.org/l3-gateway-config": func() string {
			return generateL3GatewayConfig(node.Name)
		},
		"k8s.ovn.org/node-chassis-id": func() string {
			return generateRandomUUID()
		},
		"k8s.ovn.org/node-encap-ips": func() string {
			return fmt.Sprintf("[\"%s\"]", generateRandomIP())
		},
		"k8s.ovn.org/node-primary-ifaddr": func() string {
			return fmt.Sprintf("{\"ipv4\":\"%s/19\"}", generateRandomIP())
		},
		"k8s.ovn.org/node-subnets": func() string {
			return fmt.Sprintf("{\"default\":[\"%s/23\"]}", generateRandomSubnet())
		},
		"k8s.ovn.org/node-transit-switch-port-ifaddr": func() string {
			return fmt.Sprintf("{\"ipv4\":\"%s/16\"}", generateTransitIP())
		},
		"k8s.ovn.org/zone-name": func() string {
			return node.Name
		},
		"k8s.ovn.org/remote-zone-migrated": func() string {
			return node.Name
		},
		"k8s.ovn.org/layer2-topology-version": func() string {
			versions := []string{"2.0", "2.1", "2.2"}
			return versions[rand.Intn(len(versions))]
		},
	}

	// Update 30% of networking annotations each time
	for annotation, generator := range ovnAnnotations {
		if rand.Float64() < 0.3 {
			node.Annotations[annotation] = generator()
			updated = true
		}
	}

	// Cloud network annotations
	if rand.Float64() < 0.2 { // Update less frequently
		node.Annotations["cloud.network.openshift.io/egress-ipconfig"] = generateEgressIPConfig()
		updated = true
	}

	if updated {
		node.Annotations["scale.openshift.io/last-annotation-update"] = now.Format(time.RFC3339)
		node.Annotations["scale.openshift.io/networking-churn-iteration"] = fmt.Sprintf("%d", rand.Intn(10000))
	}

	return updated
}

// updateMachineConfigAnnotations simulates machine-config-daemon annotation updates
func (r *ScaleLoadConfigReconciler) updateMachineConfigAnnotations(node *corev1.Node) bool {
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	updated := false
	now := time.Now()

	// Simulate machine config annotations (based on must-gather patterns)
	machineConfigAnnotations := map[string]func() string{
		"machineconfiguration.openshift.io/currentConfig": func() string {
			return fmt.Sprintf("rendered-worker-%s", generateRandomHash())
		},
		"machineconfiguration.openshift.io/desiredConfig": func() string {
			return fmt.Sprintf("rendered-worker-%s", generateRandomHash())
		},
		"machineconfiguration.openshift.io/desiredDrain": func() string {
			return fmt.Sprintf("uncordon-rendered-worker-%s", generateRandomHash())
		},
		"machineconfiguration.openshift.io/lastAppliedDrain": func() string {
			return fmt.Sprintf("uncordon-rendered-worker-%s", generateRandomHash())
		},
		"machineconfiguration.openshift.io/state": func() string {
			states := []string{"Done", "Working", "Degraded"}
			return states[rand.Intn(len(states))]
		},
		"machineconfiguration.openshift.io/reason": func() string {
			if rand.Float64() < 0.8 {
				return "" // Usually empty
			}
			reasons := []string{"Updating", "Rebooting", "ConfigChange"}
			return reasons[rand.Intn(len(reasons))]
		},
		"machineconfiguration.openshift.io/lastSyncedControllerConfigResourceVersion": func() string {
			return fmt.Sprintf("%d", 2900000+rand.Intn(100000))
		},
		"machineconfiguration.openshift.io/controlPlaneTopology": func() string {
			return "HighlyAvailable"
		},
		"machineconfiguration.openshift.io/lastObservedServerCAAnnotation": func() string {
			return "false"
		},
		"machineconfiguration.openshift.io/post-config-action": func() string {
			return ""
		},
	}

	// Update 40% of machine config annotations each time
	for annotation, generator := range machineConfigAnnotations {
		if rand.Float64() < 0.4 {
			node.Annotations[annotation] = generator()
			updated = true
		}
	}

	if updated {
		node.Annotations["scale.openshift.io/last-annotation-update"] = now.Format(time.RFC3339)
		node.Annotations["scale.openshift.io/machine-config-churn-iteration"] = fmt.Sprintf("%d", rand.Intn(10000))
	}

	return updated
}

// updateClusterAnnotations simulates other cluster-level annotation updates
func (r *ScaleLoadConfigReconciler) updateClusterAnnotations(node *corev1.Node) bool {
	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	updated := false
	now := time.Now()

	// CSI and volume annotations
	if rand.Float64() < 0.1 { // Update less frequently
		node.Annotations["csi.volume.kubernetes.io/nodeid"] = generateCSINodeID()
		updated = true
	}

	// Machine API annotations
	if rand.Float64() < 0.05 { // Update rarely
		node.Annotations["machine.openshift.io/machine"] = generateMachineReference(node.Name)
		updated = true
	}

	// Custom load generator tracking
	node.Annotations["scale.openshift.io/load-generator-managed"] = "true"
	node.Annotations["scale.openshift.io/last-seen"] = now.Format(time.RFC3339)
	updated = true

	return updated
}

// Helper functions for generating realistic annotation values

func generateRandomIP() string {
	return fmt.Sprintf("10.0.%d.%d", rand.Intn(256), rand.Intn(256))
}

func generateRandomSubnet() string {
	return fmt.Sprintf("10.%d.%d.0", 128+rand.Intn(128), rand.Intn(256)&0xFE)
}

func generateTransitIP() string {
	return fmt.Sprintf("100.88.0.%d", rand.Intn(256))
}

func generateRandomHash() string {
	chars := "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, 32)
	for i := range result {
		result[i] = chars[rand.Intn(len(chars))]
	}
	return string(result)
}

func generateRandomUUID() string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		rand.Uint32(),
		rand.Uint32()&0xFFFF,
		rand.Uint32()&0xFFFF,
		rand.Uint32()&0xFFFF,
		rand.Uint64()&0xFFFFFFFFFFFF)
}

func generateL3GatewayConfig(nodeName string) string {
	ip := generateRandomIP()
	mac := fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		rand.Intn(256), rand.Intn(256), rand.Intn(256),
		rand.Intn(256), rand.Intn(256), rand.Intn(256))

	return fmt.Sprintf(`{"default":{"mode":"shared","bridge-id":"br-ex","interface-id":"br-ex_%s","mac-address":"%s","ip-addresses":["%s/19"],"ip-address":"%s/19","next-hops":["10.0.0.1"],"next-hop":"10.0.0.1","node-port-enable":"true","vlan-id":"0"}}`,
		nodeName, mac, ip, ip)
}

func generateEgressIPConfig() string {
	ip := generateRandomIP()
	eni := fmt.Sprintf("eni-%012x", rand.Uint64()&0xFFFFFFFFFFFF)

	return fmt.Sprintf(`[{"interface":"%s","ifaddr":{"ipv4":"%s/19"},"capacity":{"ipv4":%d,"ipv6":%d}}]`,
		eni, ip, 10+rand.Intn(20), 10+rand.Intn(20))
}

func generateCSINodeID() string {
	return fmt.Sprintf(`{"ebs.csi.aws.com":"i-%016x"}`, rand.Uint64())
}

func generateMachineReference(nodeName string) string {
	// Extract some identifier from node name for consistency
	suffix := nodeName[len(nodeName)-6:]
	return fmt.Sprintf("openshift-machine-api/ci-op-%s-worker-us-west-2a-%s",
		generateRandomString(6), suffix)
}

// updateNodeWithRetry implements retry logic with exponential backoff for node updates
func (r *ScaleLoadConfigReconciler) updateNodeWithRetry(ctx context.Context, nodeName string, nodeUpdate *corev1.Node) error {
	backoff := wait.Backoff{
		Steps:    5,
		Duration: 100 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
	}

	return wait.ExponentialBackoff(backoff, func() (bool, error) {
		// Get the latest version of the node
		var currentNode corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, &currentNode); err != nil {
			return false, err
		}

		// Apply the annotation updates to the current version
		if currentNode.Annotations == nil {
			currentNode.Annotations = make(map[string]string)
		}
		for k, v := range nodeUpdate.Annotations {
			currentNode.Annotations[k] = v
		}

		// Attempt the update
		if err := r.Update(ctx, &currentNode); err != nil {
			// If it's a conflict error, retry
			if errors.IsConflict(err) {
				return false, nil // Retry
			}
			return false, err // Other errors are not retryable
		}

		return true, nil // Success
	})
}
