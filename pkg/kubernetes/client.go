package kubernetes

import (
	"context"
	"fmt"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	httpRouteGVR = schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "httproutes",
	}

	gatewayGVR = schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "gateways",
	}
)

// Client wraps Kubernetes clients
type Client struct {
	dynamicClient     dynamic.Interface
	clientset         kubernetes.Interface
	informerFactory   dynamicinformer.DynamicSharedInformerFactory
	httpRouteInformer cache.SharedInformer
}

// NewClient creates a new Kubernetes client
func NewClient(kubeconfigPath string) (*Client, error) {
	config, err := getKubernetesConfig(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("error getting Kubernetes config: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating dynamic client: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating clientset: %w", err)
	}

	// Verify connection
	_, err = clientset.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{Limit: 1})
	if err != nil {
		return nil, fmt.Errorf("error connecting to Kubernetes cluster: %w", err)
	}

	// Create informer factory
	informerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)

	// Create HTTPRoute informer
	httpRouteInformer := informerFactory.ForResource(httpRouteGVR).Informer()

	return &Client{
		dynamicClient:     dynamicClient,
		clientset:         clientset,
		informerFactory:   informerFactory,
		httpRouteInformer: httpRouteInformer,
	}, nil
}

// getKubernetesConfig returns Kubernetes config, trying in-cluster first, then kubeconfig
func getKubernetesConfig(kubeconfigPath string) (*rest.Config, error) {
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}

	// Fall back to kubeconfig file
	if kubeconfigPath == "" {
		kubeconfigPath = filepath.Join(homedir.HomeDir(), ".kube", "config")
	}

	config, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("error building config from kubeconfig: %w", err)
	}

	return config, nil
}

// ListHTTPRoutes lists all HTTPRoutes
func (c *Client) ListHTTPRoutes(ctx context.Context) ([]*unstructured.Unstructured, error) {
	httpRouteClient := c.dynamicClient.Resource(httpRouteGVR)
	list, err := httpRouteClient.Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error listing HTTPRoutes: %w", err)
	}

	var routes []*unstructured.Unstructured
	for i := range list.Items {
		routes = append(routes, &list.Items[i])
	}

	return routes, nil
}

// GetHTTPRouteInformer returns the HTTPRoute informer
func (c *Client) GetHTTPRouteInformer() cache.SharedInformer {
	return c.httpRouteInformer
}

// StartInformerFactory starts the informer factory
func (c *Client) StartInformerFactory(stopCh <-chan struct{}) {
	c.informerFactory.Start(stopCh)
}

// WaitForCacheSync waits for the HTTPRoute informer cache to sync
func (c *Client) WaitForCacheSync(ctx context.Context) bool {
	return cache.WaitForCacheSync(ctx.Done(), c.httpRouteInformer.HasSynced)
}

// GetGateway gets a Gateway by namespace and name
func (c *Client) GetGateway(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	gatewayClient := c.dynamicClient.Resource(gatewayGVR)
	return gatewayClient.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

// GetHTTPRoute gets an HTTPRoute by namespace and name
func (c *Client) GetHTTPRoute(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	httpRouteClient := c.dynamicClient.Resource(httpRouteGVR)
	return httpRouteClient.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

// ExtractHTTPRouteMetadata extracts metadata from an HTTPRoute object
func ExtractHTTPRouteMetadata(obj runtime.Object) (name, namespace string, annotations map[string]string, err error) {
	// Try to get metadata directly first
	meta, ok := obj.(metav1.Object)
	if ok {
		return meta.GetName(), meta.GetNamespace(), meta.GetAnnotations(), nil
	}

	// If that fails, try unstructured
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return "", "", nil, fmt.Errorf("could not convert object to unstructured.Unstructured or metav1.Object")
	}

	return unstructuredObj.GetName(), unstructuredObj.GetNamespace(), unstructuredObj.GetAnnotations(), nil
}
