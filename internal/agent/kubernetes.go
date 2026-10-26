package agent

import (
	"k8gb-ui/internal/mutation"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewKubernetesClient constructs a client without discovery or any cluster I/O.
// An empty kubeconfig means in-cluster only, never an implicit local context.
func NewKubernetesClient(kubeconfig string, timeout time.Duration) (dynamic.Interface, error) {
	var config *rest.Config
	var err error
	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	config.Timeout = timeout
	config.QPS = 10
	config.Burst = 20
	config.UserAgent = "k8gb-ui-agent"
	config.Wrap(mutation.SingleAttemptTransport)
	return dynamic.NewForConfig(config)
}
