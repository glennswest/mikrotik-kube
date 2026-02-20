package provider

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/glennswest/mkube/pkg/config"
)

// generateDefaultConfigMaps creates built-in ConfigMaps derived from the
// running mkube configuration. These are loaded at startup and can be
// overridden by user-supplied ConfigMaps from the boot manifest.
func generateDefaultConfigMaps(cfg *config.Config) []*corev1.ConfigMap {
	gateway := cfg.DefaultNetwork().Gateway

	consoleConfig := fmt.Sprintf(`listenAddr: ":9090"
nodes:
  - name: %s
    address: "http://%s:8082"
registryURL: "http://%s:5000"
`, cfg.NodeName, gateway, gateway)

	return []*corev1.ConfigMap{
		{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mkube-console-config",
				Namespace: "infra",
			},
			Data: map[string]string{
				"config.yaml": consoleConfig,
			},
		},
	}
}
