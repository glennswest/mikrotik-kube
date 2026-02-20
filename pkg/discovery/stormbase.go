package discovery

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/lifecycle"
	"github.com/glennswest/mkube/pkg/stormbase"
)

// DeviceInventoryEntry represents a discovered device for scheduling purposes.
type DeviceInventoryEntry struct {
	ID         string
	Class      string
	Vendor     string
	State      string
	BoundTo    string
	IommuGroup string
}

// DiscoverStormBase queries a stormd node via gRPC and builds an inventory
// of running workloads, system resources, and devices.
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

	// Discover devices and build labels for scheduling affinity
	devices, err := client.ListDevices(ctx)
	if err != nil {
		log.Warnw("failed to list devices", "error", err)
	} else {
		inv.DeviceLabels = buildDeviceLabels(devices)
		log.Infow("device discovery complete", "devices", len(devices), "labels", len(inv.DeviceLabels))
	}

	log.Infow("stormbase discovery complete", "workloads", len(discovered))
	return inv, nil
}

// buildDeviceLabels counts available devices per class and returns labels
// like "nvme.stormblock.io/count=4", "gpu.nvidia.com/count=1".
func buildDeviceLabels(devices []stormbase.DeviceInfo) map[string]string {
	classCounts := make(map[string]int)

	for _, d := range devices {
		if d.State != "available" {
			continue
		}
		// Map known PCI classes to device class labels
		switch {
		case d.Class == "0108":
			classCounts["nvme.stormblock.io"]++
		case (d.Class == "0300" || d.Class == "0302") && d.Vendor == "10de":
			classCounts["gpu.nvidia.com"]++
		case (d.Class == "0300" || d.Class == "0302") && d.Vendor == "1002":
			classCounts["gpu.amd.com"]++
		}
	}

	labels := make(map[string]string, len(classCounts))
	for class, count := range classCounts {
		labels[fmt.Sprintf("%s/count", class)] = fmt.Sprintf("%d", count)
	}
	return labels
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
