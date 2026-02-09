package network

import (
	"net"
	"testing"

	"github.com/glenneth/mikrotik-kube/pkg/config"
)

func TestIPConversion(t *testing.T) {
	tests := []struct {
		ip   string
		want uint32
	}{
		{"0.0.0.0", 0},
		{"0.0.0.1", 1},
		{"192.168.1.1", 3232235777},
		{"255.255.255.255", 4294967295},
	}

	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		got := ipToUint32(ip)
		if got != tt.want {
			t.Errorf("ipToUint32(%s) = %d, want %d", tt.ip, got, tt.want)
		}

		back := uint32ToIP(tt.want)
		if !back.Equal(ip.To4()) {
			t.Errorf("uint32ToIP(%d) = %s, want %s", tt.want, back, tt.ip)
		}
	}
}

func TestAllocateIP(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("172.20.0.0/24")
	gateway := net.ParseIP("172.20.0.1")

	mgr := &Manager{
		cfg:       config.NetworkConfig{PodCIDR: "172.20.0.0/24"},
		subnet:    subnet,
		gateway:   gateway,
		allocated: make(map[string]net.IP),
		nextIP:    2,
	}

	// First allocation should be .2
	ip1, err := mgr.allocateIP()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip1.String() != "172.20.0.2" {
		t.Errorf("expected 172.20.0.2, got %s", ip1)
	}
	mgr.allocated["veth-0"] = ip1

	// Second should be .3
	ip2, err := mgr.allocateIP()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip2.String() != "172.20.0.3" {
		t.Errorf("expected 172.20.0.3, got %s", ip2)
	}
	mgr.allocated["veth-1"] = ip2

	// Should skip gateway (.1) on wrap-around
	mgr.nextIP = 1
	ip3, err := mgr.allocateIP()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip3.Equal(gateway) {
		t.Error("allocated gateway IP, should have been skipped")
	}
}

func TestAllocateIPExhaustion(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("172.20.0.0/30") // only 2 hosts (.1 and .2)
	gateway := net.ParseIP("172.20.0.1")

	mgr := &Manager{
		cfg:       config.NetworkConfig{PodCIDR: "172.20.0.0/30"},
		subnet:    subnet,
		gateway:   gateway,
		allocated: make(map[string]net.IP),
		nextIP:    2,
	}

	// Allocate .2 (the only usable address since .1 is gateway)
	ip, err := mgr.allocateIP()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mgr.allocated["veth-0"] = ip

	// Should fail — pool exhausted
	_, err = mgr.allocateIP()
	if err == nil {
		t.Error("expected exhaustion error")
	}
}

func TestGetAllocations(t *testing.T) {
	mgr := &Manager{
		allocated: map[string]net.IP{
			"veth-a": net.ParseIP("172.20.0.2"),
			"veth-b": net.ParseIP("172.20.0.3"),
		},
	}

	allocs := mgr.GetAllocations()
	if len(allocs) != 2 {
		t.Fatalf("expected 2 allocations, got %d", len(allocs))
	}
	if allocs["veth-a"] != "172.20.0.2" {
		t.Errorf("expected 172.20.0.2, got %s", allocs["veth-a"])
	}
}
