package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Condition type we'll set on the Node
const (
	ConditionTypeKubeletTCPReachable corev1.NodeConditionType = "KubeletTCPReachable"

	defaultKubeletPort = 10250
	maxRetries         = 5
	dialTimeout        = 2 * time.Second
)

// pickNodeIP chooses the best IP to use to reach the kubelet.
// Prefers InternalIP, then ExternalIP, then Hostname.
func pickNodeIP(n *corev1.Node) string {
	var internal, external, hostname string
	for _, a := range n.Status.Addresses {
		switch a.Type {
		case corev1.NodeInternalIP:
			if internal == "" {
				internal = a.Address
			}
		case corev1.NodeExternalIP:
			if external == "" {
				external = a.Address
			}
		case corev1.NodeHostName:
			if hostname == "" {
				hostname = a.Address
			}
		}
	}
	if internal != "" {
		return internal
	}
	if external != "" {
		return external
	}
	return hostname
}

// tcpReachable tries to complete a TCP handshake to addr:port up to maxRetries.
// This is effectively "send SYN, expect SYN-ACK" in terms of reachability.
func tcpReachable(addr string, port int, retries int, timeout time.Duration) bool {
	target := fmt.Sprintf("%s:%d", addr, port)
	dialer := net.Dialer{
		Timeout: timeout,
	}

	for i := 0; i < retries; i++ {
		conn, err := dialer.Dial("tcp", target)
		if err == nil {
			_ = conn.Close()
			return true
		}
		// small delay between retries
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// upsertCondition updates or inserts the custom condition on the Node.
func upsertCondition(node *corev1.Node, reachable bool) bool {
	now := metav1.NewTime(time.Now())

	status := corev1.ConditionFalse
	reason := "KubeletTCPDialFailed"
	message := "Failed to TCP connect to kubelet port"
	if reachable {
		status = corev1.ConditionTrue
		reason = "KubeletTCPDialSucceeded"
		message = "Successfully TCP connected to kubelet port"
	}

	newCond := corev1.NodeCondition{
		Type:               ConditionTypeKubeletTCPReachable,
		Status:             status,
		LastHeartbeatTime:  now,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	}

	conds := node.Status.Conditions
	for i := range conds {
		if conds[i].Type == ConditionTypeKubeletTCPReachable {
			// If nothing changed except heartbeat, just update heartbeat/transition appropriately.
			if conds[i].Status == newCond.Status &&
				conds[i].Reason == newCond.Reason &&
				conds[i].Message == newCond.Message {
				conds[i].LastHeartbeatTime = now
				// LastTransitionTime unchanged if status didn't change.
				node.Status.Conditions = conds
				return true
			}

			// Status or meaning changed – update everything and bump transition time.
			newCond.LastTransitionTime = now
			node.Status.Conditions[i] = newCond
			return true
		}
	}

	// Condition not present – append it.
	node.Status.Conditions = append(node.Status.Conditions, newCond)
	return true
}

func buildKubeClient(kubeconfig string) (*kubernetes.Clientset, error) {
	var cfg *rest.Config
	var err error

	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

func main() {
	var (
		kubeconfig = flag.String("kubeconfig", "", "Path to kubeconfig (if empty, use in-cluster config)")
		port       = flag.Int("port", defaultKubeletPort, "Kubelet TCP port to probe")
		retries    = flag.Int("retries", maxRetries, "Number of TCP dial retries")
		timeoutSec = flag.Int("timeout-seconds", int(dialTimeout.Seconds()), "Per-dial timeout in seconds")
	)
	flag.Parse()

	client, err := buildKubeClient(*kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build kube client: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to list nodes: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Found %d nodes\n", len(nodes.Items))

	for _, n := range nodes.Items {
		nodeName := n.Name
		nodeIP := pickNodeIP(&n)
		if nodeIP == "" {
			fmt.Fprintf(os.Stderr, "node %s has no usable IP; skipping\n", nodeName)
			continue
		}

		fmt.Printf("Probing node %s (%s)...\n", nodeName, nodeIP)
		reachable := tcpReachable(nodeIP, *port, *retries, time.Duration(*timeoutSec)*time.Second)
		fmt.Printf("  reachable=%v\n", reachable)

		// Get fresh copy & update status subresource
		latest, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to get node %s: %v\n", nodeName, err)
			continue
		}

		// Work on a deep copy / modify status
		updated := latest.DeepCopy()
		changed := upsertCondition(updated, reachable)
		if !changed {
			continue
		}

		// Use UpdateStatus to avoid racing spec updates
		_, err = client.CoreV1().Nodes().UpdateStatus(ctx, updated, metav1.UpdateOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to update status for node %s: %v\n", nodeName, err)

			// Optional: fall back to a strategic merge patch if your cluster is picky:
			// _ = patchNodeCondition(ctx, client, latest, updated)
			continue
		}

		fmt.Printf("  updated condition %q on node %s\n", ConditionTypeKubeletTCPReachable, nodeName)
	}

	fmt.Println("Done.")
}

// Optional: if you want to patch instead of UpdateStatus, you could implement something like this:
//
// func patchNodeCondition(ctx context.Context, client *kubernetes.Clientset, old, new *corev1.Node) error {
//     // Build a minimal status patch here if UpdateStatus is blocked by RBAC / admission,
//     // using e.g. strategic merge patch on status.conditions.
//     // Left as an exercise since it's cluster-policy-dependent.
//     return nil
// }
//
// You'd call it from main if UpdateStatus fails with a known conflict you care about.
