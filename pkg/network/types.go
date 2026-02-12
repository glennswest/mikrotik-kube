package network

// LogicalSwitch represents a network segment (maps to a physical bridge on a node).
type LogicalSwitch struct {
	Name    string            `json:"name" yaml:"name"`       // e.g. "gt", "g10"
	Bridge  string            `json:"bridge" yaml:"bridge"`   // physical bridge name
	CIDR    string            `json:"cidr" yaml:"cidr"`
	Gateway string            `json:"gateway" yaml:"gateway"`
	VLANs   []int             `json:"vlans,omitempty" yaml:"vlans,omitempty"`
	Labels  map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
}

// LogicalPort represents a port on a logical switch (maps to a veth on a node).
type LogicalPort struct {
	Name     string `json:"name" yaml:"name"`         // veth name
	Switch   string `json:"switch" yaml:"switch"`     // parent LogicalSwitch name
	Address  string `json:"address" yaml:"address"`   // assigned IP/mask
	Gateway  string `json:"gateway" yaml:"gateway"`
	Hostname string `json:"hostname" yaml:"hostname"`
	VLANTag  int    `json:"vlanTag,omitempty" yaml:"vlanTag,omitempty"`
	NodeName string `json:"nodeName" yaml:"nodeName"` // which node owns this port
}

// LogicalRouter represents a router connecting logical switches.
type LogicalRouter struct {
	Name       string            `json:"name" yaml:"name"`
	Interfaces []RouterInterface `json:"interfaces" yaml:"interfaces"`
}

// RouterInterface is a logical router's connection to a switch.
type RouterInterface struct {
	Switch  string `json:"switch" yaml:"switch"`
	Address string `json:"address" yaml:"address"` // IP on that switch
}

// Tunnel represents a cross-node tunnel (VXLAN, WireGuard, GRE).
type Tunnel struct {
	Name       string `json:"name" yaml:"name"`
	Type       string `json:"type" yaml:"type"`             // "vxlan", "wireguard", "gre"
	LocalNode  string `json:"localNode" yaml:"localNode"`
	RemoteNode string `json:"remoteNode" yaml:"remoteNode"`
	LocalIP    string `json:"localIP" yaml:"localIP"`
	RemoteIP   string `json:"remoteIP" yaml:"remoteIP"`
	VNI        int    `json:"vni,omitempty" yaml:"vni,omitempty"`
}
