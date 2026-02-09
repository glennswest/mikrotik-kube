package network

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	"go.uber.org/zap"

	"github.com/glenneth/mikrotik-kube/pkg/config"
	"github.com/glenneth/mikrotik-kube/pkg/routeros"
)

// Manager handles IP address allocation (IPAM), veth interface creation,
// and bridge port management for containers on RouterOS.
type Manager struct {
	cfg    config.NetworkConfig
	ros    *routeros.Client
	log    *zap.SugaredLogger

	mu        sync.Mutex
	subnet    *net.IPNet
	gateway   net.IP
	allocated map[string]net.IP // veth name -> allocated IP
	nextIP    uint32            // next IP to try (host-order offset)
}

// NewManager initializes the network manager and ensures the bridge exists.
func NewManager(cfg config.NetworkConfig, ros *routeros.Client, log *zap.SugaredLogger) (*Manager, error) {
	_, subnet, err := net.ParseCIDR(cfg.PodCIDR)
	if err != nil {
		return nil, fmt.Errorf("parsing pod CIDR %q: %w", cfg.PodCIDR, err)
	}

	// Gateway is .1 in the subnet by default
	gateway := make(net.IP, len(subnet.IP))
	copy(gateway, subnet.IP)
	gateway[len(gateway)-1] = 1

	if cfg.GatewayIP != "" {
		gateway = net.ParseIP(cfg.GatewayIP)
		if gateway == nil {
			return nil, fmt.Errorf("invalid gateway IP: %s", cfg.GatewayIP)
		}
	}

	mgr := &Manager{
		cfg:       cfg,
		ros:       ros,
		log:       log,
		subnet:    subnet,
		gateway:   gateway,
		allocated: make(map[string]net.IP),
		nextIP:    2, // start at .2 (skip network and gateway)
	}

	// Sync with existing veth allocations on RouterOS
	if err := mgr.syncExistingAllocations(context.Background()); err != nil {
		log.Warnw("failed to sync existing allocations", "error", err)
	}

	return mgr, nil
}

// AllocateInterface creates a veth, assigns an IP from the pool, and
// adds it to the container bridge. Returns the IP and gateway.
func (m *Manager) AllocateInterface(ctx context.Context, vethName string) (ip string, gateway string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Allocate an IP
	allocatedIP, err := m.allocateIP()
	if err != nil {
		return "", "", err
	}

	// Determine the prefix length from the subnet
	ones, _ := m.subnet.Mask.Size()
	ipCIDR := fmt.Sprintf("%s/%d", allocatedIP.String(), ones)
	gw := m.gateway.String()

	// Create veth on RouterOS
	if err := m.ros.CreateVeth(ctx, vethName, ipCIDR, gw); err != nil {
		return "", "", fmt.Errorf("creating veth %s: %w", vethName, err)
	}

	// Add to bridge
	if err := m.ros.AddBridgePort(ctx, m.cfg.BridgeName, vethName); err != nil {
		// Rollback veth
		_ = m.ros.RemoveVeth(ctx, vethName)
		return "", "", fmt.Errorf("adding %s to bridge %s: %w", vethName, m.cfg.BridgeName, err)
	}

	m.allocated[vethName] = allocatedIP
	m.log.Infow("interface allocated", "veth", vethName, "ip", ipCIDR, "bridge", m.cfg.BridgeName)

	return ipCIDR, gw, nil
}

// ReleaseInterface removes the veth and returns its IP to the pool.
func (m *Manager) ReleaseInterface(ctx context.Context, vethName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ros.RemoveVeth(ctx, vethName); err != nil {
		m.log.Warnw("error removing veth", "name", vethName, "error", err)
	}

	delete(m.allocated, vethName)
	m.log.Infow("interface released", "veth", vethName)

	return nil
}

// GetAllocations returns a snapshot of current IP allocations.
func (m *Manager) GetAllocations() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[string]string, len(m.allocated))
	for name, ip := range m.allocated {
		result[name] = ip.String()
	}
	return result
}

// ─── IPAM ───────────────────────────────────────────────────────────────────

// allocateIP finds the next available IP in the subnet.
// Must be called with m.mu held.
func (m *Manager) allocateIP() (net.IP, error) {
	ones, bits := m.subnet.Mask.Size()
	maxHosts := uint32(1<<(bits-ones)) - 2 // exclude network and broadcast

	baseIP := ipToUint32(m.subnet.IP)

	// Try sequential allocation, wrapping around if needed
	for attempts := uint32(0); attempts < maxHosts; attempts++ {
		candidate := baseIP + m.nextIP
		candidateIP := uint32ToIP(candidate)

		// Check not already allocated
		taken := false
		for _, existing := range m.allocated {
			if existing.Equal(candidateIP) {
				taken = true
				break
			}
		}

		m.nextIP++
		if m.nextIP > maxHosts {
			m.nextIP = 2 // wrap around
		}

		if !taken && !candidateIP.Equal(m.gateway) {
			return candidateIP, nil
		}
	}

	return nil, fmt.Errorf("IPAM: no available IPs in %s (all %d addresses allocated)", m.subnet.String(), maxHosts)
}

// syncExistingAllocations reads current veth interfaces from RouterOS
// and populates the allocation map, so we don't double-allocate on restart.
func (m *Manager) syncExistingAllocations(ctx context.Context) error {
	veths, err := m.ros.ListVeths(ctx)
	if err != nil {
		return err
	}

	for _, v := range veths {
		if v.Address == "" {
			continue
		}
		ip, _, err := net.ParseCIDR(v.Address)
		if err != nil {
			ip = net.ParseIP(v.Address)
		}
		if ip != nil && m.subnet.Contains(ip) {
			m.allocated[v.Name] = ip
			m.log.Debugw("synced existing allocation", "veth", v.Name, "ip", ip)
		}
	}

	m.log.Infow("synced existing allocations", "count", len(m.allocated))
	return nil
}

// ─── IP Helpers ─────────────────────────────────────────────────────────────

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return binary.BigEndian.Uint32(ip)
}

func uint32ToIP(n uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, n)
	return ip
}
