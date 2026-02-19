package storage

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"go.uber.org/zap"

	"github.com/glennswest/mkube/pkg/config"
	"github.com/glennswest/mkube/pkg/routeros"
)

// Manager handles OCI image → tarball conversion, volume provisioning,
// tarball caching on RouterOS, and garbage collection of unused images
// and orphaned volumes.
type Manager struct {
	cfg         config.StorageConfig
	registryCfg config.RegistryConfig
	ros         *routeros.Client
	log         *zap.SugaredLogger

	mu      sync.Mutex
	images  map[string]*CachedImage // image ref -> cache entry
	volumes map[string]*ProvisionedVolume
}

// CachedImage tracks a cached OCI image tarball on the RouterOS filesystem.
type CachedImage struct {
	Ref         string // e.g. "docker.io/library/nginx:latest"
	TarballPath string // path on RouterOS, e.g. "/container-cache/nginx-latest.tar"
	PulledAt    time.Time
	Size        int64
	InUse       int // reference count
}

// ProvisionedVolume tracks a volume created for a container.
type ProvisionedVolume struct {
	Name          string
	ContainerName string
	HostPath      string
	MountPath     string
	CreatedAt     time.Time
}

// NewManager initializes the storage manager.
func NewManager(cfg config.StorageConfig, registryCfg config.RegistryConfig, ros *routeros.Client, log *zap.SugaredLogger) (*Manager, error) {
	return &Manager{
		cfg:         cfg,
		registryCfg: registryCfg,
		ros:         ros,
		log:         log,
		images:      make(map[string]*CachedImage),
		volumes:     make(map[string]*ProvisionedVolume),
	}, nil
}

// EnsureImage makes sure the given OCI image reference is available as a
// tarball on the RouterOS filesystem. Steps:
//  1. Check local cache
//  2. If not cached: pull from registry, convert to tarball, upload to RouterOS
//  3. Return the tarball path on RouterOS
func (m *Manager) EnsureImage(ctx context.Context, imageRef string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check cache
	if cached, ok := m.images[imageRef]; ok {
		cached.InUse++
		m.log.Debugw("image cache hit", "ref", imageRef, "path", cached.TarballPath)
		return cached.TarballPath, nil
	}

	m.log.Infow("pulling image", "ref", imageRef)

	// Determine tarball filename from image ref
	tarballName := sanitizeImageRef(imageRef) + ".tar"
	tarballPath := fmt.Sprintf("%s/%s", m.cfg.TarballCache, tarballName)

	// Pull the image and convert to tarball.
	// In practice this would use:
	//   - crane/go-containerregistry to pull OCI images
	//   - Convert to a flat tarball that RouterOS expects
	//   - Upload via RouterOS file API or SFTP
	//
	// For images from the embedded Zot registry (localhost:5000),
	// this is a local operation.
	if err := m.pullAndUpload(ctx, imageRef, tarballPath); err != nil {
		return "", fmt.Errorf("pulling image %s: %w", imageRef, err)
	}

	// When selfRootDir is set, translate the container-internal path to the
	// host-visible path that RouterOS uses for container file references.
	// e.g. /raid1/cache/foo.tar → raid1/images/kube.gt.lo/raid1/cache/foo.tar
	hostPath := tarballPath
	if m.cfg.SelfRootDir != "" {
		hostPath = m.cfg.SelfRootDir + "/" + strings.TrimPrefix(tarballPath, "/")
		m.log.Infow("translated tarball path for RouterOS", "container", tarballPath, "host", hostPath)
	}

	m.images[imageRef] = &CachedImage{
		Ref:         imageRef,
		TarballPath: hostPath,
		PulledAt:    time.Now(),
		InUse:       1,
	}

	return hostPath, nil
}

// pullAndUpload pulls an OCI image from a registry, converts it to a
// RouterOS-compatible flat rootfs tar, and uploads it.
func (m *Manager) pullAndUpload(ctx context.Context, imageRef, tarballPath string) error {
	// Rewrite bare localhost/ refs to the configured local registry address
	imageRef = m.rewriteLocalhost(imageRef)

	// Determine crane options — allow insecure for localhost registry
	opts := []crane.Option{crane.WithContext(ctx)}

	// Explicit platform: target is always Linux (RouterOS), arch matches build
	opts = append(opts, crane.WithPlatform(&v1.Platform{
		OS:           "linux",
		Architecture: runtime.GOARCH,
	}))

	if m.isLocalRegistry(imageRef) {
		opts = append(opts, crane.Insecure)
	} else {
		// DefaultKeychain reads ~/.docker/config.json if present;
		// Anonymous fallback lets the transport handle OAuth2 bearer
		// token exchange for public registries (GHCR, Docker Hub, etc.)
		opts = append(opts, crane.WithAuthFromKeychain(
			authn.NewMultiKeychain(authn.DefaultKeychain, anonymousKeychain{}),
		))
	}

	m.log.Infow("pulling OCI image", "ref", imageRef)
	img, err := crane.Pull(imageRef, opts...)
	if err != nil {
		return fmt.Errorf("pulling image %s: %w", imageRef, err)
	}

	// Flatten OCI layers into a single uncompressed rootfs tarball,
	// then wrap it in docker-save format that RouterOS expects.
	m.log.Infow("flattening OCI layers to rootfs", "ref", imageRef)
	rootfsReader := mutate.Extract(img)
	defer rootfsReader.Close()

	var rootfsBuf bytes.Buffer
	if _, err := io.Copy(&rootfsBuf, rootfsReader); err != nil {
		return fmt.Errorf("extracting rootfs for %s: %w", imageRef, err)
	}

	// Extract image config for entrypoint/cmd/env
	imgCfg, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("reading image config for %s: %w", imageRef, err)
	}

	// Build docker-save format archive with uncompressed layer
	var dockerSave bytes.Buffer
	if err := writeDockerSave(&dockerSave, rootfsBuf.Bytes(), imageRef, imgCfg); err != nil {
		return fmt.Errorf("building docker-save for %s: %w", imageRef, err)
	}

	if m.cfg.SelfRootDir != "" {
		m.log.Infow("writing docker-save tarball to local disk", "ref", imageRef, "path", tarballPath, "size", dockerSave.Len())
		if err := os.MkdirAll(filepath.Dir(tarballPath), 0o755); err != nil {
			return fmt.Errorf("creating cache dir for %s: %w", tarballPath, err)
		}
		if err := os.WriteFile(tarballPath, dockerSave.Bytes(), 0o644); err != nil {
			return fmt.Errorf("writing tarball %s: %w", tarballPath, err)
		}
	} else {
		m.log.Infow("uploading tarball to RouterOS", "ref", imageRef, "path", tarballPath, "size", dockerSave.Len())
		if err := m.ros.UploadFile(ctx, tarballPath, bytes.NewReader(dockerSave.Bytes())); err != nil {
			return fmt.Errorf("uploading %s to RouterOS: %w", tarballPath, err)
		}
	}

	return nil
}

// rewriteLocalhost rewrites bare "localhost/foo:tag" refs to the configured
// local registry address. go-containerregistry treats "localhost/foo" (no port)
// as docker.io/localhost/foo, not a local registry.
func (m *Manager) rewriteLocalhost(imageRef string) string {
	if strings.HasPrefix(imageRef, "localhost/") && len(m.registryCfg.LocalAddresses) > 0 {
		rewritten := m.registryCfg.LocalAddresses[0] + "/" + strings.TrimPrefix(imageRef, "localhost/")
		m.log.Infow("rewrote bare localhost ref", "original", imageRef, "rewritten", rewritten)
		return rewritten
	}
	return imageRef
}

// isLocalRegistry returns true if the image ref points to the embedded registry
// (localhost or any configured local address).
func (m *Manager) isLocalRegistry(imageRef string) bool {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return strings.HasPrefix(imageRef, "localhost:") || strings.HasPrefix(imageRef, "localhost/")
	}
	registry := ref.Context().RegistryStr()
	if registry == "localhost" || registry == "localhost:5000" || strings.HasPrefix(registry, "localhost:") {
		return true
	}
	for _, addr := range m.registryCfg.LocalAddresses {
		if registry == addr {
			return true
		}
	}
	return false
}

// ReleaseImage decrements the use count of an image.
func (m *Manager) ReleaseImage(imageRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cached, ok := m.images[imageRef]; ok {
		cached.InUse--
		if cached.InUse < 0 {
			cached.InUse = 0
		}
	}
}

// ProvisionVolume creates a directory on the RouterOS filesystem for a
// container's volume mount.
func (m *Manager) ProvisionVolume(ctx context.Context, containerName, volumeName, mountPath string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	hostPath := fmt.Sprintf("%s/%s/%s", m.cfg.BasePath, containerName, volumeName)
	key := fmt.Sprintf("%s/%s", containerName, volumeName)

	// Create the directory on RouterOS
	// RouterOS filesystem is accessible via /file/
	// The directory will be created implicitly when the container starts
	// if root-dir is set, but we track it for GC purposes.

	m.volumes[key] = &ProvisionedVolume{
		Name:          volumeName,
		ContainerName: containerName,
		HostPath:      hostPath,
		MountPath:     mountPath,
		CreatedAt:     time.Now(),
	}

	m.log.Infow("volume provisioned", "container", containerName, "volume", volumeName, "path", hostPath)
	return hostPath, nil
}

// ─── Garbage Collection ─────────────────────────────────────────────────────

// RunGarbageCollector periodically cleans up unused images and orphaned volumes.
func (m *Manager) RunGarbageCollector(ctx context.Context) {
	interval := time.Duration(m.cfg.GCIntervalMinutes) * time.Minute
	if interval == 0 {
		interval = 30 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	m.log.Infow("storage GC started", "interval", interval)

	for {
		select {
		case <-ctx.Done():
			m.log.Info("storage GC shutting down")
			return
		case <-ticker.C:
			m.runGC(ctx)
		}
	}
}

func (m *Manager) runGC(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.log.Debug("running storage garbage collection")

	// 1. Clean up unused images (InUse == 0)
	var candidates []*CachedImage
	for _, img := range m.images {
		if img.InUse == 0 {
			candidates = append(candidates, img)
		}
	}

	// Sort by PulledAt (oldest first), keep last N
	// (simplified: just count and remove excess)
	removed := 0
	keepN := m.cfg.GCKeepLastN
	if keepN == 0 {
		keepN = 5
	}

	if len(candidates) > keepN {
		for _, img := range candidates[:len(candidates)-keepN] {
			if m.cfg.GCDryRun {
				m.log.Infow("GC dry-run: would remove image", "ref", img.Ref, "path", img.TarballPath)
				continue
			}

			if err := m.ros.RemoveFile(ctx, img.TarballPath); err != nil {
				m.log.Warnw("GC: failed to remove image", "ref", img.Ref, "error", err)
				continue
			}

			delete(m.images, img.Ref)
			removed++
		}
	}

	// 2. Find orphaned volumes (volumes whose container no longer exists)
	containers, err := m.ros.ListContainers(ctx)
	if err != nil {
		m.log.Warnw("GC: failed to list containers", "error", err)
		return
	}

	activeContainers := make(map[string]bool)
	for _, c := range containers {
		activeContainers[c.Name] = true
	}

	orphanedVolumes := 0
	for key, vol := range m.volumes {
		if !activeContainers[vol.ContainerName] {
			if m.cfg.GCDryRun {
				m.log.Infow("GC dry-run: would remove volume", "path", vol.HostPath)
				continue
			}

			if err := m.ros.RemoveFile(ctx, vol.HostPath); err != nil {
				m.log.Warnw("GC: failed to remove volume", "path", vol.HostPath, "error", err)
				continue
			}

			delete(m.volumes, key)
			orphanedVolumes++
		}
	}

	if removed > 0 || orphanedVolumes > 0 {
		m.log.Infow("GC completed", "imagesRemoved", removed, "volumesRemoved", orphanedVolumes)
	}
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// writeDockerSave writes a docker-save format tar archive to w.
// RouterOS requires this format with uncompressed layers.
// The structure matches what `docker save` produces:
//   - manifest.json
//   - repositories
//   - <config-sha>.json
//   - <layer-id>/layer.tar
//   - <layer-id>/VERSION
//   - <layer-id>/json
func writeDockerSave(w io.Writer, rootfsData []byte, imageRef string, imgCfg *v1.ConfigFile) error {
	// Compute layer SHA256
	layerHash := sha256.Sum256(rootfsData)
	layerID := fmt.Sprintf("%x", layerHash)

	// Derive a repo:tag from the image ref
	repoTag := "mkube:latest"
	if ref, err := name.NewTag(imageRef, name.WeakValidation); err == nil {
		repoTag = ref.Context().RepositoryStr() + ":" + ref.TagStr()
	}
	parts := strings.SplitN(repoTag, ":", 2)
	repo, tag := parts[0], "latest"
	if len(parts) == 2 {
		tag = parts[1]
	}

	// Build container config from the original image config
	containerCfg := map[string]interface{}{}
	if imgCfg != nil {
		if len(imgCfg.Config.Entrypoint) > 0 {
			containerCfg["Entrypoint"] = imgCfg.Config.Entrypoint
		}
		if len(imgCfg.Config.Cmd) > 0 {
			containerCfg["Cmd"] = imgCfg.Config.Cmd
		}
		if imgCfg.Config.WorkingDir != "" {
			containerCfg["WorkingDir"] = imgCfg.Config.WorkingDir
		}
		if len(imgCfg.Config.Env) > 0 {
			containerCfg["Env"] = imgCfg.Config.Env
		}
	}

	// Build image config JSON
	configObj := map[string]interface{}{
		"architecture": runtime.GOARCH,
		"os":           "linux",
		"rootfs": map[string]interface{}{
			"type":     "layers",
			"diff_ids": []string{"sha256:" + layerID},
		},
	}
	if len(containerCfg) > 0 {
		configObj["config"] = containerCfg
	}
	configJSON, _ := json.Marshal(configObj)
	configHash := sha256.Sum256(configJSON)
	configName := fmt.Sprintf("%x.json", configHash)

	// Build manifest.json
	manifestJSON, _ := json.Marshal([]map[string]interface{}{
		{
			"Config":   configName,
			"RepoTags": []string{repoTag},
			"Layers":   []string{layerID + "/layer.tar"},
		},
	})

	// Build repositories file
	reposJSON, _ := json.Marshal(map[string]map[string]string{
		repo: {tag: layerID},
	})

	// Build layer json (legacy docker format)
	layerJSON, _ := json.Marshal(map[string]interface{}{
		"id":      layerID,
		"created": "1970-01-01T00:00:00Z",
		"config":  containerCfg,
	})

	tw := tar.NewWriter(w)
	defer tw.Close()

	addFile := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Size: int64(len(data)),
			Mode: 0o644,
		}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}

	if err := addFile("manifest.json", manifestJSON); err != nil {
		return err
	}
	if err := addFile("repositories", reposJSON); err != nil {
		return err
	}
	if err := addFile(configName, configJSON); err != nil {
		return err
	}
	if err := addFile(layerID+"/VERSION", []byte("1.0")); err != nil {
		return err
	}
	if err := addFile(layerID+"/json", layerJSON); err != nil {
		return err
	}
	if err := addFile(layerID+"/layer.tar", rootfsData); err != nil {
		return err
	}

	return nil
}

// anonymousKeychain is a Keychain that always returns Anonymous auth,
// allowing the transport layer to handle OAuth2 bearer token exchange.
type anonymousKeychain struct{}

func (anonymousKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	return authn.Anonymous, nil
}

func sanitizeImageRef(ref string) string {
	result := make([]byte, 0, len(ref))
	for _, c := range ref {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, byte(c))
		} else if c >= 'A' && c <= 'Z' {
			result = append(result, byte(c+32))
		} else {
			result = append(result, '-')
		}
	}
	return string(result)
}
