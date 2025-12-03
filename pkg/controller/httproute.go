package controller

import (
	"fmt"
	"time"

	"github.com/chia-network/go-modules/pkg/slogs"
	"github.com/starttoaster/routeflare/pkg/cloudflare"
	"github.com/starttoaster/routeflare/pkg/gateway"
	"github.com/starttoaster/routeflare/pkg/kubernetes"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
)

// startHTTPRouteInformer starts the HTTPRoute informer and sets up event handlers
func (c *Controller) startHTTPRouteInformer() error {
	informer := c.k8sClient.GetHTTPRouteInformer()

	// Set up event handlers
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if route, ok := obj.(*unstructured.Unstructured); ok {
				slogs.Logr.Info("HTTPRoute added", "route", fmt.Sprintf("%s/%s", route.GetNamespace(), route.GetName()))
				c.processHTTPRoute(route, false)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if route, ok := newObj.(*unstructured.Unstructured); ok {
				slogs.Logr.Info("HTTPRoute modified", "route", fmt.Sprintf("%s/%s", route.GetNamespace(), route.GetName()))
				c.processHTTPRoute(route, false)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// Handle deletion - obj might be a DeletedFinalStateUnknown
			var route *unstructured.Unstructured
			switch t := obj.(type) {
			case *unstructured.Unstructured:
				route = t
			case cache.DeletedFinalStateUnknown:
				if deleted, ok := t.Obj.(*unstructured.Unstructured); ok {
					route = deleted
				} else {
					slogs.Logr.Warn("Could not convert deleted object to unstructured", "type", fmt.Sprintf("%T", t.Obj))
					return
				}
			default:
				slogs.Logr.Warn("Unknown object type in delete handler", "type", fmt.Sprintf("%T", obj))
				return
			}
			slogs.Logr.Info("HTTPRoute deleted", "route", fmt.Sprintf("%s/%s", route.GetNamespace(), route.GetName()))
			c.processHTTPRouteDeletion(route)
		},
	})
	if err != nil {
		return fmt.Errorf("error adding event handlers: %w", err)
	}

	// Start the informer factory
	stopCh := make(chan struct{})
	go func() {
		<-c.ctx.Done()
		close(stopCh)
	}()
	c.k8sClient.StartInformerFactory(stopCh)

	// Wait for cache to sync
	slogs.Logr.Info("Waiting for HTTPRoute informer cache to sync...")
	if !c.k8sClient.WaitForCacheSync(c.ctx) {
		return fmt.Errorf("error waiting for HTTPRoute informer cache to sync")
	}
	slogs.Logr.Info("HTTPRoute informer cache synced")

	// Process existing HTTPRoutes from cache
	return c.processExistingHTTPRoutes(informer)
}

// processExistingHTTPRoutes processes all existing HTTPRoutes from the informer cache
func (c *Controller) processExistingHTTPRoutes(informer cache.SharedInformer) error {
	routes := informer.GetStore().List()
	slogs.Logr.Info("Processing existing HTTPRoutes from cache", "count", len(routes))

	for _, obj := range routes {
		if route, ok := obj.(*unstructured.Unstructured); ok {
			c.processHTTPRoute(route, false)
		}
	}

	return nil
}

// processHTTPRoute processes a single HTTPRoute
func (c *Controller) processHTTPRoute(route *unstructured.Unstructured, isReconciliationUpdate bool) {
	name, namespace, annotations, err := kubernetes.ExtractHTTPRouteMetadata(route)
	if err != nil {
		slogs.Logr.Error("extracting metadata from HTTPRoute", "error", err)
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
		slogs.Logr.Error("getting record name from HTTPRoute",
			"route", fmt.Sprintf("%s/%s", namespace, name),
			"error", err)
		return
	}

	// Get zone name from HTTPRoute
	// TODO: any downsides to the zone being derived from the HTTPRoute? Should zone be an annotation? For now this seems sufficient.
	zoneName, err := extractZoneFromRecordName(recordName)
	if err != nil {
		slogs.Logr.Error("extracting zone from record name for HTTPRoute",
			"record", recordName,
			"route", fmt.Sprintf("%s/%s", namespace, name),
			"error", err)
		return
	}

	// Parse other annotations
	recordType := routeflareAnns["type"]
	if recordType == "" {
		recordType = "A" // Default to A
	}

	ttl, err := cloudflare.ParseTTL(routeflareAnns["ttl"])
	if err != nil {
		slogs.Logr.Error("parsing TTL for HTTPRoute",
			"route", fmt.Sprintf("%s/%s", namespace, name),
			"error", err)
		ttl = 1 // Default to auto
	}

	proxied, err := cloudflare.ParseProxied(routeflareAnns["proxied"])
	if err != nil {
		slogs.Logr.Error("parsing proxied for HTTPRoute",
			"route", fmt.Sprintf("%s/%s", namespace, name),
			"error", err)
		proxied = false
	}

	// Process based on content mode
	switch contentMode {
	case "gateway-address":
		c.processGatewayAddressMode(route, namespace, name, zoneName, recordName, recordType, ttl, proxied, isReconciliationUpdate)
	case "ddns":
		c.processDDNSMode(route, namespace, name, zoneName, recordName, recordType, ttl, proxied, isReconciliationUpdate)
	default:
		slogs.Logr.Warn("Unknown content-mode for HTTPRoute", "route", fmt.Sprintf("%s/%s", namespace, name))
	}
}

// processGatewayAddressMode processes HTTPRoute with gateway-address content mode
func (c *Controller) processGatewayAddressMode(route *unstructured.Unstructured, namespace, name, zoneName, recordName, recordType string, ttl int, proxied bool, isReconciliationUpdate bool) {
	// Get parent Gateway references
	parents, found, err := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	if !found || err != nil || len(parents) == 0 {
		slogs.Logr.Warn("HTTPRoute does not have parentRefs", "route", fmt.Sprintf("%s/%s", route.GetNamespace(), route.GetName()))
		return
	}

	// Get the first parent Gateway
	// TODO: I don't think multiple Gateways can ever be supported by this tool, however, we may not be able to assume that the first object in the parentRefs is a Gateway.
	// This logic may need to be built out to find the first actual Gateway parent in this list, or else another parent API object that references a Gateway itself.
	parentRef, ok := parents[0].(map[string]interface{})
	if !ok {
		slogs.Logr.Warn("Invalid parentRef format for HTTPRoute", "route", fmt.Sprintf("%s/%s", route.GetNamespace(), route.GetName()))
		return
	}

	gatewayName, found, err := unstructured.NestedString(parentRef, "name")
	if !found || err != nil {
		slogs.Logr.Warn("Could not get gateway name from parentRef for HTTPRoute", "route", fmt.Sprintf("%s/%s", route.GetNamespace(), route.GetName()))
		return
	}

	gatewayNamespace, found, err := unstructured.NestedString(parentRef, "namespace")
	if !found || err != nil {
		gatewayNamespace = route.GetNamespace() // Default to HTTPRoute namespace
	}

	// Get the Gateway
	gatewayObj, err := c.k8sClient.GetGateway(c.ctx, gatewayNamespace, gatewayName)
	if err != nil {
		slogs.Logr.Error("getting Gateway",
			"gateway", fmt.Sprintf("%s/%s", gatewayNamespace, gatewayName),
			"error", err)
		return
	}

	// Extract IP addresses from Gateway
	ips, err := gateway.GetGatewayAddresses(gatewayObj, recordType)
	if err != nil {
		slogs.Logr.Error("getting Gateway addresses",
			"gateway", fmt.Sprintf("%s/%s", gatewayNamespace, gatewayName),
			"error", err)
		return
	}

	// For reconciliation, we always update to fix any drift (e.g., manual DNS changes in Cloudflare)
	// even if Gateway IPs haven't changed. This ensures DNS records always match Gateway addresses.
	routeKey := fmt.Sprintf("%s/%s", namespace, name)

	// Get zone ID
	zoneID, err := c.cfClient.GetZoneIDByName(zoneName)
	if err != nil {
		slogs.Logr.Error("getting zone ID from name", "zone-name", zoneName, "error", err)
		return
	}

	// Create/update DNS records (always update to ensure reconciliation fixes drift)
	err = c.createOrUpdateRecords(recordType, zoneID, ips, recordName, ttl, proxied)
	if err != nil {
		slogs.Logr.Error("creating or updating records", "error", err)
		return
	}

	// Store route info for periodic reconciliation
	c.routesMutex.Lock()
	c.trackedRoutes[routeKey] = &trackedRoute{
		contentMode:      "gateway-address",
		namespace:        namespace,
		name:             name,
		zoneName:         zoneName,
		recordName:       recordName,
		recordType:       recordType,
		ttl:              ttl,
		proxied:          proxied,
		lastIPs:          ips,
		gatewayNamespace: gatewayNamespace,
		gatewayName:      gatewayName,
	}
	c.routesMutex.Unlock()
}

// processDDNSMode processes HTTPRoute with ddns content mode
func (c *Controller) processDDNSMode(route *unstructured.Unstructured, namespace, name, zoneName, recordName, recordType string, ttl int, proxied bool, isReconciliationUpdate bool) {
	// Get current public IPs
	ips, err := c.ddnsDetector.GetPublicIPsByType(c.ctx, recordType)
	if err != nil {
		slogs.Logr.Error("getting public IPs for HTTPRoute",
			"route", fmt.Sprintf("%s/%s", namespace, name),
			"error", err)
		return
	}

	// Check if IPs have changed (only for reconciliation updates, not initial processing)
	routeKey := fmt.Sprintf("%s/%s", namespace, name)
	if isReconciliationUpdate {
		c.routesMutex.RLock()
		trackedRoute, exists := c.trackedRoutes[routeKey]
		c.routesMutex.RUnlock()

		if exists && ipsEqual(trackedRoute.lastIPs, ips) {
			return // IPs haven't changed, skip update
		}
	}

	// Get zone ID
	zoneID, err := c.cfClient.GetZoneIDByName(zoneName)
	if err != nil {
		slogs.Logr.Error("getting zone ID from name", "zone-name", zoneName, "error", err)
		return
	}

	// Create/update DNS records
	err = c.createOrUpdateRecords(recordType, zoneID, ips, recordName, ttl, proxied)
	if err != nil {
		slogs.Logr.Error("creating or updating records", "error", err)
	}

	// Store route info for periodic reconciliation
	c.routesMutex.Lock()
	c.trackedRoutes[routeKey] = &trackedRoute{
		contentMode: "ddns",
		namespace:   namespace,
		name:        name,
		zoneName:    zoneName,
		recordName:  recordName,
		recordType:  recordType,
		ttl:         ttl,
		proxied:     proxied,
		lastIPs:     ips,
	}
	c.routesMutex.Unlock()
}

func (c *Controller) createOrUpdateRecords(recordType string, zoneID string, ips []string, recordName string, ttl int, proxied bool) error {
	if len(ips) == 0 {
		return fmt.Errorf("no IP addresses found for record type %s", recordType)
	}

	switch recordType {
	case "A/AAAA":
		var createdIPv4 bool
		var createdIPv6 bool
		for _, ip := range ips {
			var recordTypeForIP string
			if isIPv6(ip) {
				recordTypeForIP = "AAAA"
			}
			if isIPv4(ip) {
				recordTypeForIP = "A"
			}
			if recordTypeForIP == "" {
				slogs.Logr.Warn("Skipping invalid IP address", "ip", ip)
				continue
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
				slogs.Logr.Error("upserting record", "type", recordTypeForIP, "name", recordName, "error", err)
				continue
			}

			// Break out of loop if we've created a record for IPv4 and IPv6
			if recordTypeForIP == "A" {
				createdIPv4 = true
			}
			if recordTypeForIP == "AAAA" {
				createdIPv6 = true
			}
			if createdIPv4 && createdIPv6 {
				break
			}
		}
	case "AAAA":
		for _, ip := range ips {
			if isIPv6(ip) {
				record := cloudflare.DNSRecord{
					Type:    cloudflare.RecordType(recordType),
					Name:    recordName,
					Content: ip,
					TTL:     ttl,
					Proxied: proxied,
				}

				_, err := c.cfClient.UpsertRecord(c.ctx, zoneID, record)
				if err != nil {
					slogs.Logr.Error("upserting record", "type", recordType, "name", recordName, "error", err)
				}
				return nil
			}
		}
	case "A":
		for _, ip := range ips {
			if isIPv4(ip) {
				record := cloudflare.DNSRecord{
					Type:    cloudflare.RecordType(recordType),
					Name:    recordName,
					Content: ip,
					TTL:     ttl,
					Proxied: proxied,
				}

				_, err := c.cfClient.UpsertRecord(c.ctx, zoneID, record)
				if err != nil {
					slogs.Logr.Error("upserting record", "type", recordType, "name", recordName, "error", err)
				}
				return nil
			}
		}
	}

	return nil
}

// processHTTPRouteDeletion handles HTTPRoute deletion
func (c *Controller) processHTTPRouteDeletion(obj runtime.Object) {
	if !c.cfg.ShouldDelete() {
		return // Upsert-only strategy, don't delete
	}

	name, namespace, annotations, err := kubernetes.ExtractHTTPRouteMetadata(obj)
	if err != nil {
		slogs.Logr.Error("extracting metadata from deleted HTTPRoute", "error", err)
		return
	}

	routeflareAnns := extractRouteflareAnnotations(annotations)
	if len(routeflareAnns) == 0 {
		return
	}

	// Get record name
	route, ok := obj.(*unstructured.Unstructured)
	if !ok {
		slogs.Logr.Error("could not convert deleted object to unstructured", "error", err)
		return
	}

	recordName, err := getRecordNameFromHTTPRoute(route)
	if err != nil {
		slogs.Logr.Error("getting record name from deleted HTTPRoute",
			"route", fmt.Sprintf("%s/%s", namespace, name),
			"error", err)
		return
	}

	zoneName, err := extractZoneFromRecordName(recordName)
	if err != nil {
		slogs.Logr.Error("extracting zone from record name", "name", recordName, "error", err)
		return
	}

	recordType := routeflareAnns["type"]
	if recordType == "" {
		recordType = "A"
	}

	// Get zone ID
	zoneID, err := c.cfClient.GetZoneIDByName(zoneName)
	if err != nil {
		slogs.Logr.Error("getting zone ID", "name", zoneName, "error", err)
		return
	}

	// Delete DNS records
	if recordType == "A/AAAA" {
		// Delete both A and AAAA records
		for _, rt := range []string{"A", "AAAA"} {
			record, err := c.cfClient.FindRecord(c.ctx, zoneID, recordName, cloudflare.RecordType(rt))
			if err != nil {
				slogs.Logr.Error("finding record to delete", "type", rt, "name", recordName, "error", err)
				continue
			}
			if record != nil {
				if err := c.cfClient.DeleteRecord(c.ctx, zoneID, record.ID); err != nil {
					slogs.Logr.Error("deleting record", "type", rt, "name", recordName, "error", err)
					continue
				}
				slogs.Logr.Info("deleted record successfully", "type", rt, "name", recordName)
			}
		}
	} else {
		record, err := c.cfClient.FindRecord(c.ctx, zoneID, recordName, cloudflare.RecordType(recordType))
		if err != nil {
			slogs.Logr.Error("finding record to delete", "type", recordType, "name", recordName, "error", err)
			return
		}
		if record != nil {
			if err := c.cfClient.DeleteRecord(c.ctx, zoneID, record.ID); err != nil {
				slogs.Logr.Error("deleting record", "type", recordType, "name", recordName, "error", err)
			} else {
				slogs.Logr.Info("deleted record successfully", "type", recordType, "name", recordName)
			}
		}
	}

	// Remove from tracked routes if present
	routeKey := fmt.Sprintf("%s/%s", namespace, name)
	c.routesMutex.Lock()
	delete(c.trackedRoutes, routeKey)
	c.routesMutex.Unlock()
}

// runReconciliationJob runs a background job to reconcile all tracked routes
// This ensures DNS records stay in sync even if manually changed in Cloudflare
func (c *Controller) runReconciliationJob() {
	ticker := time.NewTicker(c.reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			// Use informer cache to get all HTTPRoutes
			informer := c.k8sClient.GetHTTPRouteInformer()
			routes := informer.GetStore().List()

			// Build a map of routes from cache for quick lookup
			cacheRoutes := make(map[string]*unstructured.Unstructured)
			for _, obj := range routes {
				if route, ok := obj.(*unstructured.Unstructured); ok {
					routeKey := fmt.Sprintf("%s/%s", route.GetNamespace(), route.GetName())
					cacheRoutes[routeKey] = route
				}
			}

			c.routesMutex.RLock()
			trackedRoutes := make([]*trackedRoute, 0, len(c.trackedRoutes))
			for _, route := range c.trackedRoutes {
				trackedRoutes = append(trackedRoutes, route)
			}
			c.routesMutex.RUnlock()

			for _, trackedRoute := range trackedRoutes {
				routeKey := fmt.Sprintf("%s/%s", trackedRoute.namespace, trackedRoute.name)
				route, exists := cacheRoutes[routeKey]

				if !exists {
					// Route no longer exists in cache, remove from tracking
					slogs.Logr.Info("HTTPRoute no longer exists, removing from tracking",
						"route", routeKey)
					c.routesMutex.Lock()
					delete(c.trackedRoutes, routeKey)
					c.routesMutex.Unlock()
					continue
				}

				switch trackedRoute.contentMode {
				case "ddns":
					// For DDNS, check if public IPs have changed
					c.processHTTPRoute(route, true)
				case "gateway-address":
					// For gateway-address, reconcile out state drift
					c.processHTTPRoute(route, true)
				default:
					slogs.Logr.Warn("Unknown content mode during reconciliation",
						"route", routeKey,
						"contentMode", trackedRoute.contentMode)
				}
			}
		}
	}
}

// Helper funcs

// getRecordNameFromHTTPRoute gets the first hostname from an HTTPRoute
// TODO does this need to support multiple hostnames? For now, just one seems fine
func getRecordNameFromHTTPRoute(route *unstructured.Unstructured) (string, error) {
	hostnames, found, err := unstructured.NestedStringSlice(route.Object, "spec", "hostnames")
	if !found || err != nil || len(hostnames) == 0 {
		return "", fmt.Errorf("HTTPRoute has no hostnames in spec")
	}
	return hostnames[0], nil
}
