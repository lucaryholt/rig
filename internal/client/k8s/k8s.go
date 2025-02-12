package k8s

import (
	"context"
	"fmt"
	"os"
	"path"

	"github.com/rigdev/rig/internal/gateway/cluster"
	"github.com/rigdev/rig/internal/service/project"
	"go.uber.org/zap"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

type Client struct {
	logger *zap.Logger
	cs     *kubernetes.Clientset
	ps     project.Service
	mcs    *metricsclient.Clientset
}

var _ cluster.Gateway = &Client{}

func New(logger *zap.Logger, ps project.Service) (*Client, error) {
	var (
		restCfg *rest.Config
		err     error
	)
	restCfg, err = clientcmd.BuildConfigFromFlags("",
		path.Join(os.Getenv("HOME"), ".kube", "config"))
	if err != nil {
		restCfg, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("could not build rest config: %w", err)
		}
	}

	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("could not create kubernetes clientset: %w", err)
	}

	mcs, err := metricsclient.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("could not create kubernetes metrics clientset: %w", err)
	}

	return &Client{
		logger: logger,
		cs:     cs,
		mcs:    mcs,
		ps:     ps,
	}, nil
}

// CreateVolume implements cluster.Gateway.
func (*Client) CreateVolume(ctx context.Context, id string) error {
	// This is a noop as volumes are provisioned as part of a StatefulSet
	return nil
}
