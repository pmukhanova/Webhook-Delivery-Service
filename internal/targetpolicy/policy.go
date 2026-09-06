package targetpolicy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

var ErrBlockedDestination = errors.New("target destination is not public")

type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type Dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

type SafeDialer struct {
	resolver Resolver
	dialer   Dialer
}

func NewSafeDialer(resolver Resolver, dialer Dialer) *SafeDialer {
	return &SafeDialer{resolver: resolver, dialer: dialer}
}

func ValidateURL(rawURL string) error {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || parsed.Host == "" {
		return errors.New("target URL is malformed")
	}
	if scheme := strings.ToLower(parsed.Scheme); scheme != "http" && scheme != "https" {
		return errors.New("target URL scheme must be http or https")
	}
	if parsed.User != nil {
		return errors.New("target URL must not contain credentials")
	}
	host := parsed.Hostname()
	if host == "" || isLocalhost(host) {
		return ErrBlockedDestination
	}
	if ip := parseIP(host); ip != nil && IsBlockedIP(ip) {
		return ErrBlockedDestination
	}
	return nil
}

func (d *SafeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("split target address: %w", err)
	}
	if isLocalhost(host) {
		return nil, ErrBlockedDestination
	}

	if ip := parseIP(host); ip != nil {
		if IsBlockedIP(ip) {
			return nil, ErrBlockedDestination
		}
		return d.dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}

	addresses, err := d.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve target host: %w", err)
	}
	if len(addresses) == 0 {
		return nil, errors.New("target host resolved to no addresses")
	}
	for _, address := range addresses {
		if IsBlockedIP(address.IP) {
			return nil, ErrBlockedDestination
		}
	}
	var lastDialError error
	for _, address := range addresses {
		if !matchesNetwork(network, address.IP) {
			continue
		}
		host := address.IP.String()
		if address.Zone != "" {
			host += "%" + address.Zone
		}
		connection, err := d.dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
		if err == nil {
			return connection, nil
		}
		lastDialError = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if lastDialError != nil {
		return nil, fmt.Errorf("dial target: %w", lastDialError)
	}
	return nil, errors.New("target host has no address for requested network")
}

func IsBlockedIP(ip net.IP) bool {
	return ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

func isLocalhost(host string) bool {
	normalized := strings.TrimSuffix(strings.ToLower(host), ".")
	return normalized == "localhost" || strings.HasSuffix(normalized, ".localhost")
}

func parseIP(host string) net.IP {
	if zoneIndex := strings.LastIndexByte(host, '%'); zoneIndex >= 0 {
		host = host[:zoneIndex]
	}
	return net.ParseIP(host)
}

func matchesNetwork(network string, ip net.IP) bool {
	switch network {
	case "tcp4":
		return ip.To4() != nil
	case "tcp6":
		return ip.To4() == nil
	default:
		return true
	}
}
