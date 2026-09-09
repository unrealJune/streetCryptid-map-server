package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestChartCacheWiring(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	chart := filepath.Join("..", "..", "helm", "streetcryptid-map-server")
	for _, claim := range []string{"", "existing-cache"} {
		cmd := exec.Command(helm, "template", "maps", chart, "--set", "tiles.autoUpdate.enabled=false", "--set", "persistence.cache.existingClaim="+claim)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helm: %v\n%s", err, out)
		}
		text := string(out)
		for _, want := range []string{
			"BUNDLE_CACHE_MAX_BYTES\n              value: \"4294967296\"",
			"BUNDLE_MAX_BYTES\n              value: \"67108864\"",
			"BUNDLE_MAX_BUILDS\n              value: \"1\"",
			"TILE_DATA_DIR\n              value: \"/data\"",
			"mountPath: /data\n              readOnly: true",
			"memory: 1Gi",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("missing rendered config %q", want)
			}
		}
		if claim == "" {
			if !strings.Contains(text, "storage: 6Gi") || !strings.Contains(text, "claimName: maps-streetcryptid-map-server-cache") {
				t.Fatal("cache PVC not mounted")
			}
		} else {
			if !strings.Contains(text, "claimName: existing-cache") || strings.Contains(text, "name: maps-streetcryptid-map-server-cache\n") {
				t.Fatal("existing claim not respected")
			}
		}
	}
}
