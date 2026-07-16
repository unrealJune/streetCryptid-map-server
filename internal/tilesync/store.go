package tilesync

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	releasesDir  = "releases"
	stagingDir   = "staging"
	currentLink  = "current"
	activeFile   = "active.json"
	pendingFile  = "pending.json"
	lockFile     = ".lock"
	pmtilesName  = "planet.pmtiles"
	manifestName = "manifest.json"
	sigName      = "manifest.sig"
)

// Store manages verified PMTiles releases on the shared tile volume.
type Store struct {
	root string
	pub  ed25519.PublicKey
}

// NewStore roots a store at dataDir with the manifest verification key.
func NewStore(dataDir string, pub ed25519.PublicKey) *Store {
	return &Store{root: dataDir, pub: pub}
}

// ActivePointer records which release is live and when it was activated. The API
// reads the active release's manifest for its dataset version, so it does not
// depend on the filesystem symlink.
type ActivePointer struct {
	Version     string `json:"version"`
	SafeDir     string `json:"safe_dir"`
	ActivatedAt string `json:"activated_at"`
}

// PendingMarker is written by the updater when a newer verified release is
// staged. The next init container activates it before Martin starts.
type PendingMarker struct {
	Version  string `json:"version"`
	SafeDir  string `json:"safe_dir"`
	StagedAt string `json:"staged_at"`
}

func (s *Store) releasesPath() string { return filepath.Join(s.root, releasesDir) }
func (s *Store) stagingPath() string  { return filepath.Join(s.root, stagingDir) }
func (s *Store) currentPath() string  { return filepath.Join(s.root, currentLink) }

func (s *Store) releaseDir(safe string) string {
	return filepath.Join(s.releasesPath(), safe)
}

// EnsureLayout creates the directory skeleton.
func (s *Store) EnsureLayout() error {
	for _, d := range []string{s.root, s.releasesPath(), s.stagingPath()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// safeVersion maps a manifest version (which may contain ':' from RFC3339) to a
// filesystem-safe directory name. The canonical version is preserved in the
// release's manifest.json.
func safeVersion(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// PMTilesPathFor returns the on-disk PMTiles path for a release dir name.
func (s *Store) PMTilesPathFor(safe string) string {
	return filepath.Join(s.releaseDir(safe), pmtilesName)
}

// WriteRelease stores a verified artifact + manifest + signature into a
// versioned release directory using atomic renames. artifactPart is the
// already-verified .part file, moved into place.
func (s *Store) WriteRelease(m *Manifest, manifestBytes, sig, artifactPart string) error {
	safe := safeVersion(m.Version)
	dir := s.releaseDir(safe)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Move the verified artifact into place atomically (same filesystem).
	dst := filepath.Join(dir, pmtilesName)
	if err := os.Rename(artifactPart, dst); err != nil {
		return fmt.Errorf("write release: move artifact: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, manifestName), []byte(manifestBytes)); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, sigName), []byte(sig)); err != nil {
		return err
	}
	return nil
}

// ValidateRelease re-verifies a release directory end to end: signature, manifest
// structure, artifact digest, and PMTiles header against the manifest.
func (s *Store) ValidateRelease(safe string) (*Manifest, error) {
	dir := s.releaseDir(safe)
	mb, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	// A signature is required only when a verification key is configured. Locally
	// baked releases (bake mode, no pub key) are unsigned and carry no sig file.
	if s.pub != nil {
		sig, err := os.ReadFile(filepath.Join(dir, sigName))
		if err != nil {
			return nil, err
		}
		if err := VerifySignature(s.pub, mb, sig); err != nil {
			return nil, err
		}
	}
	m, err := ParseManifest(mb)
	if err != nil {
		return nil, err
	}
	art := filepath.Join(dir, pmtilesName)
	if err := verifyDigest(art, m); err != nil {
		return nil, err
	}
	if err := VerifyPMTilesAgainstManifest(art, m); err != nil {
		return nil, err
	}
	return m, nil
}

// Activate points current at the release and writes the active pointer. Both
// steps are atomic; the active pointer is the API's source of truth and the
// symlink is what Martin reads.
func (s *Store) Activate(m *Manifest) error {
	safe := safeVersion(m.Version)
	if _, err := os.Stat(s.releaseDir(safe)); err != nil {
		return fmt.Errorf("activate: release %q missing: %w", safe, err)
	}
	if err := s.swapSymlink(safe); err != nil {
		return err
	}
	ptr := ActivePointer{Version: m.Version, SafeDir: safe, ActivatedAt: nowRFC3339()}
	raw, _ := json.MarshalIndent(ptr, "", "  ")
	return writeFileAtomic(filepath.Join(s.root, activeFile), raw)
}

// swapSymlink atomically repoints current -> releases/<safe>. It writes a temp
// symlink then renames over the old one so readers never see a missing link.
func (s *Store) swapSymlink(safe string) error {
	target := filepath.Join(releasesDir, safe) // relative target keeps the volume portable
	tmp := s.currentPath() + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		if runtime.GOOS == "windows" {
			// Dev/test on Windows without symlink privilege: fall back to the
			// active pointer only. Production (Linux) requires the symlink.
			return nil
		}
		return err
	}
	// On Linux, rename over the old symlink is atomic. Windows refuses to
	// rename onto an existing name, so fall back to remove-then-rename there.
	if err := os.Rename(tmp, s.currentPath()); err != nil {
		if runtime.GOOS != "windows" {
			return err
		}
		os.Remove(s.currentPath())
		return os.Rename(tmp, s.currentPath())
	}
	return nil
}

// ReadActive returns the manifest of the currently active release, if any.
func (s *Store) ReadActive() (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(s.root, activeFile))
	if err != nil {
		return nil, err
	}
	var ptr ActivePointer
	if err := json.Unmarshal(raw, &ptr); err != nil {
		return nil, err
	}
	return s.ValidateRelease(ptr.SafeDir)
}

// ReadActivePointer returns the raw active pointer without full re-validation.
func (s *Store) ReadActivePointer() (*ActivePointer, error) {
	raw, err := os.ReadFile(filepath.Join(s.root, activeFile))
	if err != nil {
		return nil, err
	}
	var ptr ActivePointer
	if err := json.Unmarshal(raw, &ptr); err != nil {
		return nil, err
	}
	return &ptr, nil
}

// WritePending records a staged pending release for the next init container.
func (s *Store) WritePending(m *Manifest) error {
	pm := PendingMarker{Version: m.Version, SafeDir: safeVersion(m.Version), StagedAt: nowRFC3339()}
	raw, _ := json.MarshalIndent(pm, "", "  ")
	return writeFileAtomic(filepath.Join(s.root, pendingFile), raw)
}

// ReadPending returns the pending marker, or nil if none.
func (s *Store) ReadPending() (*PendingMarker, error) {
	raw, err := os.ReadFile(filepath.Join(s.root, pendingFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pm PendingMarker
	if err := json.Unmarshal(raw, &pm); err != nil {
		return nil, err
	}
	return &pm, nil
}

// ClearPending removes the pending marker after activation.
func (s *Store) ClearPending() error {
	err := os.Remove(filepath.Join(s.root, pendingFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// StagingPart returns the resumable .part path for a version.
func (s *Store) StagingPart(m *Manifest) string {
	return filepath.Join(s.stagingPath(), safeVersion(m.Version)+".part")
}

// ListReleases returns release dir names sorted oldest-first by mtime.
func (s *Store) ListReleases() ([]string, error) {
	ents, err := os.ReadDir(s.releasesPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	type re struct {
		name string
		mod  time.Time
	}
	var list []re
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, re{e.Name(), fi.ModTime()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].mod.Before(list[j].mod) })
	out := make([]string, len(list))
	for i, r := range list {
		out[i] = r.name
	}
	return out, nil
}

// Prune keeps the newest `retain` releases plus always the active one, removing
// the rest. Retention guarantees at least one rollback target.
func (s *Store) Prune(retain int) error {
	if retain < 1 {
		retain = 1
	}
	all, err := s.ListReleases()
	if err != nil {
		return err
	}
	active := ""
	if ptr, err := s.ReadActivePointer(); err == nil {
		active = ptr.SafeDir
	}
	// Keep the newest `retain` (tail of the oldest-first list) and the active.
	keep := map[string]bool{}
	for i := len(all) - 1; i >= 0 && len(keep) < retain; i-- {
		keep[all[i]] = true
	}
	if active != "" {
		keep[active] = true
	}
	for _, name := range all {
		if keep[name] {
			continue
		}
		os.RemoveAll(s.releaseDir(name))
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
