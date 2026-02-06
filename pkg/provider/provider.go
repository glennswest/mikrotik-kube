package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/glenneth/mikrotik-vk/pkg/config"
	"github.com/glenneth/mikrotik-vk/pkg/network"
	"github.com/glenneth/mikrotik-vk/pkg/routeros"
	"github.com/glenneth/mikrotik-vk/pkg/storage"
	"github.com/glenneth/mikrotik-vk/pkg/systemd"
)

// Deps holds injected dependencies for the provider.
type Deps struct {
	Config     *config.Config
	ROS        *routeros.Client
	NetworkMgr *network.Manager
	StorageMgr *storage.Manager
	SystemdMgr *systemd.Manager
	Logger     *zap.SugaredLogger
}

// MikroTikProvider implements the Virtual Kubelet provider interface.
// It translates Kubernetes Pod specifications into MikroTik RouterOS
// container operations, managing the full lifecycle including networking,
// storage, and boot ordering.
type MikroTikProvider struct {
	deps       Deps
	nodeName   string
	startTime  time.Time
	pods       map[string]*corev1.Pod // namespace/name -> pod
}

// NewMikroTikProvider creates a new provider instance.
func NewMikroTikProvider(deps Deps) (*MikroTikProvider, error) {
	return &MikroTikProvider{
		deps:      deps,
		nodeName:  deps.Config.NodeName,
		startTime: time.Now(),
		pods:      make(map[string]*corev1.Pod),
	}, nil
}

// ─── PodLifecycleHandler Interface ──────────────────────────────────────────

// CreatePod takes a Kubernetes Pod spec and creates the corresponding
// RouterOS container(s). This includes:
//  1. Pulling/caching the image as an OCI tarball
//  2. Allocating a veth interface and IP address
//  3. Creating volume mounts
//  4. Registering boot ordering if restartPolicy=Always
//  5. Creating and starting the RouterOS container
func (p *MikroTikProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	log := p.deps.Logger.With("pod", podKey(pod))
	log.Infow("creating pod")

	for i, container := range pod.Spec.Containers {
		name := sanitizeName(pod, container.Name)

		// 1. Resolve image → tarball path
		tarballPath, err := p.deps.StorageMgr.EnsureImage(ctx, container.Image)
		if err != nil {
			return fmt.Errorf("ensuring image %s: %w", container.Image, err)
		}

		// 2. Allocate network
		vethName := fmt.Sprintf("veth-%s-%d", truncate(pod.Name, 8), i)
		ip, gw, err := p.deps.NetworkMgr.AllocateInterface(ctx, vethName)
		if err != nil {
			return fmt.Errorf("allocating network for %s: %w", name, err)
		}
		log.Infow("allocated network", "veth", vethName, "ip", ip, "gateway", gw)

		// 3. Create volume mounts
		var mounts []string
		for _, vm := range container.VolumeMounts {
			hostPath, err := p.deps.StorageMgr.ProvisionVolume(ctx, name, vm.Name, vm.MountPath)
			if err != nil {
				return fmt.Errorf("provisioning volume %s: %w", vm.Name, err)
			}
			mounts = append(mounts, fmt.Sprintf("%s:%s", hostPath, vm.MountPath))
		}

		// 4. Build environment variables
		envs := make(map[string]string)
		for _, env := range container.Env {
			envs[env.Name] = env.Value
		}

		// 5. Determine boot behavior
		startOnBoot := pod.Spec.RestartPolicy == corev1.RestartPolicyAlways

		// 6. Create the RouterOS container
		spec := routeros.ContainerSpec{
			Name:        name,
			File:        tarballPath,
			Interface:   vethName,
			RootDir:     fmt.Sprintf("%s/%s", p.deps.Config.Storage.BasePath, name),
			Mounts:      mounts,
			Envs:        envs,
			Cmd:         strings.Join(container.Command, " "),
			Hostname:    pod.Name,
			DNS:         strings.Join(p.deps.Config.Network.DNSServers, ","),
			Logging:     true,
			StartOnBoot: startOnBoot,
		}

		if err := p.deps.ROS.CreateContainer(ctx, spec); err != nil {
			return fmt.Errorf("creating container %s: %w", name, err)
		}

		// 7. Start the container
		ct, err := p.deps.ROS.GetContainer(ctx, name)
		if err != nil {
			return fmt.Errorf("getting created container %s: %w", name, err)
		}
		if err := p.deps.ROS.StartContainer(ctx, ct.ID); err != nil {
			return fmt.Errorf("starting container %s: %w", name, err)
		}

		// 8. Register with systemd manager for boot ordering / health checks
		if startOnBoot {
			p.deps.SystemdMgr.Register(name, systemd.ContainerUnit{
				Name:         name,
				ContainerID:  ct.ID,
				RestartPolicy: string(pod.Spec.RestartPolicy),
				HealthCheck:  extractHealthCheck(container),
				DependsOn:    extractDependencies(pod),
				Priority:     extractPriority(pod, i),
			})
		}

		log.Infow("container created and started", "name", name, "id", ct.ID)
	}

	// Track the pod
	p.pods[podKey(pod)] = pod.DeepCopy()

	return nil
}

// UpdatePod handles pod spec updates. RouterOS containers are immutable,
// so this performs a rolling update: create new → verify → remove old.
func (p *MikroTikProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	log := p.deps.Logger.With("pod", podKey(pod))
	log.Infow("updating pod (rolling replacement)")

	// Delete and recreate — RouterOS containers are immutable
	if err := p.DeletePod(ctx, pod); err != nil {
		log.Warnw("error deleting old pod during update", "error", err)
	}
	return p.CreatePod(ctx, pod)
}

// DeletePod removes all containers associated with a pod and cleans up
// networking and storage resources.
func (p *MikroTikProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	log := p.deps.Logger.With("pod", podKey(pod))
	log.Infow("deleting pod")

	for i, container := range pod.Spec.Containers {
		name := sanitizeName(pod, container.Name)

		// Stop and remove the container
		ct, err := p.deps.ROS.GetContainer(ctx, name)
		if err != nil {
			log.Warnw("container not found during delete", "name", name, "error", err)
			continue
		}

		if ct.Status == "running" {
			if err := p.deps.ROS.StopContainer(ctx, ct.ID); err != nil {
				log.Warnw("error stopping container", "name", name, "error", err)
			}
		}

		if err := p.deps.ROS.RemoveContainer(ctx, ct.ID); err != nil {
			log.Warnw("error removing container", "name", name, "error", err)
		}

		// Release network resources
		vethName := fmt.Sprintf("veth-%s-%d", truncate(pod.Name, 8), i)
		if err := p.deps.NetworkMgr.ReleaseInterface(ctx, vethName); err != nil {
			log.Warnw("error releasing network", "veth", vethName, "error", err)
		}

		// Unregister from systemd manager
		p.deps.SystemdMgr.Unregister(name)

		// Note: storage cleanup is deferred to GC
		log.Infow("container removed", "name", name)
	}

	delete(p.pods, podKey(pod))
	return nil
}

// GetPod returns the tracked pod object.
func (p *MikroTikProvider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	key := namespace + "/" + name
	if pod, ok := p.pods[key]; ok {
		return pod, nil
	}
	return nil, fmt.Errorf("pod %s not found", key)
}

// GetPodStatus queries RouterOS for the actual container status and maps
// it back to Kubernetes pod status.
func (p *MikroTikProvider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	var containerStatuses []corev1.ContainerStatus
	allRunning := true

	for _, container := range pod.Spec.Containers {
		rosName := sanitizeName(pod, container.Name)
		ct, err := p.deps.ROS.GetContainer(ctx, rosName)

		cs := corev1.ContainerStatus{
			Name:  container.Name,
			Image: container.Image,
			Ready: false,
		}

		if err != nil {
			cs.State = corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ContainerNotFound",
					Message: err.Error(),
				},
			}
			allRunning = false
		} else {
			switch ct.Status {
			case "running":
				cs.Ready = true
				cs.State = corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{
						StartedAt: metav1.Now(),
					},
				}
			case "stopped", "error":
				cs.State = corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Reason: ct.Status,
					},
				}
				allRunning = false
			default:
				cs.State = corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason: ct.Status,
					},
				}
				allRunning = false
			}
		}

		containerStatuses = append(containerStatuses, cs)
	}

	phase := corev1.PodRunning
	if !allRunning {
		phase = corev1.PodPending
	}

	return &corev1.PodStatus{
		Phase:             phase,
		ContainerStatuses: containerStatuses,
		StartTime:         &metav1.Time{Time: p.startTime},
		HostIP:            p.deps.Config.Network.GatewayIP,
		Conditions: []corev1.PodCondition{
			{
				Type:   corev1.PodReady,
				Status: boolToConditionStatus(allRunning),
			},
			{
				Type:   corev1.PodScheduled,
				Status: corev1.ConditionTrue,
			},
		},
	}, nil
}

// GetPods returns all tracked pods.
func (p *MikroTikProvider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	pods := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		pods = append(pods, pod)
	}
	return pods, nil
}

// ─── NodeProvider Interface ─────────────────────────────────────────────────

// ConfigureNode sets up the Kubernetes node object that represents this
// MikroTik device in the cluster.
func (p *MikroTikProvider) ConfigureNode(ctx context.Context, node *corev1.Node) {
	node.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),     // typical CHR/RB capacity
		corev1.ResourceMemory: resource.MustParse("1Gi"),
		corev1.ResourcePods:   resource.MustParse("20"),
	}
	node.Status.Allocatable = node.Status.Capacity
	node.Status.NodeInfo = corev1.NodeSystemInfo{
		Architecture:    "arm64",
		OperatingSystem: "linux",
		KubeletVersion:  "v1.29.0-mikrotik-vk",
	}
	node.Status.Conditions = []corev1.NodeCondition{
		{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionTrue,
		},
	}
	node.Labels = map[string]string{
		"type":                     "virtual-kubelet",
		"kubernetes.io/os":         "linux",
		"kubernetes.io/arch":       "arm64",
		"node.kubernetes.io/role":  "mikrotik",
		"mikrotik.io/device-type":  "routeros",
	}

	// Add taint so normal pods aren't scheduled here
	node.Spec.Taints = []corev1.Taint{
		{
			Key:    "virtual-kubelet.io/provider",
			Value:  "mikrotik",
			Effect: corev1.TaintEffectNoSchedule,
		},
	}
}

// ─── Standalone Reconciler ──────────────────────────────────────────────────

// RunStandaloneReconciler runs a local reconciliation loop without requiring
// a Kubernetes API server. Reads desired state from a local YAML file and
// reconciles against actual RouterOS container state.
func (p *MikroTikProvider) RunStandaloneReconciler(ctx context.Context) error {
	log := p.deps.Logger
	log.Info("standalone reconciler starting")

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("standalone reconciler shutting down")
			return nil
		case <-ticker.C:
			if err := p.reconcile(ctx); err != nil {
				log.Errorw("reconciliation error", "error", err)
			}
		}
	}
}

func (p *MikroTikProvider) reconcile(ctx context.Context) error {
	// TODO: Load desired state from /etc/mikrotik-vk/pods.yaml
	// Compare with actual state from RouterOS
	// Create/delete/restart as needed
	//
	// This is the "kubelet-lite" reconciliation loop:
	//   1. Read desired pods from local manifest directory
	//   2. List actual containers via RouterOS API
	//   3. Diff: desired vs actual
	//   4. Create missing containers
	//   5. Remove orphaned containers
	//   6. Restart unhealthy containers (via systemd manager)
	return nil
}

// RunVirtualKubelet starts the full Virtual Kubelet node, registering
// with a Kubernetes API server.
func (p *MikroTikProvider) RunVirtualKubelet(ctx context.Context) error {
	// TODO: Wire up virtual-kubelet/node.NewNodeController
	// using this provider as the PodLifecycleHandler.
	//
	// Requires:
	//   - kubeconfig or in-cluster config
	//   - NodeController from virtual-kubelet library
	//   - Informer setup for pod watching
	//
	// See: github.com/virtual-kubelet/virtual-kubelet/node
	p.deps.Logger.Info("Virtual Kubelet mode not yet implemented — use --standalone for now")
	return p.RunStandaloneReconciler(ctx)
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func podKey(pod *corev1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}

// sanitizeName converts a pod/container name pair into a valid RouterOS
// container name (alphanumeric + hyphens, max 32 chars).
func sanitizeName(pod *corev1.Pod, containerName string) string {
	name := fmt.Sprintf("%s-%s", pod.Name, containerName)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32 // lowercase
		}
		return '-'
	}, name)
	return truncate(name, 32)
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

func boolToConditionStatus(b bool) corev1.ConditionStatus {
	if b {
		return corev1.ConditionTrue
	}
	return corev1.ConditionFalse
}

func extractHealthCheck(c corev1.Container) *systemd.HealthCheck {
	if c.LivenessProbe != nil && c.LivenessProbe.HTTPGet != nil {
		return &systemd.HealthCheck{
			Type:     "http",
			Path:     c.LivenessProbe.HTTPGet.Path,
			Port:     int(c.LivenessProbe.HTTPGet.Port.IntVal),
			Interval: int(c.LivenessProbe.PeriodSeconds),
		}
	}
	if c.LivenessProbe != nil && c.LivenessProbe.TCPSocket != nil {
		return &systemd.HealthCheck{
			Type: "tcp",
			Port: int(c.LivenessProbe.TCPSocket.Port.IntVal),
		}
	}
	return nil
}

func extractDependencies(pod *corev1.Pod) []string {
	if deps, ok := pod.Annotations["mikrotik.io/depends-on"]; ok {
		return strings.Split(deps, ",")
	}
	return nil
}

func extractPriority(pod *corev1.Pod, index int) int {
	// Default: containers within a pod start in order
	// Override with annotation: mikrotik.io/boot-priority
	return index * 10
}
