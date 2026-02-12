package network

import (
	"encoding/json"
	"net/http"
	"strings"
)

// RegisterRoutes adds network API endpoints to the given mux.
//
//	GET /api/v1/networks          — list logical switches
//	GET /api/v1/networks/{name}   — get switch details + ports
//	GET /api/v1/networks/{name}/ports — list ports on a switch
//	GET /api/v1/allocations       — IPAM dump
func (m *Manager) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/networks", m.handleNetworks)
	mux.HandleFunc("/api/v1/networks/", m.handleNetworkDetail)
	mux.HandleFunc("/api/v1/allocations", m.handleAllocations)
}

func (m *Manager) handleNetworks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	type switchSummary struct {
		Name    string `json:"name"`
		Bridge  string `json:"bridge"`
		CIDR    string `json:"cidr"`
		Gateway string `json:"gateway"`
	}

	var out []switchSummary
	for _, name := range m.netOrder {
		ns := m.networks[name]
		out = append(out, switchSummary{
			Name:    ns.def.Name,
			Bridge:  ns.def.Bridge,
			CIDR:    ns.def.CIDR,
			Gateway: ns.gateway.String(),
		})
	}

	writeJSON(w, out)
}

func (m *Manager) handleNetworkDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse: /api/v1/networks/{name}  or  /api/v1/networks/{name}/ports
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/networks/")
	parts := strings.SplitN(path, "/", 2)
	name := parts[0]

	ns, ok := m.networks[name]
	if !ok {
		http.Error(w, "network not found", http.StatusNotFound)
		return
	}

	// /api/v1/networks/{name}/ports
	if len(parts) == 2 && parts[1] == "ports" {
		m.mu.Lock()
		defer m.mu.Unlock()

		type portInfo struct {
			Name     string `json:"name"`
			IP       string `json:"ip"`
			Hostname string `json:"hostname"`
		}

		var ports []portInfo
		for veth, alloc := range m.allocs {
			if alloc.networkName == name {
				ports = append(ports, portInfo{
					Name:     veth,
					IP:       alloc.ip.String(),
					Hostname: alloc.hostname,
				})
			}
		}
		writeJSON(w, ports)
		return
	}

	// /api/v1/networks/{name}
	m.mu.Lock()
	portCount := 0
	for _, alloc := range m.allocs {
		if alloc.networkName == name {
			portCount++
		}
	}
	m.mu.Unlock()

	type switchDetail struct {
		Name    string `json:"name"`
		Bridge  string `json:"bridge"`
		CIDR    string `json:"cidr"`
		Gateway string `json:"gateway"`
		DNS     string `json:"dnsZone,omitempty"`
		Ports   int    `json:"ports"`
	}

	writeJSON(w, switchDetail{
		Name:    ns.def.Name,
		Bridge:  ns.def.Bridge,
		CIDR:    ns.def.CIDR,
		Gateway: ns.gateway.String(),
		DNS:     ns.def.DNS.Zone,
		Ports:   portCount,
	})
}

func (m *Manager) handleAllocations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, m.GetAllocations())
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
