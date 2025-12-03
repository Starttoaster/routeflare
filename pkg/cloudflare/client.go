package cloudflare

import (
	"context"
	"fmt"
	"github.com/chia-network/go-modules/pkg/slogs"
	"strconv"

	"github.com/cloudflare/cloudflare-go"
)

// Client wraps the official Cloudflare Go client
type Client struct {
	api *cloudflare.API
}

// NewClient creates a new Cloudflare API client
func NewClient(apiToken string) (*Client, error) {
	api, err := cloudflare.NewWithAPIToken(apiToken)
	if err != nil {
		return nil, fmt.Errorf("error creating Cloudflare client: %w", err)
	}

	return &Client{
		api: api,
	}, nil
}

// RecordType represents a DNS record type
type RecordType string

const (
	// RecordTypeA represents the identifier for an A record
	RecordTypeA RecordType = "A"
	// RecordTypeAAAA represents the identifier for an AAAA record
	RecordTypeAAAA RecordType = "AAAA"
)

// DNSRecord represents a Cloudflare DNS record
type DNSRecord struct {
	ID      string
	Type    RecordType
	Name    string
	Content string
	TTL     int // 1 = auto, or seconds
	Proxied bool
}

// GetZoneIDByName finds a zone ID by its name
func (c *Client) GetZoneIDByName(zoneName string) (string, error) {
	zoneID, err := c.api.ZoneIDByName(zoneName)
	if err != nil {
		return "", fmt.Errorf("error getting zone ID for %s: %w", zoneName, err)
	}
	return zoneID, nil
}

// FindRecord finds a DNS record by zone, name, and type
func (c *Client) FindRecord(ctx context.Context, zoneID, recordName string, recordType RecordType) (*DNSRecord, error) {
	records, _, err := c.api.ListDNSRecords(ctx, cloudflare.ZoneIdentifier(zoneID), cloudflare.ListDNSRecordsParams{
		Name: recordName,
		Type: string(recordType),
	})
	if err != nil {
		return nil, fmt.Errorf("error listing DNS records: %w", err)
	}

	if len(records) == 0 {
		return nil, nil // Record not found
	}

	// Return the first matching record
	record := records[0]
	return &DNSRecord{
		ID:      record.ID,
		Type:    RecordType(record.Type),
		Name:    record.Name,
		Content: record.Content,
		TTL:     record.TTL,
		Proxied: record.Proxied != nil && *record.Proxied,
	}, nil
}

// CreateRecord creates a new DNS record
func (c *Client) CreateRecord(ctx context.Context, zoneID string, record DNSRecord) (*DNSRecord, error) {
	cfRecord := cloudflare.CreateDNSRecordParams{
		Type:    string(record.Type),
		Name:    record.Name,
		Content: record.Content,
		TTL:     record.TTL,
	}

	proxied := record.Proxied
	cfRecord.Proxied = &proxied

	created, err := c.api.CreateDNSRecord(ctx, cloudflare.ZoneIdentifier(zoneID), cfRecord)
	if err != nil {
		return nil, err
	}

	slogs.Logr.Info("Successfully created record",
		"type", cfRecord.Type,
		"name", cfRecord.Name,
		"ip", cfRecord.Content,
		"ttl", cfRecord.TTL,
		"proxied", record.Proxied)

	return &DNSRecord{
		ID:      created.ID,
		Type:    RecordType(created.Type),
		Name:    created.Name,
		Content: created.Content,
		TTL:     created.TTL,
		Proxied: created.Proxied != nil && *created.Proxied,
	}, nil
}

// UpdateRecord updates an existing DNS record
func (c *Client) UpdateRecord(ctx context.Context, zoneID string, currentRecord DNSRecord, record DNSRecord) (*DNSRecord, error) {
	// Check if all record fields are already up to date before updating
	record.ID = currentRecord.ID
	if currentRecord == record {
		return &record, nil
	}

	// Assemble update record params and make the request
	proxied := record.Proxied
	cfRecord := cloudflare.UpdateDNSRecordParams{
		ID:      record.ID,
		Type:    string(record.Type),
		Name:    record.Name,
		Content: record.Content,
		TTL:     record.TTL,
		Proxied: &proxied,
	}

	updated, err := c.api.UpdateDNSRecord(ctx, cloudflare.ZoneIdentifier(zoneID), cfRecord)
	if err != nil {
		return nil, err
	}

	slogs.Logr.Info("Successfully updated record",
		"type", cfRecord.Type,
		"name", cfRecord.Name,
		"ip", cfRecord.Content,
		"ttl", cfRecord.TTL,
		"proxied", record.Proxied)

	return &DNSRecord{
		ID:      updated.ID,
		Type:    RecordType(updated.Type),
		Name:    updated.Name,
		Content: updated.Content,
		TTL:     updated.TTL,
		Proxied: updated.Proxied != nil && *updated.Proxied,
	}, nil
}

// DeleteRecord deletes a DNS record
func (c *Client) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	err := c.api.DeleteDNSRecord(ctx, cloudflare.ZoneIdentifier(zoneID), recordID)
	if err != nil {
		return fmt.Errorf("error deleting DNS record: %w", err)
	}
	return nil
}

// UpsertRecord creates or updates a DNS record
func (c *Client) UpsertRecord(ctx context.Context, zoneID string, record DNSRecord) (*DNSRecord, error) {
	existing, err := c.FindRecord(ctx, zoneID, record.Name, record.Type)
	if err != nil {
		return nil, fmt.Errorf("error finding record: %w", err)
	}

	if existing != nil {
		// Update existing record
		return c.UpdateRecord(ctx, zoneID, *existing, record)
	}

	// Create new record
	return c.CreateRecord(ctx, zoneID, record)
}

// ParseTTL parses TTL string to int (1 for auto, or seconds)
func ParseTTL(ttlStr string) (int, error) {
	if ttlStr == "" || ttlStr == "auto" {
		return 1, nil // Auto TTL
	}

	ttl, err := strconv.Atoi(ttlStr)
	if err != nil {
		return 0, fmt.Errorf("invalid TTL: %s", ttlStr)
	}

	if ttl < 1 {
		return 1, nil // Default to 1 second if TTL is invalid
	}

	return ttl, nil
}

// ParseProxied parses proxied string to bool
func ParseProxied(proxiedStr string) (bool, error) {
	if proxiedStr == "" {
		return false, nil
	}

	proxied, err := strconv.ParseBool(proxiedStr)
	if err != nil {
		return false, fmt.Errorf("invalid proxied value: %s (must be 'true' or 'false')", proxiedStr)
	}

	return proxied, nil
}
