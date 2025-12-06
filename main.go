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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
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
// It desn't retry because tcp itslf already retries but it does timeout after constant
func tcpReachable(ctx context.Context, addr string, port int) bool {
	target := fmt.Sprintf("%s:%d", addr, port)
	dialer := net.Dialer{}
	dialctx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, err := dialer.DialContext(dialctx, "tcp", target)
	cancel()
	if err == nil {
		_ = conn.Close()
		return true
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

func watchEvents(ctx context.Context, client kubernetes.Interface, port int) {
	// Use field selector to filter for KubeletTCPUnreachable events server-side
	watcher, err := client.CoreV1().Events("").Watch(ctx, metav1.ListOptions{
		FieldSelector: "reason=KubeletTCPUnreachable",
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to start watching events", "error", err)
		os.Exit(1)
	}
	defer watcher.Stop()

	slog.InfoContext(ctx, "Started watching events for KubeletTCPUnreachable from other hosts")

	for {
		select {
		case <-ctx.Done():
			fmt.Println("Shutting down...")
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				slog.WarnContext(ctx, "watch channel closed, restarting watch")
				return
			}

			k8sEvent, ok := event.Object.(*corev1.Event)
			if !ok {
				slog.WarnContext(ctx, "unexpected object type in watch event")
				continue
			}

			if k8sEvent.Source.Host == hostname {
				continue
			}

			tcpReachable(ctx, k8sEvent.InvolvedObject.Name, port) {
				return
			}
		}
	}
}

func synloop(ctx context.Context, client kubernetes.Interface, port int, interval time.Duration) {
	//use per loop context?s
	recorder := newEventRecorder(ctx, client, "nodesynack")
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

			// Check if node is schedulable (not cordoned)
			if n.Spec.Unschedulable {
				continue
			}

			// Check if node is ready
			isReady := false
			for _, condition := range n.Status.Conditions {
				if condition.Type == v1.NodeReady && condition.Status == v1.ConditionTrue {
					isReady = true
					break
				}
			}

			if !isReady {
				continue
			}

			go func(node v1.Node) {
				nodeName := n.Name
				nodeIP := pickNodeIP(&n)
				if nodeIP == "" {
					slog.WarnContext(ctx, "node has no usable IP; skipping", "node", nodeName)
					return
				}
				reachable := tcpReachable(ctx, nodeIP, port)

				if !reachable {
					recorder.Eventf(createEventNodeRef(n.Name), corev1.EventTypeWarning, // or corev1.EventTypeNormal
						"KubeletTCPUnreachable", // reason
						"Kubelet %s (%s:%d) is unreachable from %s",
						nodeName, nodeIP, port, hostname,
					)
					slog.ErrorContext(ctx, "unreachable", "node", nodeName, "ip", nodeIP, "uid", node.UID)
				}

			}(n)
		}
		select {
		case <-ctx.Done():
			fmt.Println("Shutting down...")
			return
		case <-time.After(interval):
			// continue the loop
		}
	}
}

var hostname string //global!!!

func main() {
	var (
		kubeconfig     = flag.String("kubeconfig", "", "Path to kubeconfig (if empty, use in-cluster config)")
		port           = flag.Int("port", defaultKubeletPort, "Kubelet TCP port to probe")
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

	hostname, err = os.Hostname()
	if err != nil {
		slog.ErrorContext(ctx, "failed to get hostname", "error", err)
		os.Exit(1)
	}

	synloop(ctx, client, *port, time.Duration(*loopTimeoutSec)*time.Second)

}

func newEventRecorder(ctx context.Context, client kubernetes.Interface, component string) record.EventRecorder {
	broadcaster := record.NewBroadcaster(record.WithContext(ctx))
	// Optional but nice:
	broadcaster.StartStructuredLogging(0)
	sink := &corev1client.EventSinkImpl{Interface: corev1client.New(client.CoreV1().RESTClient()).Events("")}
	broadcaster.StartLogging(slog.Info)
	broadcaster.StartRecordingToSink(sink)

	return broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{
		Component: component,
		Host:      hostname,
	})
}

// CreateEventNodeRef creates an ObjectReference to a Node to be used as the
// target in an Event.
// This is required because the Node Event references must have the UID of the
// ObjectRef set to the name of the Node, *NOT* the UID of the Node Object, so
// that the Events show up in the Event stream of the Node correctly.
// (like when executing a `kubectl describe <node>`)
// Letting the EventRecorder machinery build the ObjectRef automatically
// sets the Node Qbject's actual UID which does not correctly associate the
// Event to the Node.
func createEventNodeRef(nodeName string) *v1.ObjectReference {
	return &v1.ObjectReference{
		Kind:      "Node",
		Name:      string(nodeName),
		UID:       types.UID(nodeName), // not a UID, but that's how it works
		Namespace: "",
	}
}
