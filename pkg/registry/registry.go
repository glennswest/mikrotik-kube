package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"go.uber.org/zap"

	"github.com/glenneth/mikrotik-kube/pkg/config"
)

// Registry provides an OCI Distribution Spec v2 compatible registry with
// on-disk blob/manifest storage and optional pull-through caching from
// upstream registries.
type Registry struct {
	cfg    config.RegistryConfig
	log    *zap.SugaredLogger
	server *http.Server
	store  *BlobStore
}

// Start launches the embedded registry server.
func Start(ctx context.Context, cfg config.RegistryConfig, log *zap.SugaredLogger) (*Registry, error) {
	store, err := NewBlobStore(cfg.StorePath)
	if err != nil {
		return nil, fmt.Errorf("initializing blob store: %w", err)
	}

	r := &Registry{
		cfg:   cfg,
		log:   log,
		store: store,
	}

	mux := http.NewServeMux()

	// OCI Distribution Spec v2 endpoints
	mux.HandleFunc("/v2/", r.handleV2)
	mux.HandleFunc("/v2/_catalog", r.handleCatalog)

	r.server = &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	go func() {
		log.Infow("registry listening", "addr", cfg.ListenAddr)
		if err := r.server.ListenAndServe(); err != http.ErrServerClosed {
			log.Errorw("registry server error", "error", err)
		}
	}()

	return r, nil
}

// Shutdown gracefully stops the registry server.
func (r *Registry) Shutdown(ctx context.Context) error {
	r.log.Info("shutting down registry")
	return r.server.Shutdown(ctx)
}

// handleV2 routes OCI distribution spec requests.
func (r *Registry) handleV2(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path

	// Base endpoint: GET /v2/
	if path == "/v2/" || path == "/v2" {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Parse the path: /v2/<name>/manifests/<reference> or /v2/<name>/blobs/<digest>
	parts := strings.SplitN(strings.TrimPrefix(path, "/v2/"), "/", 3)
	if len(parts) < 3 {
		http.NotFound(w, req)
		return
	}

	// Reconstruct repo name (may contain slashes) and resource type
	var repoName, resourceType, reference string
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] == "manifests" || parts[i] == "blobs" {
			repoName = strings.Join(parts[:i], "/")
			resourceType = parts[i]
			reference = parts[i+1]
			break
		}
	}

	// Fallback: try parsing from the end of the full path
	if repoName == "" {
		trimmed := strings.TrimPrefix(path, "/v2/")
		if idx := strings.LastIndex(trimmed, "/manifests/"); idx >= 0 {
			repoName = trimmed[:idx]
			resourceType = "manifests"
			reference = trimmed[idx+len("/manifests/"):]
		} else if idx := strings.LastIndex(trimmed, "/blobs/"); idx >= 0 {
			repoName = trimmed[:idx]
			resourceType = "blobs"
			reference = trimmed[idx+len("/blobs/"):]
		}
	}

	if repoName == "" || reference == "" {
		http.NotFound(w, req)
		return
	}

	switch resourceType {
	case "manifests":
		r.handleManifest(w, req, repoName, reference)
	case "blobs":
		r.handleBlob(w, req, repoName, reference)
	default:
		http.NotFound(w, req)
	}
}

// handleManifest serves or stores manifests.
func (r *Registry) handleManifest(w http.ResponseWriter, req *http.Request, repo, ref string) {
	switch req.Method {
	case http.MethodGet:
		data, contentType, err := r.store.GetManifest(repo, ref)
		if err == nil {
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Docker-Content-Digest", ref)
			w.Write(data)
			return
		}
		// Pull-through: fetch from upstream, cache locally
		if r.cfg.PullThrough {
			r.pullThroughManifest(w, req, repo, ref)
			return
		}
		http.NotFound(w, req)

	case http.MethodHead:
		data, contentType, err := r.store.GetManifest(repo, ref)
		if err == nil {
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.Header().Set("Docker-Content-Digest", ref)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, req)

	case http.MethodPut:
		contentType := req.Header.Get("Content-Type")
		if err := r.store.PutManifest(repo, ref, contentType, req.Body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Docker-Content-Digest", ref)
		w.WriteHeader(http.StatusCreated)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleBlob serves or stores blobs.
func (r *Registry) handleBlob(w http.ResponseWriter, req *http.Request, repo, digest string) {
	switch req.Method {
	case http.MethodGet:
		data, err := r.store.GetBlob(digest)
		if err == nil {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.Header().Set("Docker-Content-Digest", digest)
			w.Write(data)
			return
		}
		// Pull-through: fetch from upstream
		if r.cfg.PullThrough {
			r.pullThroughBlob(w, req, repo, digest)
			return
		}
		http.NotFound(w, req)

	case http.MethodHead:
		exists, size := r.store.HasBlob(digest)
		if exists {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, req)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleCatalog returns the list of locally stored repositories.
func (r *Registry) handleCatalog(w http.ResponseWriter, req *http.Request) {
	repos := r.store.ListRepositories()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string][]string{
		"repositories": repos,
	})
}

// pullThroughManifest fetches a manifest from upstream, caches it, and serves it.
func (r *Registry) pullThroughManifest(w http.ResponseWriter, req *http.Request, repo, ref string) {
	for _, upstream := range r.cfg.UpstreamRegistries {
		upstreamURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", upstream, repo, ref)
		proxyReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, upstreamURL, nil)
		if err != nil {
			continue
		}
		proxyReq.Header.Set("Accept", req.Header.Get("Accept"))

		resp, err := http.DefaultClient.Do(proxyReq)
		if err != nil || resp.StatusCode >= 400 {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}

		contentType := resp.Header.Get("Content-Type")
		digest := resp.Header.Get("Docker-Content-Digest")

		// Cache it locally
		if err := r.store.PutManifest(repo, ref, contentType, resp.Body); err != nil {
			resp.Body.Close()
			r.log.Warnw("failed to cache manifest", "repo", repo, "ref", ref, "error", err)
			http.Error(w, "cache error", http.StatusInternalServerError)
			return
		}
		resp.Body.Close()

		// Serve from cache
		data, ct, err := r.store.GetManifest(repo, ref)
		if err != nil {
			http.Error(w, "cache read error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", ct)
		if digest != "" {
			w.Header().Set("Docker-Content-Digest", digest)
		}
		w.Write(data)
		r.log.Debugw("pull-through manifest cached", "repo", repo, "ref", ref, "upstream", upstream)
		return
	}

	http.Error(w, "manifest not found on any upstream", http.StatusNotFound)
}

// pullThroughBlob fetches a blob from upstream, caches it, and serves it.
func (r *Registry) pullThroughBlob(w http.ResponseWriter, req *http.Request, repo, digest string) {
	for _, upstream := range r.cfg.UpstreamRegistries {
		upstreamURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s", upstream, repo, digest)
		proxyReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, upstreamURL, nil)
		if err != nil {
			continue
		}

		resp, err := http.DefaultClient.Do(proxyReq)
		if err != nil || resp.StatusCode >= 400 {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}

		// Cache blob locally
		if err := r.store.PutBlob(digest, resp.Body); err != nil {
			resp.Body.Close()
			r.log.Warnw("failed to cache blob", "digest", digest, "error", err)
			http.Error(w, "cache error", http.StatusInternalServerError)
			return
		}
		resp.Body.Close()

		// Serve from cache
		data, err := r.store.GetBlob(digest)
		if err != nil {
			http.Error(w, "cache read error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.Header().Set("Docker-Content-Digest", digest)
		w.Write(data)
		r.log.Debugw("pull-through blob cached", "digest", digest, "upstream", upstream)
		return
	}

	http.Error(w, "blob not found on any upstream", http.StatusNotFound)
}

// proxyToUpstream attempts to proxy the request to configured upstream registries.
func (r *Registry) proxyToUpstream(w http.ResponseWriter, req *http.Request) {
	for _, upstream := range r.cfg.UpstreamRegistries {
		target, err := url.Parse(fmt.Sprintf("https://%s", upstream))
		if err != nil {
			continue
		}

		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			// Silently try next upstream
		}

		r.log.Debugw("proxying to upstream", "upstream", upstream, "path", req.URL.Path)
		proxy.ServeHTTP(w, req)
		return
	}

	http.Error(w, "no upstream registry available", http.StatusBadGateway)
}
