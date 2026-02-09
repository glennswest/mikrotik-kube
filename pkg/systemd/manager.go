package systemd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/glenneth/mikrotik-kube/pkg/config"
	"github.com/glenneth/mikrotik-kube/pkg/routeros"
)

// Manager implements systemd-like functionality for RouterOS containers:
//   - Boot ordering: containers start in priority order on boot
//   - Health checks: HTTP and TCP probes
//   - Watchdog: periodic checks with auto-restart
//   - Restart policies: configurable backoff and max restarts
type Manager struct {
	cfg config.SystemdConfig
	ros *routeros.Client
	log *zap.SugaredLogger

	mu    sync.RWMutex
	units map[string]*ContainerUnit
}

// ContainerUnit represents a managed container with systemd-like properties.
type ContainerUnit struct {
	Name          string
	ContainerID   string
	RestartPolicy string // "Always", "OnFailure", "Never"
	HealthCheck   *HealthCheck
	DependsOn     []string // names of containers that must start first
	Priority      int      // lower = starts first (like systemd ordering)

	// Runtime state
	RestartCount    int
	LastRestartAt   time.Time
	LastHealthCheck time.Time
	Healthy         bool
	Status          string // "running", "stopped", "restarting", "failed"
}

// HealthCheck defines how to probe container health.
type HealthCheck struct {
	Type     string // "http", "tcp"
	Path     string // HTTP path (for type=http)
	Port     int
	Interval int    // seconds between checks
	Timeout  int    // seconds before check is considered failed
	Retries  int    // consecutive failures before marking unhealthy
}

// NewManager creates a new systemd-like manager.
func NewManager(cfg config.SystemdConfig, ros *routeros.Client, log *zap.SugaredLogger) *Manager {
	return &Manager{
		cfg:   cfg,
		ros:   ros,
		log:   log,
		units: make(map[string]*ContainerUnit),
	}
}

// Register adds a container to the systemd manager.
func (m *Manager) Register(name string, unit ContainerUnit) {
	m.mu.Lock()
	defer m.mu.Unlock()

	unit.Healthy = true // assume healthy until proven otherwise
	unit.Status = "running"
	m.units[name] = &unit
	m.log.Infow("registered container unit", "name", name, "priority", unit.Priority)
}

// Unregister removes a container from the systemd manager.
func (m *Manager) Unregister(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.units, name)
	m.log.Infow("unregistered container unit", "name", name)
}

// ─── Boot Ordering ──────────────────────────────────────────────────────────

// BootSequence returns containers sorted by boot priority, respecting
// dependency ordering (topological sort with priority as tiebreaker).
func (m *Manager) BootSequence() []*ContainerUnit {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Collect all units
	units := make([]*ContainerUnit, 0, len(m.units))
	for _, u := range m.units {
		units = append(units, u)
	}

	// Topological sort with priority tiebreaker
	return topoSort(units, m.units)
}

// ExecuteBootSequence starts all registered containers in dependency order.
func (m *Manager) ExecuteBootSequence(ctx context.Context) error {
	sequence := m.BootSequence()
	m.log.Infow("executing boot sequence", "containers", len(sequence))

	for i, unit := range sequence {
		m.log.Infow("booting container",
			"order", i+1,
			"name", unit.Name,
			"priority", unit.Priority,
			"depends_on", unit.DependsOn,
		)

		if err := m.ros.StartContainer(ctx, unit.ContainerID); err != nil {
			m.log.Errorw("failed to start container during boot",
				"name", unit.Name, "error", err)

			// Continue booting others unless this is a hard dependency
			continue
		}

		// Wait for the container to be healthy before proceeding to dependents
		if unit.HealthCheck != nil {
			if err := m.waitForHealthy(ctx, unit, 30*time.Second); err != nil {
				m.log.Warnw("container not healthy after boot, continuing",
					"name", unit.Name, "error", err)
			}
		}

		// Small delay between starts to avoid overwhelming RouterOS
		time.Sleep(500 * time.Millisecond)
	}

	m.log.Info("boot sequence complete")
	return nil
}

// ─── Watchdog ───────────────────────────────────────────────────────────────

// RunWatchdog periodically checks container health and restarts unhealthy ones.
func (m *Manager) RunWatchdog(ctx context.Context) {
	interval := time.Duration(m.cfg.WatchdogInterval) * time.Second
	if interval == 0 {
		interval = 15 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	m.log.Infow("watchdog started", "interval", interval)

	for {
		select {
		case <-ctx.Done():
			m.log.Info("watchdog shutting down")
			return
		case <-ticker.C:
			m.checkAll(ctx)
		}
	}
}

func (m *Manager) checkAll(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for name, unit := range m.units {
		if unit.HealthCheck == nil {
			continue
		}

		healthy := m.performHealthCheck(ctx, unit)
		wasHealthy := unit.Healthy
		unit.Healthy = healthy
		unit.LastHealthCheck = time.Now()

		if wasHealthy && !healthy {
			m.log.Warnw("container became unhealthy", "name", name)
			m.handleUnhealthy(ctx, unit)
		} else if !wasHealthy && healthy {
			m.log.Infow("container recovered", "name", name)
			unit.Status = "running"
		}
	}
}

func (m *Manager) performHealthCheck(ctx context.Context, unit *ContainerUnit) bool {
	switch unit.HealthCheck.Type {
	case "http":
		return m.httpCheck(ctx, unit)
	case "tcp":
		return m.tcpCheck(ctx, unit)
	default:
		// If no valid check type, check container status via RouterOS
		return m.statusCheck(ctx, unit)
	}
}

func (m *Manager) httpCheck(ctx context.Context, unit *ContainerUnit) bool {
	timeout := time.Duration(unit.HealthCheck.Timeout) * time.Second
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	client := &http.Client{Timeout: timeout}
	url := fmt.Sprintf("http://localhost:%d%s", unit.HealthCheck.Port, unit.HealthCheck.Path)

	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

func (m *Manager) tcpCheck(ctx context.Context, unit *ContainerUnit) bool {
	timeout := time.Duration(unit.HealthCheck.Timeout) * time.Second
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	addr := fmt.Sprintf("localhost:%d", unit.HealthCheck.Port)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (m *Manager) statusCheck(ctx context.Context, unit *ContainerUnit) bool {
	ct, err := m.ros.GetContainer(ctx, unit.Name)
	if err != nil {
		return false
	}
	return ct.Status == "running"
}

// ─── Restart Handling ───────────────────────────────────────────────────────

func (m *Manager) handleUnhealthy(ctx context.Context, unit *ContainerUnit) {
	maxRestarts := m.cfg.MaxRestarts
	if maxRestarts == 0 {
		maxRestarts = 5
	}

	cooldown := time.Duration(m.cfg.RestartCooldown) * time.Second
	if cooldown == 0 {
		cooldown = 10 * time.Second
	}

	switch unit.RestartPolicy {
	case "Always", "OnFailure":
		if unit.RestartCount >= maxRestarts {
			m.log.Errorw("container exceeded max restarts, marking as failed",
				"name", unit.Name,
				"restarts", unit.RestartCount,
				"max", maxRestarts)
			unit.Status = "failed"
			return
		}

		// Check cooldown
		if time.Since(unit.LastRestartAt) < cooldown {
			m.log.Debugw("restart cooldown active", "name", unit.Name)
			return
		}

		m.log.Infow("restarting unhealthy container",
			"name", unit.Name,
			"attempt", unit.RestartCount+1)

		unit.Status = "restarting"

		// Stop then start
		_ = m.ros.StopContainer(ctx, unit.ContainerID)
		time.Sleep(2 * time.Second)

		if err := m.ros.StartContainer(ctx, unit.ContainerID); err != nil {
			m.log.Errorw("failed to restart container", "name", unit.Name, "error", err)
		}

		unit.RestartCount++
		unit.LastRestartAt = time.Now()

	case "Never":
		m.log.Infow("container unhealthy but restart policy is Never", "name", unit.Name)
		unit.Status = "stopped"
	}
}

// ─── Wait Helpers ───────────────────────────────────────────────────────────

func (m *Manager) waitForHealthy(ctx context.Context, unit *ContainerUnit, timeout time.Duration) error {
	deadline := time.After(timeout)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for %s to become healthy", unit.Name)
		case <-tick.C:
			if m.performHealthCheck(ctx, unit) {
				return nil
			}
		}
	}
}

// GetUnitStatus returns the status of all managed units.
func (m *Manager) GetUnitStatus() map[string]UnitStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]UnitStatus, len(m.units))
	for name, unit := range m.units {
		result[name] = UnitStatus{
			Name:          name,
			Status:        unit.Status,
			Healthy:       unit.Healthy,
			RestartCount:  unit.RestartCount,
			LastRestart:   unit.LastRestartAt,
			LastHealthCheck: unit.LastHealthCheck,
		}
	}
	return result
}

// UnitStatus is an exported snapshot of a unit's runtime state.
type UnitStatus struct {
	Name            string    `json:"name"`
	Status          string    `json:"status"`
	Healthy         bool      `json:"healthy"`
	RestartCount    int       `json:"restartCount"`
	LastRestart     time.Time `json:"lastRestart,omitempty"`
	LastHealthCheck time.Time `json:"lastHealthCheck,omitempty"`
}

// ─── Topological Sort ───────────────────────────────────────────────────────

func topoSort(units []*ContainerUnit, unitMap map[string]*ContainerUnit) []*ContainerUnit {
	// Sort by priority first
	sort.Slice(units, func(i, j int) bool {
		return units[i].Priority < units[j].Priority
	})

	visited := make(map[string]bool)
	var result []*ContainerUnit

	var visit func(u *ContainerUnit)
	visit = func(u *ContainerUnit) {
		if visited[u.Name] {
			return
		}
		visited[u.Name] = true

		// Visit dependencies first
		for _, dep := range u.DependsOn {
			if depUnit, ok := unitMap[dep]; ok {
				visit(depUnit)
			}
		}

		result = append(result, u)
	}

	for _, u := range units {
		visit(u)
	}

	return result
}
