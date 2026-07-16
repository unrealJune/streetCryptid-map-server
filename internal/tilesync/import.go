package tilesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ImportLocal installs a locally-baked PMTiles file as the active release. It is
// the in-cluster-bake counterpart to the signed-download path: the bake Job (a
// throttled Planetiler run) produces the file on the tile volume and this call
// verifies its PMTiles header, synthesizes a `local:` manifest, activates it,
// and prunes old releases. No network fetch and no signature are involved —
// trust comes from the file having been produced inside the cluster.
func (s *Syncer) ImportLocal(ctx context.Context, pmtilesPath, version string) (*Manifest, error) {
	if version == "" {
		version = nowRFC3339()
	}
	if err := s.st.EnsureLayout(); err != nil {
		return nil, err
	}
	lock, err := Lock(s.cfg.DataDir, 5*time.Minute, 30*time.Minute)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()

	m, err := s.st.ImportLocalRelease(pmtilesPath, version)
	if err != nil {
		return nil, err
	}
	if err := s.st.Activate(m); err != nil {
		return nil, err
	}
	if err := s.st.Prune(s.cfg.RetainReleases); err != nil {
		s.log.Warn("prune after import failed", "err", err.Error())
	}
	s.log.Info("imported and activated local release", "version", m.Version, "zoom", fmt.Sprintf("%d-%d", m.MinZoom, m.MaxZoom))

	// If running in-cluster with a Deployment target, recreate the pod so Martin
	// reopens the new dataset (Recreate strategy). Best-effort: the release is
	// already active regardless.
	if s.kube != nil && s.cfg.DeploymentName != "" {
		if err := s.kube.PatchDeploymentPendingVersion(ctx, s.cfg.Namespace, s.cfg.DeploymentName, m.Version); err != nil {
			s.log.Warn("could not trigger pod recreate after import", "err", err.Error())
		} else {
			s.log.Info("patched deployment to pick up new dataset", "deployment", s.cfg.DeploymentName)
		}
	}
	return m, nil
}

// ImportLocalRelease verifies a baked PMTiles file, builds a `local:` manifest
// from its header, and writes it into a versioned release directory. The file is
// moved into place (or copied if it lives on another filesystem). The caller
// holds the volume lock.
func (s *Store) ImportLocalRelease(pmtilesPath, version string) (*Manifest, error) {
	info, err := readPMTilesInfo(pmtilesPath)
	if err != nil {
		return nil, fmt.Errorf("import: read pmtiles: %w", err)
	}
	size, sha, err := hashFile(pmtilesPath)
	if err != nil {
		return nil, err
	}
	schema := info.TileSchema
	if schema == "" {
		if info.HasVectorLayers {
			schema = "openmaptiles"
		} else {
			return nil, fmt.Errorf("import: pmtiles has no recognizable vector schema")
		}
	}
	m := &Manifest{
		SchemaVersion: SchemaVersion,
		Version:       version,
		URL:           "local:planet",
		SHA256:        sha,
		Size:          size,
		MinZoom:       info.MinZoom,
		MaxZoom:       info.MaxZoom,
		TileSchema:    schema,
		CreatedAt:     nowRFC3339(),
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("import: synthesized manifest invalid: %w", err)
	}

	safe := safeVersion(version)
	dir := s.releaseDir(safe)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	dst := filepath.Join(dir, pmtilesName)
	if err := moveOrCopy(pmtilesPath, dst); err != nil {
		return nil, fmt.Errorf("import: place artifact: %w", err)
	}
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := writeFileAtomic(filepath.Join(dir, manifestName), mb); err != nil {
		return nil, err
	}
	// Re-validate the finished release end to end (digest + PMTiles vs manifest).
	// Signature is skipped: a local release is unsigned and s.pub is nil in bake
	// mode.
	if _, err := s.ValidateRelease(safe); err != nil {
		return nil, fmt.Errorf("import: post-write validation failed: %w", err)
	}
	return m, nil
}

func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return 0, "", err
	}
	return fi.Size(), hex.EncodeToString(h.Sum(nil)), nil
}

// moveOrCopy renames src to dst, falling back to a streaming copy when the two
// live on different filesystems (the bake scratch volume vs the tile volume).
func moveOrCopy(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	os.Remove(src)
	return nil
}
