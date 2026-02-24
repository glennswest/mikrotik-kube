// mkube-installer: One-shot bootstrap that creates the registry container,
// seeds required images from GHCR, and starts mkube-update.
//
// Flow:
//   1. Create registry container on RouterOS (pull image → docker-save → REST API)
//   2. Wait for registry to respond on /v2/
//   3. Seed all required images from GHCR into local registry
//   4. Create mkube-update container from local registry
//   5. Exit
//
// After this runs, mkube-update takes over and bootstraps mkube itself.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/glennswest/mkube/pkg/dockersave"
)

var version = "dev"

// Config is the installer configuration.
type Config struct {
	// RouterOS connection
	RouterOSURL      string `yaml:"routerosURL"`
	RouterOSUser     string `yaml:"routerosUser"`
	RouterOSPassword string `yaml:"routerosPassword"`

	// Registry container settings
	Registry RegistryContainerConfig `yaml:"registry"`

	// mkube-update container settings
	MkubeUpdate MkubeUpdateContainerConfig `yaml:"mkubeUpdate"`

	// Images to seed from GHCR into local registry after it's up
	SeedImages []SeedImage `yaml:"seedImages"`

	// Working directory for tarballs (inside the installer container)
	TarballDir string `yaml:"tarballDir"`

	// SelfRootDir is the installer's root-dir as seen by RouterOS host,
	// for translating container paths to host-visible paths.
	SelfRootDir string `yaml:"selfRootDir"`
}

// RegistryContainerConfig defines the registry container to create.
type RegistryContainerConfig struct {
	// Image to pull from GHCR for the registry container
	Image string `yaml:"image"` // e.g. "ghcr.io/glennswest/mkube-registry:edge"

	// Container spec
	Name        string `yaml:"name"`      // e.g. "registry.gt.lo"
	Interface   string `yaml:"interface"` // e.g. "veth-registry"
	RootDir     string `yaml:"rootDir"`   // e.g. "/raid1/images/registry.gt.lo"
	DNS         string `yaml:"dns"`       // e.g. "192.168.200.199"
	MountLists  string `yaml:"mountLists"`
	IP          string `yaml:"ip"`     // e.g. "192.168.200.3"
	Bridge      string `yaml:"bridge"` // e.g. "bridge-gt"
	Gateway     string `yaml:"gateway"` // e.g. "192.168.200.1"

	// URL for health check after creation
	HealthURL string `yaml:"healthURL"` // e.g. "http://192.168.200.3:5000/v2/"
}

// MkubeUpdateContainerConfig defines the mkube-update container to create.
type MkubeUpdateContainerConfig struct {
	// Image ref in local registry (set after seeding)
	LocalImage string `yaml:"localImage"` // e.g. "192.168.200.3:5000/mkube-update:edge"

	// Container spec
	Name        string `yaml:"name"`      // e.g. "mkube-update-updater"
	Interface   string `yaml:"interface"` // e.g. "veth-mkube"
	RootDir     string `yaml:"rootDir"`
	DNS         string `yaml:"dns"`
	MountLists  string `yaml:"mountLists"`
	Hostname    string `yaml:"hostname"`
}

// SeedImage defines an image to copy from GHCR to local registry.
type SeedImage struct {
	Upstream string `yaml:"upstream"` // e.g. "ghcr.io/glennswest/mkube:edge"
	Local    string `yaml:"local"`    // e.g. "mkube:edge" (local registry repo:tag)
}

func main() {
	logger, _ := zap.NewProduction()
	defer func() { _ = logger.Sync() }()
	log := logger.Sugar()

	log.Infow("starting mkube-installer", "version", version)

	configPath := "/etc/installer/config.yaml"
	if v := os.Getenv("INSTALLER_CONFIG"); v != "" {
		configPath = v
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalw("reading config", "path", configPath, "error", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalw("parsing config", "error", err)
	}

	if cfg.TarballDir == "" {
		cfg.TarballDir = "/data"
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	installer := &Installer{
		cfg: cfg,
		log: log,
		http: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
			Timeout: 30 * time.Second,
		},
	}

	if err := installer.Run(ctx); err != nil {
		log.Fatalw("installer failed", "error", err)
	}

	log.Info("installer complete, exiting")
}

// Installer orchestrates the bootstrap process.
type Installer struct {
	cfg  Config
	log  *zap.SugaredLogger
	http *http.Client
}

// Run executes the full bootstrap sequence.
func (ins *Installer) Run(ctx context.Context) error {
	// Step 1: Create registry veth + container
	if err := ins.ensureRegistry(ctx); err != nil {
		return fmt.Errorf("ensuring registry: %w", err)
	}

	// Step 2: Wait for registry to be healthy
	if err := ins.waitForRegistry(ctx); err != nil {
		return fmt.Errorf("waiting for registry: %w", err)
	}

	// Step 3: Seed images from GHCR
	if err := ins.seedImages(ctx); err != nil {
		return fmt.Errorf("seeding images: %w", err)
	}

	// Step 4: Create mkube-update container
	if err := ins.ensureMkubeUpdate(ctx); err != nil {
		return fmt.Errorf("ensuring mkube-update: %w", err)
	}

	return nil
}

// ensureRegistry creates the registry container if it doesn't already exist.
func (ins *Installer) ensureRegistry(ctx context.Context) error {
	rc := ins.cfg.Registry
	log := ins.log.With("step", "registry")

	// Check if already running
	ct, err := ins.rosGetContainer(ctx, rc.Name)
	if err == nil {
		if ct.isRunning() {
			log.Info("registry already running")
			return nil
		}
		log.Info("registry exists but stopped, starting")
		if err := ins.rosPost(ctx, "/container/start", map[string]string{".id": ct.ID}); err != nil {
			return fmt.Errorf("starting registry: %w", err)
		}
		return ins.waitForContainerRunning(ctx, rc.Name)
	}

	// Create veth for registry
	log.Infow("creating registry veth", "interface", rc.Interface, "ip", rc.IP)
	ins.rosPost(ctx, "/interface/veth/add", map[string]string{
		"name":    rc.Interface,
		"address": rc.IP + "/24",
		"gateway": rc.Gateway,
	}) // ignore error if already exists

	// Add veth to bridge
	ins.rosPost(ctx, "/interface/bridge/port/add", map[string]string{
		"bridge":    rc.Bridge,
		"interface": rc.Interface,
	}) // ignore error if already exists

	// Pull registry image from GHCR
	log.Infow("pulling registry image", "ref", rc.Image)
	img, err := ins.pullImage(ctx, rc.Image)
	if err != nil {
		return fmt.Errorf("pulling registry image: %w", err)
	}

	// Build docker-save tarball
	tarball, err := ins.buildTarball(img, rc.Image)
	if err != nil {
		return fmt.Errorf("building registry tarball: %w", err)
	}

	// Write tarball to disk
	tarballPath := filepath.Join(ins.cfg.TarballDir, "registry.tar")
	log.Infow("writing tarball", "path", tarballPath, "size", len(tarball))
	if err := os.MkdirAll(filepath.Dir(tarballPath), 0o755); err != nil {
		return fmt.Errorf("creating tarball dir: %w", err)
	}
	if err := os.WriteFile(tarballPath, tarball, 0o644); err != nil {
		return fmt.Errorf("writing tarball: %w", err)
	}

	// Translate path for RouterOS host visibility
	hostTarball := tarballPath
	if ins.cfg.SelfRootDir != "" {
		hostTarball = ins.cfg.SelfRootDir + "/" + strings.TrimPrefix(tarballPath, "/")
	}

	// Create container
	spec := map[string]string{
		"name":          rc.Name,
		"file":          hostTarball,
		"interface":     rc.Interface,
		"root-dir":      rc.RootDir,
		"logging":       "yes",
		"start-on-boot": "yes",
		"hostname":      rc.Name,
	}
	if rc.DNS != "" {
		spec["dns"] = rc.DNS
	}
	if rc.MountLists != "" {
		spec["mountlists"] = rc.MountLists
	}

	log.Infow("creating registry container", "spec", spec)
	if err := ins.rosPost(ctx, "/container/add", spec); err != nil {
		return fmt.Errorf("creating registry container: %w", err)
	}

	// Wait for extraction
	log.Info("waiting for container extraction")
	if err := ins.waitForExtraction(ctx, rc.Name); err != nil {
		return err
	}

	// Start
	newCt, err := ins.rosGetContainer(ctx, rc.Name)
	if err != nil {
		return fmt.Errorf("getting new container: %w", err)
	}

	log.Info("starting registry container")
	if err := ins.rosPost(ctx, "/container/start", map[string]string{".id": newCt.ID}); err != nil {
		return fmt.Errorf("starting registry: %w", err)
	}

	return ins.waitForContainerRunning(ctx, rc.Name)
}

// waitForRegistry polls the registry's /v2/ endpoint until it responds.
func (ins *Installer) waitForRegistry(ctx context.Context) error {
	url := ins.cfg.Registry.HealthURL
	if url == "" {
		url = fmt.Sprintf("http://%s:5000/v2/", strings.TrimSuffix(ins.cfg.Registry.IP, "/24"))
	}

	ins.log.Infow("waiting for registry health", "url", url)

	for i := 0; i < 120; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return err
		}

		resp, err := ins.http.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ins.log.Info("registry is healthy")
				return nil
			}
		}

		time.Sleep(time.Second)
	}

	return fmt.Errorf("registry did not become healthy within 120s")
}

// seedImages copies images from GHCR to the local registry.
func (ins *Installer) seedImages(ctx context.Context) error {
	registryAddr := strings.TrimSuffix(ins.cfg.Registry.IP, "/24") + ":5000"

	for _, img := range ins.cfg.SeedImages {
		localRef := registryAddr + "/" + img.Local
		ins.log.Infow("seeding image", "upstream", img.Upstream, "local", localRef)

		err := crane.Copy(img.Upstream, localRef,
			crane.WithContext(ctx),
			crane.WithPlatform(&v1.Platform{
				OS:           "linux",
				Architecture: "arm64",
			}),
			crane.Insecure,
			crane.WithAuthFromKeychain(
				authn.NewMultiKeychain(authn.DefaultKeychain, dockersave.AnonymousKeychain{}),
			),
		)
		if err != nil {
			ins.log.Errorw("failed to seed image", "upstream", img.Upstream, "error", err)
			return fmt.Errorf("seeding %s: %w", img.Upstream, err)
		}
		ins.log.Infow("seeded image", "local", localRef)
	}

	return nil
}

// ensureMkubeUpdate creates the mkube-update container if it doesn't exist.
func (ins *Installer) ensureMkubeUpdate(ctx context.Context) error {
	mc := ins.cfg.MkubeUpdate
	log := ins.log.With("step", "mkube-update")

	// Check if already running
	ct, err := ins.rosGetContainer(ctx, mc.Name)
	if err == nil {
		if ct.isRunning() {
			log.Info("mkube-update already running")
			return nil
		}
		log.Info("mkube-update exists but stopped, starting")
		if err := ins.rosPost(ctx, "/container/start", map[string]string{".id": ct.ID}); err != nil {
			return fmt.Errorf("starting mkube-update: %w", err)
		}
		return ins.waitForContainerRunning(ctx, mc.Name)
	}

	// Create container from local registry image
	spec := map[string]string{
		"name":          mc.Name,
		"tag":           mc.LocalImage,
		"interface":     mc.Interface,
		"root-dir":      mc.RootDir,
		"logging":       "yes",
		"start-on-boot": "yes",
	}
	if mc.DNS != "" {
		spec["dns"] = mc.DNS
	}
	if mc.Hostname != "" {
		spec["hostname"] = mc.Hostname
	}
	if mc.MountLists != "" {
		spec["mountlists"] = mc.MountLists
	}

	log.Infow("creating mkube-update container", "spec", spec)
	if err := ins.rosPost(ctx, "/container/add", spec); err != nil {
		return fmt.Errorf("creating mkube-update: %w", err)
	}

	// Wait for image pull + extraction
	log.Info("waiting for mkube-update extraction")
	if err := ins.waitForExtraction(ctx, mc.Name); err != nil {
		return err
	}

	// Start
	newCt, err := ins.rosGetContainer(ctx, mc.Name)
	if err != nil {
		return fmt.Errorf("getting new container: %w", err)
	}

	log.Info("starting mkube-update")
	if err := ins.rosPost(ctx, "/container/start", map[string]string{".id": newCt.ID}); err != nil {
		return fmt.Errorf("starting mkube-update: %w", err)
	}

	return ins.waitForContainerRunning(ctx, mc.Name)
}

// ── Image helpers ───────────────────────────────────────────────────────────

func (ins *Installer) pullImage(ctx context.Context, ref string) (v1.Image, error) {
	return crane.Pull(ref,
		crane.WithContext(ctx),
		crane.WithPlatform(&v1.Platform{
			OS:           "linux",
			Architecture: "arm64",
		}),
		crane.WithAuthFromKeychain(
			authn.NewMultiKeychain(authn.DefaultKeychain, dockersave.AnonymousKeychain{}),
		),
	)
}

func (ins *Installer) buildTarball(img v1.Image, ref string) ([]byte, error) {
	// Flatten OCI layers into rootfs
	rootfsReader := mutate.Extract(img)
	defer rootfsReader.Close()

	var rootfsBuf bytes.Buffer
	if _, err := io.Copy(&rootfsBuf, rootfsReader); err != nil {
		return nil, fmt.Errorf("extracting rootfs: %w", err)
	}

	imgCfg, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("reading image config: %w", err)
	}

	var dockerSave bytes.Buffer
	if err := dockersave.Write(&dockerSave, rootfsBuf.Bytes(), ref, imgCfg); err != nil {
		return nil, fmt.Errorf("building tarball: %w", err)
	}

	return dockerSave.Bytes(), nil
}

// ── RouterOS REST helpers ───────────────────────────────────────────────────

type rosContainer struct {
	ID      string
	Name    string
	Running string
	Stopped string
}

func (c rosContainer) isRunning() bool { return c.Running == "true" }
func (c rosContainer) isStopped() bool { return c.Stopped == "true" }

func (ins *Installer) rosGetContainer(ctx context.Context, name string) (*rosContainer, error) {
	var containers []map[string]interface{}
	if err := ins.rosGET(ctx, "/container", &containers); err != nil {
		return nil, err
	}

	for _, c := range containers {
		n, _ := c["name"].(string)
		if n != name {
			continue
		}
		return &rosContainer{
			ID:      strVal(c, ".id"),
			Name:    n,
			Running: strVal(c, "running"),
			Stopped: strVal(c, "stopped"),
		}, nil
	}
	return nil, fmt.Errorf("container %q not found", name)
}

func strVal(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

func (ins *Installer) rosGET(ctx context.Context, path string, result interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "GET", ins.cfg.RouterOSURL+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(ins.cfg.RouterOSUser, ins.cfg.RouterOSPassword)
	req.Header.Set("Accept", "application/json")

	resp, err := ins.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: %d: %s", path, resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(result)
}

func (ins *Installer) rosPost(ctx context.Context, path string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", ins.cfg.RouterOSURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.SetBasicAuth(ins.cfg.RouterOSUser, ins.cfg.RouterOSPassword)
	req.Header.Set("Content-Type", "application/json")

	resp, err := ins.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: %d: %s", path, resp.StatusCode, string(b))
	}
	return nil
}

// ── Wait helpers ────────────────────────────────────────────────────────────

func (ins *Installer) waitForExtraction(ctx context.Context, name string) error {
	for i := 0; i < 120; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		time.Sleep(time.Second)
		ct, err := ins.rosGetContainer(ctx, name)
		if err != nil {
			continue
		}
		if ct.isStopped() {
			return nil
		}
	}
	return fmt.Errorf("container %s not extracted within 120s", name)
}

func (ins *Installer) waitForContainerRunning(ctx context.Context, name string) error {
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		ct, err := ins.rosGetContainer(ctx, name)
		if err != nil {
			continue
		}
		if ct.isRunning() {
			ins.log.Infow("container running", "name", name)
			return nil
		}
	}
	return fmt.Errorf("container %s did not start within 30s", name)
}
