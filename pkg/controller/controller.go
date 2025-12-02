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
	"github.com/starttoaster/routeflare/pkg/gateway"
	"github.com/starttoaster/routeflare/pkg/kubernetes"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
)

const (
	annotationPrefix = "routeflare/"
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

	// List all existing HTTPRoutes
	routes, err := c.k8sClient.ListHTTPRoutes(c.ctx)
	if err != nil {
		return fmt.Errorf("error listing HTTPRoutes: %w", err)
	}

	log.Printf("Found %d HTTPRoute(s)", len(routes))
	for _, route := range routes {
		c.processHTTPRoute(route, false)
	}

	// Watch for changes
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

// watchHTTPRoutes watches for HTTPRoute changes
func (c *Controller) watchHTTPRoutes() error {
	log.Println("Watching for HTTPRoute changes...")

	for {
		watcher, err := c.k8sClient.WatchHTTPRoutes(c.ctx)
		if err != nil {
			return fmt.Errorf("error watching HTTPRoutes: %w", err)
		}

		log.Println("HTTPRoute watcher started. Waiting for events...")

		for {
			select {
			case <-c.ctx.Done():
				log.Println("Context cancelled, stopping watcher")
				watcher.Stop()
				return nil
			case event, ok := <-watcher.ResultChan():
				if !ok {
					log.Println("Watch channel closed, reconnecting...")
					watcher.Stop()
					time.Sleep(5 * time.Second)
					break // Break inner loop to reconnect
				}

				switch event.Type {
				case watch.Added:
					if route, ok := event.Object.(*unstructured.Unstructured); ok {
						log.Printf("HTTPRoute added: %s/%s", route.GetNamespace(), route.GetName())
						c.processHTTPRoute(route, false)
					}
				case watch.Modified:
					if route, ok := event.Object.(*unstructured.Unstructured); ok {
						log.Printf("HTTPRoute modified: %s/%s", route.GetNamespace(), route.GetName())
						c.processHTTPRoute(route, false)
					}
				case watch.Deleted:
					log.Printf("HTTPRoute deleted: %s/%s", getNamespace(event.Object), getName(event.Object))
					c.processHTTPRouteDeletion(event.Object)
				case watch.Error:
					log.Printf("Watch error: %v", event.Object)
				}
			}
		}
	}
}

// processHTTPRoute processes a single HTTPRoute
func (c *Controller) processHTTPRoute(route *unstructured.Unstructured, isDDNSUpdate bool) {
	name, namespace, annotations, err := kubernetes.ExtractHTTPRouteMetadata(route)
	if err != nil {
		log.Printf("Error extracting metadata from HTTPRoute: %v", err)
		return
	}

	// Extract routeflare annotations
	routeflareAnns := extractRouteflareAnnotations(annotations)
	if len(routeflareAnns) == 0 {
		return // No routeflare annotations, skip
	}

	// Check for required content-mode annotation
	contentMode, ok := routeflareAnns["content-mode"]
	if !ok || contentMode == "" {
		return // No content-mode, skip
	}

	// Get record name from HTTPRoute spec.hostnames
	recordName, err := getRecordNameFromHTTPRoute(route)
	if err != nil {
		log.Printf("Error getting record name from HTTPRoute %s/%s: %v", namespace, name, err)
		return
	}

	// Get zone name from HTTPRoute (we'll derive it from the record name for now)
	// In a real implementation, you might want to add a routeflare/zone annotation
	zoneName, err := extractZoneFromRecordName(recordName)
	if err != nil {
		log.Printf("Error extracting zone from record name %s for HTTPRoute %s/%s: %v", recordName, namespace, name, err)
		return
	}

	// Parse other annotations
	recordType := routeflareAnns["type"]
	if recordType == "" {
		recordType = "A" // Default to A
	}

	ttl, err := cloudflare.ParseTTL(routeflareAnns["ttl"])
	if err != nil {
		log.Printf("Error parsing TTL for HTTPRoute %s/%s: %v", namespace, name, err)
		ttl = 1 // Default to auto
	}

	proxied, err := cloudflare.ParseProxied(routeflareAnns["proxied"])
	if err != nil {
		log.Printf("Error parsing proxied for HTTPRoute %s/%s: %v", namespace, name, err)
		proxied = false
	}

	// Process based on content mode
	switch contentMode {
	case "gateway-address":
		c.processGatewayAddressMode(route, zoneName, recordName, recordType, ttl, proxied)
	case "ddns":
		c.processDDNSMode(route, namespace, name, zoneName, recordName, recordType, ttl, proxied, isDDNSUpdate)
	default:
		log.Printf("Unknown content-mode '%s' for HTTPRoute %s/%s", contentMode, namespace, name)
	}
}

// processGatewayAddressMode processes HTTPRoute with gateway-address content mode
func (c *Controller) processGatewayAddressMode(route *unstructured.Unstructured, zoneName, recordName, recordType string, ttl int, proxied bool) {
	// Get parent Gateway references
	parents, found, err := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	if !found || err != nil || len(parents) == 0 {
		log.Printf("HTTPRoute %s/%s has no parentRefs", route.GetNamespace(), route.GetName())
		return
	}

	// Get the first parent Gateway
	parentRef, ok := parents[0].(map[string]interface{})
	if !ok {
		log.Printf("Invalid parentRef format for HTTPRoute %s/%s", route.GetNamespace(), route.GetName())
		return
	}

	gatewayName, found, err := unstructured.NestedString(parentRef, "name")
	if !found || err != nil {
		log.Printf("Could not get gateway name from parentRef for HTTPRoute %s/%s", route.GetNamespace(), route.GetName())
		return
	}

	gatewayNamespace, found, err := unstructured.NestedString(parentRef, "namespace")
	if !found || err != nil {
		gatewayNamespace = route.GetNamespace() // Default to HTTPRoute namespace
	}

	// Get the Gateway
	gatewayObj, err := c.k8sClient.GetGateway(c.ctx, gatewayNamespace, gatewayName)
	if err != nil {
		log.Printf("Error getting Gateway %s/%s: %v", gatewayNamespace, gatewayName, err)
		return
	}

	// Extract IP addresses from Gateway
	ips, err := gateway.GetGatewayAddresses(gatewayObj, recordType)
	if err != nil {
		log.Printf("Error getting Gateway addresses: %v", err)
		return
	}

	// Get zone ID
	zoneID, err := c.cfClient.GetZoneIDByName(zoneName)
	if err != nil {
		log.Printf("Error getting zone ID for %s: %v", zoneName, err)
		return
	}

	// Create/update DNS records
	if recordType == "A/AAAA" {
		// Create both A and AAAA records
		for _, ip := range ips {
			recordTypeForIP := "A"
			if isIPv6(ip) {
				recordTypeForIP = "AAAA"
			}

			record := cloudflare.DNSRecord{
				Type:    cloudflare.RecordType(recordTypeForIP),
				Name:    recordName,
				Content: ip,
				TTL:     ttl,
				Proxied: proxied,
			}

			_, err := c.cfClient.UpsertRecord(c.ctx, zoneID, record)
			if err != nil {
				log.Printf("Error upserting %s record %s: %v", recordTypeForIP, recordName, err)
			} else {
				log.Printf("Successfully upserted %s record %s -> %s", recordTypeForIP, recordName, ip)
			}
		}
	} else {
		// Single record type
		if len(ips) == 0 {
			log.Printf("No IP addresses found for record type %s", recordType)
			return
		}

		record := cloudflare.DNSRecord{
			Type:    cloudflare.RecordType(recordType),
			Name:    recordName,
			Content: ips[0],
			TTL:     ttl,
			Proxied: proxied,
		}

		_, err := c.cfClient.UpsertRecord(c.ctx, zoneID, record)
		if err != nil {
			log.Printf("Error upserting record %s: %v", recordName, err)
		} else {
			log.Printf("Successfully upserted %s record %s -> %s", recordType, recordName, ips[0])
		}
	}
}

// processDDNSMode processes HTTPRoute with ddns content mode
func (c *Controller) processDDNSMode(route *unstructured.Unstructured, namespace, name, zoneName, recordName, recordType string, ttl int, proxied bool, isDDNSUpdate bool) {
	// Get current public IPs
	ips, err := c.ddnsDetector.GetPublicIPsByType(c.ctx, recordType)
	if err != nil {
		log.Printf("Error getting public IPs for HTTPRoute %s/%s: %v", namespace, name, err)
		return
	}

	// Check if IPs have changed (only for updates, not initial processing)
	routeKey := fmt.Sprintf("%s/%s", namespace, name)
	if isDDNSUpdate {
		c.ddnsMutex.RLock()
		ddnsRoute, exists := c.ddnsRoutes[routeKey]
		c.ddnsMutex.RUnlock()

		if exists && ipsEqual(ddnsRoute.lastIPs, ips) {
			return // IPs haven't changed, skip update
		}
	}

	// Get zone ID
	zoneID, err := c.cfClient.GetZoneIDByName(zoneName)
	if err != nil {
		log.Printf("Error getting zone ID for %s: %v", zoneName, err)
		return
	}

	// Create/update DNS records
	if recordType == "A/AAAA" {
		// Create both A and AAAA records
		for _, ip := range ips {
			recordTypeForIP := "A"
			if isIPv6(ip) {
				recordTypeForIP = "AAAA"
			}

			record := cloudflare.DNSRecord{
				Type:    cloudflare.RecordType(recordTypeForIP),
				Name:    recordName,
				Content: ip,
				TTL:     ttl,
				Proxied: proxied,
			}

			_, err := c.cfClient.UpsertRecord(c.ctx, zoneID, record)
			if err != nil {
				log.Printf("Error upserting %s record %s: %v", recordTypeForIP, recordName, err)
			} else {
				log.Printf("Successfully upserted %s record %s -> %s", recordTypeForIP, recordName, ip)
			}
		}
	} else {
		// Single record type
		if len(ips) == 0 {
			log.Printf("No IP addresses found for record type %s", recordType)
			return
		}

		record := cloudflare.DNSRecord{
			Type:    cloudflare.RecordType(recordType),
			Name:    recordName,
			Content: ips[0],
			TTL:     ttl,
			Proxied: proxied,
		}

		_, err := c.cfClient.UpsertRecord(c.ctx, zoneID, record)
		if err != nil {
			log.Printf("Error upserting record %s: %v", recordName, err)
		} else {
			log.Printf("Successfully upserted %s record %s -> %s", recordType, recordName, ips[0])
		}
	}

	// Store DDNS route info
	c.ddnsMutex.Lock()
	c.ddnsRoutes[routeKey] = &ddnsRoute{
		namespace:  namespace,
		name:       name,
		zoneName:   zoneName,
		recordName: recordName,
		recordType: recordType,
		ttl:        ttl,
		proxied:    proxied,
		lastIPs:    ips,
	}
	c.ddnsMutex.Unlock()
}

// processHTTPRouteDeletion handles HTTPRoute deletion
func (c *Controller) processHTTPRouteDeletion(obj runtime.Object) {
	if !c.cfg.ShouldDelete() {
		return // Upsert-only strategy, don't delete
	}

	name, namespace, annotations, err := kubernetes.ExtractHTTPRouteMetadata(obj)
	if err != nil {
		log.Printf("Error extracting metadata from deleted HTTPRoute: %v", err)
		return
	}

	routeflareAnns := extractRouteflareAnnotations(annotations)
	if len(routeflareAnns) == 0 {
		return
	}

	// Get record name
	route, ok := obj.(*unstructured.Unstructured)
	if !ok {
		log.Printf("Could not convert deleted object to unstructured")
		return
	}

	recordName, err := getRecordNameFromHTTPRoute(route)
	if err != nil {
		log.Printf("Error getting record name from deleted HTTPRoute %s/%s: %v", namespace, name, err)
		return
	}

	zoneName, err := extractZoneFromRecordName(recordName)
	if err != nil {
		log.Printf("Error extracting zone from record name %s: %v", recordName, err)
		return
	}

	recordType := routeflareAnns["type"]
	if recordType == "" {
		recordType = "A"
	}

	// Get zone ID
	zoneID, err := c.cfClient.GetZoneIDByName(zoneName)
	if err != nil {
		log.Printf("Error getting zone ID for %s: %v", zoneName, err)
		return
	}

	// Delete DNS records
	if recordType == "A/AAAA" {
		// Delete both A and AAAA records
		for _, rt := range []string{"A", "AAAA"} {
			record, err := c.cfClient.FindRecord(c.ctx, zoneID, recordName, cloudflare.RecordType(rt))
			if err != nil {
				log.Printf("Error finding %s record %s: %v", rt, recordName, err)
				continue
			}
			if record != nil {
				if err := c.cfClient.DeleteRecord(c.ctx, zoneID, record.ID); err != nil {
					log.Printf("Error deleting %s record %s: %v", rt, recordName, err)
				} else {
					log.Printf("Successfully deleted %s record %s", rt, recordName)
				}
			}
		}
	} else {
		record, err := c.cfClient.FindRecord(c.ctx, zoneID, recordName, cloudflare.RecordType(recordType))
		if err != nil {
			log.Printf("Error finding record %s: %v", recordName, err)
			return
		}
		if record != nil {
			if err := c.cfClient.DeleteRecord(c.ctx, zoneID, record.ID); err != nil {
				log.Printf("Error deleting record %s: %v", recordName, err)
			} else {
				log.Printf("Successfully deleted %s record %s", recordType, recordName)
			}
		}
	}

	// Remove from DDNS routes if present
	routeKey := fmt.Sprintf("%s/%s", namespace, name)
	c.ddnsMutex.Lock()
	delete(c.ddnsRoutes, routeKey)
	c.ddnsMutex.Unlock()
}

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

// runDDNSJob runs a background job to check for IP changes in DDNS routes
func (c *Controller) runDDNSJob() {
	ticker := time.NewTicker(c.ddnsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.ddnsMutex.RLock()
			routes := make([]*ddnsRoute, 0, len(c.ddnsRoutes))
			for _, route := range c.ddnsRoutes {
				routes = append(routes, route)
			}
			c.ddnsMutex.RUnlock()

			for _, ddnsRoute := range routes {
				// Get the specific HTTPRoute
				route, err := c.k8sClient.GetHTTPRoute(c.ctx, ddnsRoute.namespace, ddnsRoute.name)
				if err != nil {
					// Route might have been deleted, remove from tracking
					log.Printf("Error getting HTTPRoute %s/%s for DDNS update (may have been deleted): %v", ddnsRoute.namespace, ddnsRoute.name, err)
					c.ddnsMutex.Lock()
					routeKey := fmt.Sprintf("%s/%s", ddnsRoute.namespace, ddnsRoute.name)
					delete(c.ddnsRoutes, routeKey)
					c.ddnsMutex.Unlock()
					continue
				}

				c.processHTTPRoute(route, true) // true = isDDNSUpdate
			}
		}
	}
}

// Helper functions

func extractRouteflareAnnotations(annotations map[string]string) map[string]string {
	result := make(map[string]string)
	for key, value := range annotations {
		if strings.HasPrefix(key, annotationPrefix) {
			settingName := strings.TrimPrefix(key, annotationPrefix)
			result[settingName] = value
		}
	}
	return result
}

func getRecordNameFromHTTPRoute(route *unstructured.Unstructured) (string, error) {
	hostnames, found, err := unstructured.NestedStringSlice(route.Object, "spec", "hostnames")
	if !found || err != nil || len(hostnames) == 0 {
		return "", fmt.Errorf("HTTPRoute has no hostnames in spec")
	}
	return hostnames[0], nil
}

func extractZoneFromRecordName(recordName string) (string, error) {
	parts := strings.Split(recordName, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid record name format: %s", recordName)
	}
	// Assume the zone is everything after the first part
	// e.g., api.example.com -> example.com
	return strings.Join(parts[len(parts)-2:], "."), nil
}

func isIPv6(ip string) bool {
	parsed := net.ParseIP(ip)
	return parsed != nil && parsed.To4() == nil
}

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

func getName(obj runtime.Object) string {
	if meta, ok := obj.(interface{ GetName() string }); ok {
		return meta.GetName()
	}
	return "unknown"
}

func getNamespace(obj runtime.Object) string {
	if meta, ok := obj.(interface{ GetNamespace() string }); ok {
		return meta.GetNamespace()
	}
	return "unknown"
}
