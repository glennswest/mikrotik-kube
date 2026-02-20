package discovery

import (
	"context"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/lifecycle"
	"github.com/glennswest/mkube/pkg/stormbase"
)

// DiscoverStormBase queries a stormd node via gRPC and builds an inventory
// of running workloads and system resources.
func DiscoverStormBase(ctx context.Context, client *stormbase.Client, log *zap.SugaredLogger) (*Inventory, error) {
	log.Info("starting stormbase discovery")

	containers, err := client.ListContainers(ctx)
	if err != nil {
		return nil, err
	}

	sysRes, err := client.GetSystemResource(ctx)
	if err != nil {
		log.Warnw("failed to get stormbase system resource", "error", err)
	}

	var discovered []Container
	for _, ct := range containers {
		dc := Container{
			Name:      ct.Name,
			Status:    ct.Status,
			IP:        ct.DNS,
			Interface: ct.Interface,
			StartBoot: ct.StartOnBoot == "true",
		}
		discovered = append(discovered, dc)
	}

	inv := &Inventory{
		Containers: discovered,
	}

	if sysRes != nil {
		// Map runtime.SystemResource back to routeros.SystemResource for inventory compat
		inv.System = nil // StormBase doesn't use routeros.SystemResource
	}

	log.Infow("stormbase discovery complete", "workloads", len(discovered))
	return inv, nil
}

// BuildLifecycleUnitsFromStormBase converts discovered stormbase workloads
// into lifecycle.ContainerUnit entries. On StormBase, stormd handles restarts
// internally, so these units are primarily for probe tracking.
func BuildLifecycleUnitsFromStormBase(inv *Inventory) []lifecycle.ContainerUnit {
	var units []lifecycle.ContainerUnit
	for _, ct := range inv.Containers {
		if ct.Status != "running" {
			continue
		}
		unit := lifecycle.ContainerUnit{
			Name:          ct.Name,
			ContainerIP:   ct.IP,
			RestartPolicy: "Always",
			StartOnBoot:   ct.StartBoot,
			Managed:       false,
		}
		units = append(units, unit)
	}
	return units
}
