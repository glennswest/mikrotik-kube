package driver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/glenneth/microkube/pkg/config"
	"github.com/glenneth/microkube/pkg/network"
	"github.com/glenneth/microkube/pkg/routeros"
)

// mockRouterOS creates an httptest.Server that simulates RouterOS REST API
// endpoints used by the driver.
func mockRouterOS(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	// GET /rest/interface/bridge — list bridges
	mux.HandleFunc("/rest/interface/bridge", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{".id": "*1", "name": "bridge"},
			{".id": "*2", "name": "containers"},
		})
	})

	// GET /rest/interface/veth — list veths
	mux.HandleFunc("/rest/interface/veth", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{".id": "*10", "name": "veth-app1", "address": "192.168.200.2/24", "gateway": "192.168.200.1"},
		})
	})

	// POST /rest/interface/veth/add — create veth
	mux.HandleFunc("/rest/interface/veth/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] == "" {
			http.Error(w, `{"error":"missing name"}`, http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})

	// POST /rest/interface/veth/remove — remove veth
	mux.HandleFunc("/rest/interface/veth/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	// POST /rest/interface/bridge/port/add — add bridge port
	mux.HandleFunc("/rest/interface/bridge/port/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})

	return httptest.NewServer(mux)
}

func newTestDriver(t *testing.T, serverURL string) *RouterOS {
	t.Helper()

	client, err := routeros.NewClient(config.RouterOSConfig{
		RESTURL:        serverURL + "/rest",
		User:           "admin",
		Password:       "test",
		InsecureVerify: true,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	log := zap.NewNop().Sugar()
	return NewRouterOS(client, "test-node", log)
}

func TestListBridges(t *testing.T) {
	srv := mockRouterOS(t)
	defer srv.Close()
	d := newTestDriver(t, srv.URL)

	bridges, err := d.ListBridges(context.Background())
	if err != nil {
		t.Fatalf("ListBridges: %v", err)
	}
	if len(bridges) != 2 {
		t.Fatalf("expected 2 bridges, got %d", len(bridges))
	}
	if bridges[0].Name != "bridge" {
		t.Errorf("expected first bridge 'bridge', got %q", bridges[0].Name)
	}
	if bridges[1].Name != "containers" {
		t.Errorf("expected second bridge 'containers', got %q", bridges[1].Name)
	}
}

func TestListPorts(t *testing.T) {
	srv := mockRouterOS(t)
	defer srv.Close()
	d := newTestDriver(t, srv.URL)

	ports, err := d.ListPorts(context.Background())
	if err != nil {
		t.Fatalf("ListPorts: %v", err)
	}
	if len(ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(ports))
	}
	if ports[0].Name != "veth-app1" {
		t.Errorf("expected port 'veth-app1', got %q", ports[0].Name)
	}
	if ports[0].Address != "192.168.200.2/24" {
		t.Errorf("expected address '192.168.200.2/24', got %q", ports[0].Address)
	}
}

func TestCreateAndDeletePort(t *testing.T) {
	srv := mockRouterOS(t)
	defer srv.Close()
	d := newTestDriver(t, srv.URL)

	ctx := context.Background()

	if err := d.CreatePort(ctx, "veth-test", "10.0.0.2/24", "10.0.0.1"); err != nil {
		t.Fatalf("CreatePort: %v", err)
	}

	if err := d.DeletePort(ctx, "veth-test"); err != nil {
		t.Fatalf("DeletePort: %v", err)
	}
}

func TestAttachPort(t *testing.T) {
	srv := mockRouterOS(t)
	defer srv.Close()
	d := newTestDriver(t, srv.URL)

	if err := d.AttachPort(context.Background(), "containers", "veth-test"); err != nil {
		t.Fatalf("AttachPort: %v", err)
	}
}

func TestDetachPortNoOp(t *testing.T) {
	srv := mockRouterOS(t)
	defer srv.Close()
	d := newTestDriver(t, srv.URL)

	// DetachPort is a no-op on RouterOS
	if err := d.DetachPort(context.Background(), "containers", "veth-test"); err != nil {
		t.Fatalf("DetachPort should be no-op: %v", err)
	}
}

func TestUnsupportedOperations(t *testing.T) {
	srv := mockRouterOS(t)
	defer srv.Close()
	d := newTestDriver(t, srv.URL)

	ctx := context.Background()

	if err := d.CreateBridge(ctx, "br0", network.BridgeOpts{}); err != network.ErrNotSupported {
		t.Errorf("CreateBridge should return ErrNotSupported, got %v", err)
	}
	if err := d.DeleteBridge(ctx, "br0"); err != network.ErrNotSupported {
		t.Errorf("DeleteBridge should return ErrNotSupported, got %v", err)
	}
	// CreateTunnel with unsupported type should fail
	if err := d.CreateTunnel(ctx, "tun0", network.TunnelSpec{Type: "vxlan"}); err == nil {
		t.Error("CreateTunnel with unsupported type should return error")
	}
}

func TestNodeNameAndCapabilities(t *testing.T) {
	srv := mockRouterOS(t)
	defer srv.Close()
	d := newTestDriver(t, srv.URL)

	if d.NodeName() != "test-node" {
		t.Errorf("expected node name 'test-node', got %q", d.NodeName())
	}

	caps := d.Capabilities()
	if !caps.VLANs {
		t.Error("RouterOS driver should support VLANs")
	}
	if !caps.Tunnels {
		t.Error("RouterOS driver should support Tunnels (EoIP)")
	}
	if caps.ACLs {
		t.Errorf("RouterOS driver should not support ACLs, got %+v", caps)
	}
}
