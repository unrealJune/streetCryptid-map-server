package tilesync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/junephilip/streetcryptid-map-server/internal/pmtiles"
	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
)

// The terrain DEM is one archive beside the planet releases, not a release
// series of its own: it is baked rarely (the Copernicus DEM changes on the scale
// of years), it is optional (the app falls back to flat parkland without it),
// and an import is a single atomic rename followed by a pod recreate.
const (
	terrainDirName     = "terrain"
	terrainFileName    = "terrain.pmtiles"
	terrainVersionName = "version"
)

// TerrainPath is where the serving pod looks for the terrain archive.
func TerrainPath(dataDir string) string {
	return filepath.Join(dataDir, terrainDirName, terrainFileName)
}

// TerrainInfo describes an installed terrain archive.
type TerrainInfo struct {
	Path        string
	Version     string
	ContentType string
	MaxZoom     int
}

// ReadTerrain reports the installed terrain archive, or (nil, nil) when there is
// none — a server without terrain is a supported configuration.
func ReadTerrain(dataDir string) (*TerrainInfo, error) {
	path := TerrainPath(dataDir)
	r, err := pmtiles.OpenRaster(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("terrain: %w", err)
	}
	defer r.Close()
	if err := checkTerrainArchive(r); err != nil {
		return nil, err
	}
	version, err := os.ReadFile(filepath.Join(filepath.Dir(path), terrainVersionName))
	if err != nil {
		return nil, fmt.Errorf("terrain: version: %w", err)
	}
	return &TerrainInfo{
		Path:        path,
		Version:     strings.TrimSpace(string(version)),
		ContentType: terrainContentType(r.TileType()),
		MaxZoom:     r.MaxZoom(),
	}, nil
}

func checkTerrainArchive(r *pmtiles.Reader) error {
	if r.MaxZoom() > privacy.MaxTerrainZoom {
		return fmt.Errorf("terrain: archive reaches z%d, past the z%d terrain boundary", r.MaxZoom(), privacy.MaxTerrainZoom)
	}
	return nil
}

func terrainContentType(tileType byte) string {
	if tileType == pmtiles.TileTypePNG {
		return "image/png"
	}
	return "image/webp"
}

// ImportTerrain verifies a baked terrain archive and installs it atomically as
// the served DEM, then (in-cluster) recreates the pod so it reopens the file.
func (s *Syncer) ImportTerrain(ctx context.Context, path, version string) (*TerrainInfo, error) {
	if version == "" {
		version = nowRFC3339()
	}
	r, err := pmtiles.OpenRaster(path)
	if err != nil {
		return nil, fmt.Errorf("terrain import: %w", err)
	}
	err = checkTerrainArchive(r)
	r.Close()
	if err != nil {
		return nil, err
	}

	lock, err := Lock(s.cfg.DataDir, 5*time.Minute, 30*time.Minute)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()

	dst := TerrainPath(s.cfg.DataDir)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	if err := moveOrCopy(path, dst); err != nil {
		return nil, fmt.Errorf("terrain import: place archive: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(filepath.Dir(dst), terrainVersionName), []byte(version+"\n")); err != nil {
		return nil, err
	}
	info, err := ReadTerrain(s.cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("terrain import: post-write validation failed: %w", err)
	}
	s.log.Info("imported terrain", "version", info.Version, "maxZoom", info.MaxZoom)

	if s.kube != nil && s.cfg.DeploymentName != "" {
		if err := s.kube.PatchDeploymentTerrainVersion(ctx, s.cfg.Namespace, s.cfg.DeploymentName, info.Version); err != nil {
			s.log.Warn("could not trigger pod recreate after terrain import", "err", err.Error())
		}
	}
	return info, nil
}
