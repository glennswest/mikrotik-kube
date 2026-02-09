package systemd

import (
	"testing"

	"go.uber.org/zap"

	"github.com/glenneth/mikrotik-kube/pkg/config"
)

func testLogger() *zap.SugaredLogger {
	log, _ := zap.NewDevelopment()
	return log.Sugar()
}

func TestRegisterAndUnregister(t *testing.T) {
	mgr := NewManager(config.SystemdConfig{}, nil, testLogger())

	mgr.Register("test-container", ContainerUnit{
		Name:        "test-container",
		ContainerID: "*1",
		Priority:    10,
	})

	if len(mgr.units) != 1 {
		t.Fatalf("expected 1 unit, got %d", len(mgr.units))
	}
	if !mgr.units["test-container"].Healthy {
		t.Error("newly registered unit should be healthy")
	}
	if mgr.units["test-container"].Status != "running" {
		t.Errorf("expected status 'running', got %q", mgr.units["test-container"].Status)
	}

	mgr.Unregister("test-container")
	if len(mgr.units) != 0 {
		t.Errorf("expected 0 units after unregister, got %d", len(mgr.units))
	}
}

func TestBootSequencePriorityOrder(t *testing.T) {
	mgr := NewManager(config.SystemdConfig{}, nil, testLogger())

	mgr.Register("c-low", ContainerUnit{Name: "c-low", Priority: 5})
	mgr.Register("c-mid", ContainerUnit{Name: "c-mid", Priority: 20})
	mgr.Register("c-high", ContainerUnit{Name: "c-high", Priority: 10})

	seq := mgr.BootSequence()
	if len(seq) != 3 {
		t.Fatalf("expected 3 units, got %d", len(seq))
	}
	if seq[0].Name != "c-low" {
		t.Errorf("expected c-low first, got %s", seq[0].Name)
	}
	if seq[1].Name != "c-high" {
		t.Errorf("expected c-high second, got %s", seq[1].Name)
	}
	if seq[2].Name != "c-mid" {
		t.Errorf("expected c-mid third, got %s", seq[2].Name)
	}
}

func TestBootSequenceDependencies(t *testing.T) {
	mgr := NewManager(config.SystemdConfig{}, nil, testLogger())

	mgr.Register("database", ContainerUnit{Name: "database", Priority: 20})
	mgr.Register("app", ContainerUnit{Name: "app", Priority: 10, DependsOn: []string{"database"}})

	seq := mgr.BootSequence()
	if len(seq) != 2 {
		t.Fatalf("expected 2 units, got %d", len(seq))
	}
	// database should come first despite higher priority number,
	// because app depends on it
	if seq[0].Name != "database" {
		t.Errorf("expected database first (dependency), got %s", seq[0].Name)
	}
	if seq[1].Name != "app" {
		t.Errorf("expected app second, got %s", seq[1].Name)
	}
}

func TestGetUnitStatus(t *testing.T) {
	mgr := NewManager(config.SystemdConfig{}, nil, testLogger())
	mgr.Register("a", ContainerUnit{Name: "a", Priority: 10})
	mgr.Register("b", ContainerUnit{Name: "b", Priority: 20})

	statuses := mgr.GetUnitStatus()
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}
	if statuses["a"].Status != "running" {
		t.Errorf("expected status 'running', got %q", statuses["a"].Status)
	}
}

func TestTopoSortNoDeps(t *testing.T) {
	unitMap := map[string]*ContainerUnit{
		"a": {Name: "a", Priority: 30},
		"b": {Name: "b", Priority: 10},
		"c": {Name: "c", Priority: 20},
	}
	units := []*ContainerUnit{unitMap["a"], unitMap["b"], unitMap["c"]}

	result := topoSort(units, unitMap)
	if result[0].Name != "b" {
		t.Errorf("expected b first (lowest priority), got %s", result[0].Name)
	}
	if result[1].Name != "c" {
		t.Errorf("expected c second, got %s", result[1].Name)
	}
	if result[2].Name != "a" {
		t.Errorf("expected a third, got %s", result[2].Name)
	}
}

func TestTopoSortWithDeps(t *testing.T) {
	unitMap := map[string]*ContainerUnit{
		"dns":        {Name: "dns", Priority: 10},
		"monitoring": {Name: "monitoring", Priority: 20, DependsOn: []string{"dns"}},
		"vpn":        {Name: "vpn", Priority: 5},
	}
	units := []*ContainerUnit{unitMap["dns"], unitMap["monitoring"], unitMap["vpn"]}

	result := topoSort(units, unitMap)
	// vpn (5) should be first, then dns (10), then monitoring (20, depends on dns)
	if result[0].Name != "vpn" {
		t.Errorf("expected vpn first, got %s", result[0].Name)
	}
	if result[1].Name != "dns" {
		t.Errorf("expected dns second, got %s", result[1].Name)
	}
	if result[2].Name != "monitoring" {
		t.Errorf("expected monitoring third, got %s", result[2].Name)
	}
}

func TestHandleUnhealthyMaxRestarts(t *testing.T) {
	mgr := NewManager(config.SystemdConfig{
		MaxRestarts: 3,
	}, nil, testLogger())

	unit := &ContainerUnit{
		Name:          "failing",
		RestartPolicy: "Always",
		RestartCount:  3,
	}

	mgr.handleUnhealthy(nil, unit)
	if unit.Status != "failed" {
		t.Errorf("expected status 'failed', got %q", unit.Status)
	}
}

func TestHandleUnhealthyNeverRestart(t *testing.T) {
	mgr := NewManager(config.SystemdConfig{}, nil, testLogger())

	unit := &ContainerUnit{
		Name:          "no-restart",
		RestartPolicy: "Never",
	}

	mgr.handleUnhealthy(nil, unit)
	if unit.Status != "stopped" {
		t.Errorf("expected status 'stopped', got %q", unit.Status)
	}
}
