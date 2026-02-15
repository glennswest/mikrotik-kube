package topology

import (
	"fmt"
	"sync"

	"github.com/glennswest/microkube/pkg/network"
)

// Node represents a device in the topology that has a NetworkDriver.
type Node struct {
	Name         string
	Address      string // management IP/hostname
	Driver       network.NetworkDriver
	Capabilities network.DriverCapabilities
}

// Topology tracks multiple network nodes and their drivers.
type Topology struct {
	mu    sync.RWMutex
	nodes map[string]*Node
}

// New returns an empty Topology.
func New() *Topology {
	return &Topology{
		nodes: make(map[string]*Node),
	}
}

// AddNode registers a node. Returns error if the name is already taken.
func (t *Topology) AddNode(n Node) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if _, exists := t.nodes[n.Name]; exists {
		return fmt.Errorf("node %q already registered", n.Name)
	}
	t.nodes[n.Name] = &n
	return nil
}

// RemoveNode unregisters a node by name.
func (t *Topology) RemoveNode(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.nodes, name)
}

// GetNode returns a node by name.
func (t *Topology) GetNode(name string) (*Node, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	n, ok := t.nodes[name]
	if !ok {
		return nil, fmt.Errorf("node %q not found", name)
	}
	return n, nil
}

// ListNodes returns all registered nodes.
func (t *Topology) ListNodes() []Node {
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := make([]Node, 0, len(t.nodes))
	for _, n := range t.nodes {
		out = append(out, *n)
	}
	return out
}

// NodeCount returns the number of registered nodes.
func (t *Topology) NodeCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.nodes)
}

// DriverFor returns the NetworkDriver for the named node.
func (t *Topology) DriverFor(name string) (network.NetworkDriver, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	n, ok := t.nodes[name]
	if !ok {
		return nil, fmt.Errorf("node %q not found", name)
	}
	return n.Driver, nil
}

// NodesWithCapability returns nodes that support the given capability.
func (t *Topology) NodesWithCapability(cap string) []Node {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var out []Node
	for _, n := range t.nodes {
		switch cap {
		case "vlans":
			if n.Capabilities.VLANs {
				out = append(out, *n)
			}
		case "tunnels":
			if n.Capabilities.Tunnels {
				out = append(out, *n)
			}
		case "acls":
			if n.Capabilities.ACLs {
				out = append(out, *n)
			}
		}
	}
	return out
}
