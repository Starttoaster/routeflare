package ddns

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	ipv4ServiceURL = "https://api.ipify.org"
	ipv6ServiceURL = "https://api64.ipify.org"
	timeout        = 10 * time.Second
)

// Detector detects public IP addresses
type Detector struct {
	httpClient *http.Client
}

// NewDetector creates a new IP detector
func NewDetector() *Detector {
	return &Detector{
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// GetPublicIPv4 gets the current public IPv4 address
func (d *Detector) GetPublicIPv4(ctx context.Context) (string, error) {
	return d.getPublicIP(ctx, ipv4ServiceURL, "IPv4")
}

// GetPublicIPv6 gets the current public IPv6 address
func (d *Detector) GetPublicIPv6(ctx context.Context) (string, error) {
	return d.getPublicIP(ctx, ipv6ServiceURL, "IPv6")
}

// GetPublicIPs gets both IPv4 and IPv6 addresses
func (d *Detector) GetPublicIPs(ctx context.Context) ([]string, error) {
	var ips []string

	// Try to get IPv4
	ipv4, err := d.GetPublicIPv4(ctx)
	if err == nil {
		ips = append(ips, ipv4)
	}

	// Try to get IPv6
	ipv6, err := d.GetPublicIPv6(ctx)
	if err == nil {
		ips = append(ips, ipv6)
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("could not detect any public IP addresses")
	}

	return ips, nil
}

// GetPublicIPsByType gets public IP addresses based on record type
func (d *Detector) GetPublicIPsByType(ctx context.Context, recordType string) ([]string, error) {
	switch recordType {
	case "A":
		ip, err := d.GetPublicIPv4(ctx)
		if err != nil {
			return nil, err
		}
		return []string{ip}, nil
	case "AAAA":
		ip, err := d.GetPublicIPv6(ctx)
		if err != nil {
			return nil, err
		}
		return []string{ip}, nil
	case "A/AAAA":
		return d.GetPublicIPs(ctx)
	default:
		return nil, fmt.Errorf("unsupported record type: %s", recordType)
	}
}

// getPublicIP gets a public IP from a service URL
func (d *Detector) getPublicIP(ctx context.Context, url, ipType string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("error creating request: %w", err)
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("error getting %s address: %w", ipType, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response: %w", err)
	}

	ipStr := string(body)
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return "", fmt.Errorf("invalid IP address received: %s", ipStr)
	}

	// Validate IP type
	if ipType == "IPv4" && ip.To4() == nil {
		return "", fmt.Errorf("expected IPv4 but got IPv6: %s", ipStr)
	}
	if ipType == "IPv6" && ip.To4() != nil {
		return "", fmt.Errorf("expected IPv6 but got IPv4: %s", ipStr)
	}

	return ipStr, nil
}

