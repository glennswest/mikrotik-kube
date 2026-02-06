package registry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	"go.uber.org/zap"

	"github.com/glenneth/mikrotik-vk/pkg/config"
)

// Registry wraps an embedded OCI registry (Zot) or provides a lightweight
// distribution-spec proxy. For the initial implementation, this provides
// a simple pull-through cache proxy that forwards to upstream registries.
//
// Production deployment options:
//   1. Embed Zot as a Go library (zotregistry.dev/zot)
//   2. Use this lightweight proxy that caches tarballs locally
//   3. Run Zot as a separate container managed by mikrotik-vk itself
type Registry struct {
	cfg    config.RegistryConfig
	log    *zap.SugaredLogger
	server *http.Server
}

// Start launches the embedded registry server.
func Start(ctx context.Context, cfg config.RegistryConfig, log *zap.SugaredLogger) (*Registry, error) {
	r := &Registry{
		cfg: cfg,
		log: log,
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

// handleV2 implements the OCI distribution spec base endpoint.
// GET /v2/ should return 200 OK to indicate the registry is available.
func (r *Registry) handleV2(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/v2/" || req.URL.Path == "/v2" {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Pull-through: proxy to upstream registries
	if r.cfg.PullThrough && len(r.cfg.UpstreamRegistries) > 0 {
		r.proxyToUpstream(w, req)
		return
	}

	// TODO: Implement local blob/manifest storage
	// For now, return 404 for anything not cached
	http.NotFound(w, req)
}

// handleCatalog returns the list of locally cached images.
func (r *Registry) handleCatalog(w http.ResponseWriter, req *http.Request) {
	// TODO: Read from local storage and return catalog
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"repositories":[]}`)
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
