package provider

import (
	"encoding/json"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RegisterRoutes registers Kubernetes-compatible Pod API handlers on the provided mux.
func (p *MicroKubeProvider) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/pods", p.handleListAllPods)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/pods", p.handleListNamespacedPods)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/pods/{name}", p.handleGetPod)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/pods/{name}/status", p.handleGetPodStatus)
}

func (p *MicroKubeProvider) handleListAllPods(w http.ResponseWriter, r *http.Request) {
	pods, err := p.GetPods(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	items := make([]corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		enriched := pod.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
		if status, err := p.GetPodStatus(r.Context(), pod.Namespace, pod.Name); err == nil {
			enriched.Status = *status
		}
		items = append(items, *enriched)
	}

	podWriteJSON(w, http.StatusOK, corev1.PodList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleListNamespacedPods(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")

	pods, err := p.GetPods(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	items := make([]corev1.Pod, 0)
	for _, pod := range pods {
		if pod.Namespace != ns {
			continue
		}
		enriched := pod.DeepCopy()
		enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
		if status, err := p.GetPodStatus(r.Context(), pod.Namespace, pod.Name); err == nil {
			enriched.Status = *status
		}
		items = append(items, *enriched)
	}

	podWriteJSON(w, http.StatusOK, corev1.PodList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
		Items:    items,
	})
}

func (p *MicroKubeProvider) handleGetPod(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("namespace")
	name := r.PathValue("name")

	pod, err := p.GetPod(r.Context(), ns, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	enriched := pod.DeepCopy()
	enriched.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	if status, err := p.GetPodStatus(r.Context(), ns, name); err == nil {
		enriched.Status = *status
	}

	podWriteJSON(w, http.StatusOK, enriched)
}

func (p *MicroKubeProvider) handleGetPodStatus(w http.ResponseWriter, r *http.Request) {
	p.handleGetPod(w, r)
}

func podWriteJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
