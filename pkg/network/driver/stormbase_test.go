package driver

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/network"
	"github.com/glennswest/mkube/pkg/stormbase"
)

func TestStormBaseCapabilities(t *testing.T) {
	// Use insecure client pointing nowhere — we only test Capabilities and NodeName
	// which don't make gRPC calls.
	client, err := stormbase.NewClient(stormbase.ClientConfig{
		Address:  "localhost:0",
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	defer client.Close()

	log := zap.NewNop().Sugar()
	d := NewStormBase(client, "storm-node-1", log)

	caps := d.Capabilities()
	if caps.VLANs {
		t.Error("StormBase should not support VLANs")
	}
	if !caps.Tunnels {
		t.Error("StormBase should support Tunnels (WireGuard)")
	}
	if !caps.ACLs {
		t.Error("StormBase should support ACLs (eBPF)")
	}

	if d.NodeName() != "storm-node-1" {
		t.Errorf("expected node name 'storm-node-1', got %q", d.NodeName())
	}
}

func TestStormBaseVLANNotSupported(t *testing.T) {
	client, err := stormbase.NewClient(stormbase.ClientConfig{
		Address:  "localhost:0",
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	defer client.Close()

	log := zap.NewNop().Sugar()
	d := NewStormBase(client, "storm-node-1", log)

	ctx := context.Background()

	if err := d.SetPortVLAN(ctx, "eth0", 100, true); err != network.ErrNotSupported {
		t.Errorf("SetPortVLAN should return ErrNotSupported, got %v", err)
	}
	if err := d.RemovePortVLAN(ctx, "eth0", 100); err != network.ErrNotSupported {
		t.Errorf("RemovePortVLAN should return ErrNotSupported, got %v", err)
	}
}

func TestStormBaseBridgeNoOps(t *testing.T) {
	client, err := stormbase.NewClient(stormbase.ClientConfig{
		Address:  "localhost:0",
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	defer client.Close()

	log := zap.NewNop().Sugar()
	d := NewStormBase(client, "storm-node-1", log)

	ctx := context.Background()

	// Bridge operations are no-ops on StormBase (BPF routing)
	if err := d.CreateBridge(ctx, "br0", network.BridgeOpts{}); err != nil {
		t.Errorf("CreateBridge should be no-op, got %v", err)
	}
	if err := d.DeleteBridge(ctx, "br0"); err != nil {
		t.Errorf("DeleteBridge should be no-op, got %v", err)
	}

	bridges, err := d.ListBridges(ctx)
	if err != nil {
		t.Errorf("ListBridges should return empty, got %v", err)
	}
	if len(bridges) != 0 {
		t.Errorf("expected 0 bridges, got %d", len(bridges))
	}
}

func TestStormBasePortCreateDelete(t *testing.T) {
	client, err := stormbase.NewClient(stormbase.ClientConfig{
		Address:  "localhost:0",
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	defer client.Close()

	log := zap.NewNop().Sugar()
	d := NewStormBase(client, "storm-node-1", log)

	ctx := context.Background()

	// Port create/delete are record-keeping ops (stormd creates veths internally)
	if err := d.CreatePort(ctx, "veth-test", "10.42.0.2/24", "10.42.0.1"); err != nil {
		t.Errorf("CreatePort should succeed (no-op), got %v", err)
	}
	if err := d.DeletePort(ctx, "veth-test"); err != nil {
		t.Errorf("DeletePort should succeed (no-op), got %v", err)
	}
	if err := d.AttachPort(ctx, "br0", "veth-test"); err != nil {
		t.Errorf("AttachPort should be no-op, got %v", err)
	}
	if err := d.DetachPort(ctx, "br0", "veth-test"); err != nil {
		t.Errorf("DetachPort should be no-op, got %v", err)
	}
}
