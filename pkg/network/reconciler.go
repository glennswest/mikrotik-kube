package network

import (
	"context"
	"time"
)

// ReconcilerOpts configures the reconciliation loop.
type ReconcilerOpts struct {
	Interval time.Duration // how often to reconcile (default 30s)
}

// RunReconciler starts a periodic loop that compares desired state (logical
// switches/ports stored in the state store) against actual state (queried from
// the network driver) and converges differences.
//
// Runs until ctx is cancelled.
func (m *Manager) RunReconciler(ctx context.Context, opts ReconcilerOpts) {
	interval := opts.Interval
	if interval == 0 {
		interval = 30 * time.Second
	}

	m.log.Infow("network reconciler started", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.log.Info("network reconciler stopped")
			return
		case <-ticker.C:
			m.reconcile(ctx)
		}
	}
}

func (m *Manager) reconcile(ctx context.Context) {
	log := m.log.Named("reconciler")

	// Get actual ports from the driver
	actualPorts, err := m.driver.ListPorts(ctx)
	if err != nil {
		log.Warnw("failed to list actual ports", "error", err)
		return
	}

	// Build lookup of actual ports
	actual := make(map[string]PortInfo, len(actualPorts))
	for _, p := range actualPorts {
		actual[p.Name] = p
	}

	// Get desired ports from state
	state := m.state.get()
	if state == nil {
		return
	}

	drifts := 0

	// Check for desired ports that are missing
	for name, desired := range state.Ports {
		if _, exists := actual[name]; !exists {
			log.Warnw("drift: desired port missing from device",
				"port", name,
				"switch", desired.Switch,
				"address", desired.Address,
			)
			drifts++

			// Attempt to recreate
			if err := m.driver.CreatePort(ctx, name, desired.Address, desired.Gateway); err != nil {
				log.Warnw("failed to recreate drifted port", "port", name, "error", err)
				continue
			}

			// Re-attach to bridge
			ns, ok := m.networks[desired.Switch]
			if ok {
				if err := m.driver.AttachPort(ctx, ns.def.Bridge, name); err != nil {
					log.Warnw("failed to re-attach drifted port", "port", name, "bridge", ns.def.Bridge, "error", err)
				}
			}
		}
	}

	if drifts > 0 {
		log.Infow("reconciliation complete", "drifts_detected", drifts)
	} else {
		log.Debugw("reconciliation complete, no drift")
	}
}
