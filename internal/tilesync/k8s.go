package tilesync

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// PendingAnnotation is the pod-template annotation the updater patches to make
// Kubernetes recreate the pod with a fresh init container.
const PendingAnnotation = "streetcryptid.io/pending-tiles-version"

const svcAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// KubeClient is a minimal in-cluster REST client. It uses only the projected
// service-account token and CA, so the image needs no client-go dependency and
// the RBAC surface is exactly one verb on one resource.
type KubeClient struct {
	host      string
	namespace string
	token     string
	http      *http.Client
}

// InClusterKubeClient builds a client from the pod's service-account mount and
// the standard KUBERNETES_SERVICE_* environment variables.
func InClusterKubeClient() (*KubeClient, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("k8s: not running in-cluster (KUBERNETES_SERVICE_HOST unset)")
	}
	token, err := os.ReadFile(svcAccountDir + "/token")
	if err != nil {
		return nil, fmt.Errorf("k8s: read token: %w", err)
	}
	ns, err := os.ReadFile(svcAccountDir + "/namespace")
	if err != nil {
		return nil, fmt.Errorf("k8s: read namespace: %w", err)
	}
	caPEM, err := os.ReadFile(svcAccountDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("k8s: read ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("k8s: bad ca bundle")
	}
	return &KubeClient{
		host:      fmt.Sprintf("https://%s:%s", host, port),
		namespace: strings.TrimSpace(string(ns)),
		token:     strings.TrimSpace(string(token)),
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			},
		},
	}, nil
}

// Namespace returns the pod's namespace.
func (k *KubeClient) Namespace() string { return k.namespace }

// PatchDeploymentPendingVersion strategically patches only this Deployment's
// pod-template annotation. The updater's Role grants get/patch on exactly this
// resource name, so the same call fails by RBAC on any other Deployment.
func (k *KubeClient) PatchDeploymentPendingVersion(ctx context.Context, namespace, name, version string) error {
	if namespace == "" {
		namespace = k.namespace
	}
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{
						PendingAnnotation: version,
					},
				},
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/apis/apps/v1/namespaces/%s/deployments/%s", k.host, namespace, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Content-Type", "application/strategic-merge-patch+json")
	req.Header.Set("Accept", "application/json")

	resp, err := k.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("k8s: patch deployment %s/%s: status %d: %s", namespace, name, resp.StatusCode, string(msg))
	}
	return nil
}
