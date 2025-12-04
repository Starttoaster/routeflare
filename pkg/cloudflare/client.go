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
	Comment string // Owner/comment field for tracking record ownership
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
		Comment: record.Comment,
	}, nil
}

// createRecord creates a new DNS record
// If this is made to be a public function in the future, it should check for ownership in the same way that UpsertRecord does
func (c *Client) createRecord(ctx context.Context, zoneID string, record DNSRecord) (*DNSRecord, error) {
	cfRecord := cloudflare.CreateDNSRecordParams{
		Type:    string(record.Type),
		Name:    record.Name,
		Content: record.Content,
		TTL:     record.TTL,
		Comment: record.Comment,
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
		"proxied", record.Proxied,
		"comment", created.Comment)

	return &DNSRecord{
		ID:      created.ID,
		Type:    RecordType(created.Type),
		Name:    created.Name,
		Content: created.Content,
		TTL:     created.TTL,
		Proxied: created.Proxied != nil && *created.Proxied,
		Comment: created.Comment,
	}, nil
}

// updateRecord updates an existing DNS record
// If this is made to be a public function in the future, it should check for ownership in the same way that UpsertRecord does
func (c *Client) updateRecord(ctx context.Context, zoneID string, currentRecord DNSRecord, record DNSRecord) (*DNSRecord, error) {
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
		Comment: &record.Comment,
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
		"proxied", record.Proxied,
		"comment", updated.Comment)

	return &DNSRecord{
		ID:      updated.ID,
		Type:    RecordType(updated.Type),
		Name:    updated.Name,
		Content: updated.Content,
		TTL:     updated.TTL,
		Proxied: updated.Proxied != nil && *updated.Proxied,
		Comment: updated.Comment,
	}, nil
}

// DeleteRecord deletes a DNS record
func (c *Client) DeleteRecord(ctx context.Context, zoneID string, record DNSRecord) error {
	existing, err := c.FindRecord(ctx, zoneID, record.Name, record.Type)
	if err != nil {
		return fmt.Errorf("error finding record: %w", err)
	}

	if existing != nil {
		// Check ownership
		if existing.Comment != "" && existing.Comment != record.Comment {
			return fmt.Errorf("record ownership conflict: existing owner '%s' does not match expected owner '%s'", existing.Comment, record.Comment)
		}

		// Delete existing record
		err = c.api.DeleteDNSRecord(ctx, cloudflare.ZoneIdentifier(zoneID), existing.ID)
		if err != nil {
			return fmt.Errorf("error deleting DNS record: %w", err)
		}
	}

	return nil
}

// UpsertRecord creates or updates a DNS record with ownership checking
// If the record exists and has a different owner, it returns an error
// If the record exists with no owner, it updates the record with the new owner
func (c *Client) UpsertRecord(ctx context.Context, zoneID string, record DNSRecord) (*DNSRecord, error) {
	existing, err := c.FindRecord(ctx, zoneID, record.Name, record.Type)
	if err != nil {
		return nil, fmt.Errorf("error finding record: %w", err)
	}

	if existing != nil {
		// Check ownership
		if existing.Comment != "" && existing.Comment != record.Comment {
			return nil, fmt.Errorf("record ownership conflict: existing owner '%s' does not match expected owner '%s'", existing.Comment, record.Comment)
		}

		// Update existing record
		return c.updateRecord(ctx, zoneID, *existing, record)
	}

	// Create new record with owner
	return c.createRecord(ctx, zoneID, record)
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
