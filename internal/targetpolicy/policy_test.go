package targetpolicy

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestValidateURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "public IPv4", url: "https://8.8.8.8/webhook"},
		{name: "public IPv6", url: "https://[2606:4700:4700::1111]/webhook"},
		{name: "localhost", url: "http://localhost/webhook", wantErr: true},
		{name: "localhost subdomain", url: "http://api.localhost/webhook", wantErr: true},
		{name: "IPv4 loopback", url: "http://127.0.0.1/webhook", wantErr: true},
		{name: "IPv6 loopback", url: "http://[::1]/webhook", wantErr: true},
		{name: "IPv4-mapped loopback", url: "http://[::ffff:127.0.0.1]/webhook", wantErr: true},
		{name: "private 10", url: "http://10.1.2.3/webhook", wantErr: true},
		{name: "private 172", url: "http://172.16.2.3/webhook", wantErr: true},
		{name: "private 192", url: "http://192.168.2.3/webhook", wantErr: true},
		{name: "metadata link local", url: "http://169.254.169.254/latest", wantErr: true},
		{name: "IPv6 link local", url: "http://[fe80::1]/webhook", wantErr: true},
		{name: "IPv6 private", url: "http://[fd00::1]/webhook", wantErr: true},
		{name: "unspecified", url: "http://0.0.0.0/webhook", wantErr: true},
		{name: "multicast", url: "http://224.0.0.1/webhook", wantErr: true},
		{name: "unsupported scheme", url: "ftp://8.8.8.8/file", wantErr: true},
		{name: "malformed", url: "://not-a-url", wantErr: true},
		{name: "credentials", url: "https://user:pass@example.com/webhook", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateURL(test.url)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateURL(%q) error = %v, wantErr %v", test.url, err, test.wantErr)
			}
		})
	}
}

func TestSafeDialerResolvesOnceAndDialsValidatedIP(t *testing.T) {
	resolver := &fakeResolver{addresses: map[string][]net.IPAddr{
		"public.example": {{IP: net.ParseIP("93.184.216.34")}},
	}}
	dialer := &fakeDialer{err: errors.New("stop after address capture")}
	safeDialer := NewSafeDialer(resolver, dialer)

	_, err := safeDialer.DialContext(context.Background(), "tcp", "public.example:443")
	if err == nil {
		t.Fatal("DialContext() error = nil, want fake dial error")
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
	if dialer.calls != 1 || dialer.address != "93.184.216.34:443" {
		t.Fatalf("dialer calls/address = %d/%q, want 1/public IP", dialer.calls, dialer.address)
	}
}

func TestSafeDialerBlocksResolvedNonPublicAddresses(t *testing.T) {
	tests := []struct {
		name      string
		addresses []net.IPAddr
	}{
		{name: "private", addresses: []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}},
		{name: "loopback", addresses: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}},
		{name: "link local", addresses: []net.IPAddr{{IP: net.ParseIP("169.254.1.1")}}},
		{name: "mixed public and private", addresses: []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("192.168.1.1")}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := &fakeResolver{addresses: map[string][]net.IPAddr{"target.example": test.addresses}}
			dialer := &fakeDialer{}
			_, err := NewSafeDialer(resolver, dialer).DialContext(context.Background(), "tcp", "target.example:80")
			if !errors.Is(err, ErrBlockedDestination) {
				t.Fatalf("DialContext() error = %v, want ErrBlockedDestination", err)
			}
			if dialer.calls != 0 {
				t.Fatalf("underlying dialer called %d times for blocked destination", dialer.calls)
			}
		})
	}
}

type fakeResolver struct {
	addresses map[string][]net.IPAddr
	calls     int
}

func (r *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.calls++
	return r.addresses[host], nil
}

type fakeDialer struct {
	calls   int
	address string
	err     error
}

func (d *fakeDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.calls++
	d.address = address
	return nil, d.err
}
