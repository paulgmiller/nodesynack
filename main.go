package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
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

// pickNodeIP grabs the interal ip.  Extenal ip, or hostname are ignored for now
func pickNodeIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}

// tcpReachable tries to complete a TCP handshake to addr:port up to maxRetries.
// This is effectively "send SYN, expect SYN-ACK" in terms of reachability.
// Doing this to keep
func tcpReachable(ctx context.Context, addr string, port int, retries int) bool {
	target := fmt.Sprintf("%s:%d", addr, port)
	dialer := net.Dialer{}

	//use
	for range retries {
		dialctx, cancel := context.WithTimeout(ctx, dialTimeout)
		conn, err := dialer.DialContext(dialctx, "tcp", target)
		cancel()
		if err == nil {
			_ = conn.Close()
			return true
		}
		//pass in node name for better logging?
		// small delay betwee`n retries
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
	return false
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
		kubeconfig     = flag.String("kubeconfig", "", "Path to kubeconfig (if empty, use in-cluster config)")
		port           = flag.Int("port", defaultKubeletPort, "Kubelet TCP port to probe")
		retries        = flag.Int("retries", maxRetries, "Number of TCP dial retries")
		loopTimeoutSec = flag.Int("timeout-seconds", int(dialTimeout.Seconds()*5), "Per loop timeout in seconds")
	)
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	client, err := buildKubeClient(*kubeconfig)
	if err != nil {
		slog.ErrorContext(ctx, "failed to build kube client", "error", err)
		os.Exit(1)
	}

	hostname, err := os.Hostname()
	if err != nil {
		slog.ErrorContext(ctx, "failed to get hostname", "error", err)
		os.Exit(1)
	}

	for {
		//TODO watch instead and and remove from running go routines?
		//filter out un ready
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			slog.ErrorContext(ctx, "failed to list nodes", "error", err)
			os.Exit(1) //should we crash or just contiue?
		}

		slog.InfoContext(ctx, "Found nodes", "count", len(nodes.Items))
		//wait group? or cancel context each loop?
		for _, n := range nodes.Items {
			nodeName := n.Name
			nodeIP := pickNodeIP(&n)
			if nodeIP == "" {
				fmt.Fprintf(os.Stderr, "node %s has no usable IP; skipping\n", nodeName)
				continue
			}

			//if nodes.Status.Conditions
			go func(nodeName, nodeIP string) {
				reachable := tcpReachable(ctx, nodeIP, *port, *retries)

				if !reachable {
					_, err = client.CoreV1().Events("").Create(ctx, &corev1.Event{
						ObjectMeta: metav1.ObjectMeta{
							GenerateName: fmt.Sprintf("%s-kubelet-tcp-unreachable-", nodeName),
							Namespace:    metav1.NamespaceDefault,
						},
						InvolvedObject: corev1.ObjectReference{
							Kind:      "Node",
							Name:      nodeName,
							Namespace: metav1.NamespaceDefault,
						},
						Reason:  "KubeletTCPUnreachable",
						Message: fmt.Sprintf("Kubelet %s (%s:%d) is unreachable from %s", nodeName, nodeIP, *port, hostname),
					}, metav1.CreateOptions{})
					slog.ErrorContext(ctx, "unreachable", "node", nodeName, "ip", nodeIP)
				}
				if err != nil {
					slog.ErrorContext(ctx, "failed to create event", "node", nodeName, "error", err)
				}

			}(nodeName, nodeIP)
		}
		select {
		case <-ctx.Done():
			fmt.Println("Shutting down...")
			return
		case <-time.After(time.Duration(*loopTimeoutSec) * time.Second):
			// continue the loop
		}

	}

}
