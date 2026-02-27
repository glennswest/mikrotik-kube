package provider

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	// infraHealthThreshold is the number of consecutive health check failures
	// before a container is restarted.
	infraHealthThreshold = 3

	// infraHealthTimeout is the HTTP timeout for health check requests.
	infraHealthTimeout = 5 * time.Second

	// infraRestartCooldown prevents rapid restart loops.
	infraRestartCooldown = 60 * time.Second
)

// infraContainer describes a non-pod infrastructure container that should be health-checked.
type infraContainer struct {
	Name      string // RouterOS container name
	HealthURL string // HTTP health check URL (e.g. http://192.168.200.3:5001/healthz)
}

// infraLastRestart tracks when each container was last restarted.
var infraLastRestart = make(map[string]time.Time)

// checkInfraHealth checks health of critical infrastructure containers (registry, mkube-update)
// and restarts them if they become unresponsive. Called from the reconcile loop.
func (p *MicroKubeProvider) checkInfraHealth(ctx context.Context) {
	log := p.deps.Logger

	containers := p.getInfraContainers()
	if len(containers) == 0 {
		return
	}

	client := &http.Client{Timeout: infraHealthTimeout}

	for _, ic := range containers {
		healthy := false

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ic.HealthURL, nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				healthy = resp.StatusCode == http.StatusOK
				resp.Body.Close()
			}
		}

		if healthy {
			if p.infraFailures[ic.Name] > 0 {
				log.Infow("infrastructure container recovered", "container", ic.Name)
			}
			p.infraFailures[ic.Name] = 0
			continue
		}

		p.infraFailures[ic.Name]++
		log.Warnw("infrastructure container health check failed",
			"container", ic.Name,
			"failures", p.infraFailures[ic.Name],
			"threshold", infraHealthThreshold,
		)

		if p.infraFailures[ic.Name] >= infraHealthThreshold {
			// Check cooldown
			if last, ok := infraLastRestart[ic.Name]; ok && time.Since(last) < infraRestartCooldown {
				log.Warnw("infrastructure container restart skipped (cooldown)",
					"container", ic.Name,
					"lastRestart", last.Format(time.RFC3339),
				)
				continue
			}

			log.Warnw("restarting unresponsive infrastructure container",
				"container", ic.Name,
				"consecutiveFailures", p.infraFailures[ic.Name],
			)

			if err := p.restartInfraContainer(ctx, ic.Name); err != nil {
				log.Errorw("failed to restart infrastructure container",
					"container", ic.Name,
					"error", err,
				)
			} else {
				infraLastRestart[ic.Name] = time.Now()
				p.infraFailures[ic.Name] = 0
				log.Infow("infrastructure container restarted", "container", ic.Name)
			}
		}
	}
}

// getInfraContainers returns the list of infrastructure containers to health-check.
func (p *MicroKubeProvider) getInfraContainers() []infraContainer {
	var containers []infraContainer

	// Registry: derive IP from config's LocalAddresses
	if len(p.deps.Config.Registry.LocalAddresses) > 0 {
		addr := p.deps.Config.Registry.LocalAddresses[0]
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		// Management API is on :5001 (HTTP, not HTTPS)
		containers = append(containers, infraContainer{
			Name:      "registry.gt.lo",
			HealthURL: fmt.Sprintf("http://%s:5001/healthz", host),
		})
	}

	return containers
}

// restartInfraContainer stops and starts a container by name.
func (p *MicroKubeProvider) restartInfraContainer(ctx context.Context, name string) error {
	// Find the container first
	ct, err := p.deps.Runtime.GetContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("finding container %s: %w", name, err)
	}
	if ct == nil {
		return fmt.Errorf("container %s not found", name)
	}

	// Stop if running
	if strings.EqualFold(ct.Status, "running") {
		if err := p.deps.Runtime.StopContainer(ctx, ct.ID); err != nil {
			return fmt.Errorf("stopping container %s: %w", name, err)
		}
		// Wait for stop
		time.Sleep(3 * time.Second)
	}

	// Start
	if err := p.deps.Runtime.StartContainer(ctx, ct.ID); err != nil {
		return fmt.Errorf("starting container %s: %w", name, err)
	}

	return nil
}
