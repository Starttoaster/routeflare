package gateway

import (
	"fmt"
	"net"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// GetGatewayAddresses extracts IP addresses from a Gateway's status.addresses
func GetGatewayAddresses(gateway *unstructured.Unstructured, recordType string) ([]string, error) {
	status, found, err := unstructured.NestedMap(gateway.Object, "status")
	if !found || err != nil {
		return nil, fmt.Errorf("gateway has no status or error accessing it: %w", err)
	}

	addresses, found, err := unstructured.NestedSlice(status, "addresses")
	if !found || err != nil {
		return nil, fmt.Errorf("gateway has no status.addresses or error accessing it: %w", err)
	}

	var ipv4Addrs []string
	var ipv6Addrs []string

	for _, addrInterface := range addresses {
		addrMap, ok := addrInterface.(map[string]interface{})
		if !ok {
			continue
		}

		addrValue, found, err := unstructured.NestedString(addrMap, "value")
		if !found || err != nil {
			continue
		}

		// Check if it's a valid IP address
		ip := net.ParseIP(addrValue)
		if ip == nil {
			continue
		}

		if ip.To4() != nil {
			ipv4Addrs = append(ipv4Addrs, addrValue)
		} else {
			ipv6Addrs = append(ipv6Addrs, addrValue)
		}
	}

	switch recordType {
	case "A":
		if len(ipv4Addrs) == 0 {
			return nil, fmt.Errorf("no IPv4 addresses found in gateway status.addresses")
		}
		return []string{ipv4Addrs[0]}, nil
	case "AAAA":
		if len(ipv6Addrs) == 0 {
			return nil, fmt.Errorf("no IPv6 addresses found in gateway status.addresses")
		}
		return []string{ipv6Addrs[0]}, nil
	case "A/AAAA":
		var result []string
		if len(ipv4Addrs) > 0 {
			result = append(result, ipv4Addrs[0])
		}
		if len(ipv6Addrs) > 0 {
			result = append(result, ipv6Addrs[0])
		}
		if len(result) == 0 {
			return nil, fmt.Errorf("no IP addresses found in gateway status.addresses")
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported record type: %s", recordType)
	}
}
