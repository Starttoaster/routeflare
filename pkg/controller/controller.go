package controller

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/starttoaster/routeflare/pkg/cloudflare"
	"github.com/starttoaster/routeflare/pkg/config"
	"github.com/starttoaster/routeflare/pkg/ddns"
	"github.com/starttoaster/routeflare/pkg/kubernetes"

	"k8s.io/apimachinery/pkg/runtime"
)

// Controller manages HTTPRoute watching and DNS record management
type Controller struct {
	cfg          *config.Config
	k8sClient    *kubernetes.Client
	cfClient     *cloudflare.Client
	ddnsDetector *ddns.Detector
	ctx          context.Context
	cancel       context.CancelFunc
	ddnsRoutes   map[string]*ddnsRoute
	ddnsMutex    sync.RWMutex
	ddnsInterval time.Duration
	httpServer   *http.Server
}

type ddnsRoute struct {
	namespace  string
	name       string
	zoneName   string
	recordName string
	recordType string
	ttl        int
	proxied    bool
	lastIPs    []string
}

// NewController creates a new controller
func NewController(cfg *config.Config, k8sClient *kubernetes.Client, cfClient *cloudflare.Client) *Controller {
	ctx, cancel := context.WithCancel(context.Background())
	return &Controller{
		cfg:          cfg,
		k8sClient:    k8sClient,
		cfClient:     cfClient,
		ddnsDetector: ddns.NewDetector(),
		ctx:          ctx,
		cancel:       cancel,
		ddnsRoutes:   make(map[string]*ddnsRoute),
		ddnsInterval: 5 * time.Minute, // Check every 5 minutes
	}
}

// Run starts the controller
func (c *Controller) Run() error {
	log.Println("Starting RouteFlare controller...")

	// Start healthcheck HTTP server
	if err := c.startHealthcheckServer(); err != nil {
		return fmt.Errorf("error starting healthcheck server: %w", err)
	}

	// Start DDNS background job
	go c.runDDNSJob()

	// Watch for HTTPRoute changes
	return c.watchHTTPRoutes()
}

// Stop stops the controller
func (c *Controller) Stop() {
	c.cancel()
	// Shutdown HTTP server gracefully
	if c.httpServer != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := c.httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("Error shutting down healthcheck server: %v", err)
		}
	}
}

// Healthcheck server

// startHealthcheckServer starts the HTTP server for healthcheck endpoint
func (c *Controller) startHealthcheckServer() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", c.healthcheckHandler)

	c.httpServer = &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		log.Println("Starting healthcheck server on :8080/healthz")
		if err := c.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Healthcheck server error: %v", err)
		}
	}()

	return nil
}

// healthcheckHandler handles the /healthz endpoint
func (c *Controller) healthcheckHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte("OK"))
	if err != nil {
		log.Printf("Healthcheck error writing response: %v\n", err)
	}
}

// Helper funcs

// extractRouteflareAnnotations gathers all routeflare-related settings from annotations
func extractRouteflareAnnotations(annotations map[string]string) map[string]string {
	const annotationPrefix = "routeflare/"
	result := make(map[string]string)
	for key, value := range annotations {
		if strings.HasPrefix(key, annotationPrefix) {
			settingName := strings.TrimPrefix(key, annotationPrefix)
			result[settingName] = value
		}
	}
	return result
}

// extractZoneFromRecordName gets the zone name from a domain (eg. "domain.tld" from "arbitrary.subdomain.levels.domain.tld")
func extractZoneFromRecordName(recordName string) (string, error) {
	parts := strings.Split(recordName, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid record name format: %s", recordName)
	}
	return strings.Join(parts[len(parts)-2:], "."), nil
}

// isIPv6 returns true if input is an IPv6 address
func isIPv6(ip string) bool {
	parsed := net.ParseIP(ip)
	return parsed != nil && parsed.To4() == nil
}

// isIPv4 returns true if input is an IPv4 address
func isIPv4(ip string) bool {
	parsed := net.ParseIP(ip)
	return parsed != nil && parsed.To4() != nil
}

// ipsEqual returns true if two string slice inputs are equal
func ipsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// getName gets the name out of a (kubernetes) runtime object's metadata
func getName(obj runtime.Object) string {
	if meta, ok := obj.(interface{ GetName() string }); ok {
		return meta.GetName()
	}
	return "unknown"
}

// getNamespace gets the namespace out of a (kubernetes) runtime object's metadata
func getNamespace(obj runtime.Object) string {
	if meta, ok := obj.(interface{ GetNamespace() string }); ok {
		return meta.GetNamespace()
	}
	return "unknown"
}
