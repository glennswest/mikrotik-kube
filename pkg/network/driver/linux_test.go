//go:build linux

package driver

import (
	"testing"

	"github.com/glennswest/mkube/pkg/network"
	"go.uber.org/zap"
)

func TestLinuxCapabilities(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Linux driver test in short mode")
	}

	log := zap.NewNop().Sugar()
	d := NewLinux("linux-test", log)

	caps := d.Capabilities()
	if !caps.VLANs {
		t.Error("Linux driver should support VLANs")
	}
	if !caps.Tunnels {
		t.Error("Linux driver should support Tunnels")
	}
	if caps.ACLs {
		t.Error("Linux driver should not support ACLs")
	}

	if d.NodeName() != "linux-test" {
		t.Errorf("expected node name 'linux-test', got %q", d.NodeName())
	}

	// Verify it satisfies the interface
	var _ network.NetworkDriver = d
}
