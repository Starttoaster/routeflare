package controller

import (
	"fmt"
	"github.com/chia-network/go-modules/pkg/slogs"
	"github.com/starttoaster/routeflare/pkg/cloudflare"
	"github.com/starttoaster/routeflare/pkg/gateway"
	"github.com/starttoaster/routeflare/pkg/kubernetes"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"time"
)

// watchHTTPRoutes watches for HTTPRoute changes
func (c *Controller) watchHTTPRoutes() error {
	for {
		watcher, err := c.k8sClient.WatchHTTPRoutes(c.ctx)
		if err != nil {
			return fmt.Errorf("error watching HTTPRoutes: %w", err)
		}

		slogs.Logr.Info("HTTPRoute watcher started")

		for {
			done, err := c.watchHTTPRoutesEventLoop(watcher)
			if err != nil {
				slogs.Logr.Error("watcher error", "error", err)
				time.Sleep(5 * time.Second)
				break // breaks out of inner loop to attempt a reconnect
			}
			if done {
				return nil // breaks out of both loops if routeflare is shutting down
			}
		}
	}
}

func (c *Controller) watchHTTPRoutesEventLoop(watcher watch.Interface) (bool, error) {
	select {
	case <-c.ctx.Done():
		slogs.Logr.Info("Gracefully stopping HTTPRoute watcher")
		watcher.Stop()
		return true, nil
	case event, ok := <-watcher.ResultChan():
		if !ok {
			watcher.Stop()
			return false, fmt.Errorf("watch channel closed, will attempt to restart watcher")
		}

		switch event.Type {
		case watch.Added:
			// All HTTPRoutes will appear to be Added when the watcher first starts up
			if route, ok := event.Object.(*unstructured.Unstructured); ok {
				slogs.Logr.Info("HTTPRoute added", "route", fmt.Sprintf("%s/%s", route.GetNamespace(), route.GetName()))
				c.processHTTPRoute(route, false)
			}
		case watch.Modified:
			if route, ok := event.Object.(*unstructured.Unstructured); ok {
				slogs.Logr.Info("HTTPRoute modified", "route", fmt.Sprintf("%s/%s", route.GetNamespace(), route.GetName()))
				c.processHTTPRoute(route, false)
			}
		case watch.Deleted:
			slogs.Logr.Info("HTTPRoute deleted", "route", fmt.Sprintf("%s/%s", getNamespace(event.Object), getName(event.Object)))
			c.processHTTPRouteDeletion(event.Object)
		case watch.Error:
			slogs.Logr.Info("Watch error", "error", event.Object)
		}
	}
	return false, nil
}

// processHTTPRoute processes a single HTTPRoute
func (c *Controller) processHTTPRoute(route *unstructured.Unstructured, isDDNSUpdate bool) {
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
		c.processGatewayAddressMode(route, zoneName, recordName, recordType, ttl, proxied)
	case "ddns":
		c.processDDNSMode(route, namespace, name, zoneName, recordName, recordType, ttl, proxied, isDDNSUpdate)
	default:
		slogs.Logr.Warn("Unknown content-mode for HTTPRoute", "route", fmt.Sprintf("%s/%s", namespace, name))
	}
}

// processGatewayAddressMode processes HTTPRoute with gateway-address content mode
func (c *Controller) processGatewayAddressMode(route *unstructured.Unstructured, zoneName, recordName, recordType string, ttl int, proxied bool) {
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
		return
	}
}

// processDDNSMode processes HTTPRoute with ddns content mode
func (c *Controller) processDDNSMode(route *unstructured.Unstructured, namespace, name, zoneName, recordName, recordType string, ttl int, proxied bool, isDDNSUpdate bool) {
	// Get current public IPs
	ips, err := c.ddnsDetector.GetPublicIPsByType(c.ctx, recordType)
	if err != nil {
		slogs.Logr.Error("getting public IPs for HTTPRoute",
			"route", fmt.Sprintf("%s/%s", namespace, name),
			"error", err)
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
		slogs.Logr.Error("getting zone ID from name", "zone-name", zoneName, "error", err)
		return
	}

	// Create/update DNS records
	err = c.createOrUpdateRecords(recordType, zoneID, ips, recordName, ttl, proxied)
	if err != nil {
		slogs.Logr.Error("creating or updating records", "error", err)
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
				} else {
					slogs.Logr.Info("deleted record successfully", "type", rt, "name", recordName)
				}
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

	// Remove from DDNS routes if present
	routeKey := fmt.Sprintf("%s/%s", namespace, name)
	c.ddnsMutex.Lock()
	delete(c.ddnsRoutes, routeKey)
	c.ddnsMutex.Unlock()
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
					slogs.Logr.Error("getting HTTPRoute for DDNS update (may have been deleted)",
						"route", fmt.Sprintf("%s/%s", ddnsRoute.namespace, ddnsRoute.name),
						"error", err)
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
