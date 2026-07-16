package tilesync

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config holds operational settings for bootstrap and the updater. The privacy
// constants are NOT here; this package only moves map data.
type Config struct {
	DataDir        string
	ManifestURL    string
	PublicKey      ed25519.PublicKey
	AuthToken      string
	UpdateInterval time.Duration
	RetainReleases int
	Namespace      string
	DeploymentName string
	Logger         *slog.Logger
	HTTPClient     *http.Client
	// AllowEmpty lets bootstrap succeed with no dataset yet (in-cluster bake
	// mode, before the first bake). The pod starts but stays not-Ready until a
	// bake produces data and triggers a recreate.
	AllowEmpty bool
}

// Syncer performs bootstrap and update operations against the tile volume.
type Syncer struct {
	cfg  Config
	log  *slog.Logger
	st   *Store
	dl   *Downloader
	kube *KubeClient // may be nil outside the cluster (bootstrap does not need it)
}

// NewSyncer builds a Syncer. kube may be nil for bootstrap-only use.
func NewSyncer(cfg Config, kube *KubeClient) *Syncer {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.RetainReleases < 2 {
		cfg.RetainReleases = 2
	}
	return &Syncer{
		cfg:  cfg,
		log:  cfg.Logger,
		st:   NewStore(cfg.DataDir, cfg.PublicKey),
		dl:   NewDownloader(cfg.HTTPClient, cfg.AuthToken),
		kube: kube,
	}
}

// Store exposes the release store (the API reads the active manifest for its
// dataset version).
func (s *Syncer) Store() *Store { return s.st }

// Bootstrap runs in the init container. It activates a pending release if one is
// staged, otherwise ensures a valid active release exists — fetching and
// verifying one from the remote manifest when the volume is empty. It returns an
// error (so the pod does not start) only when there is no valid local release
// AND no valid remote release can be obtained.
func (s *Syncer) Bootstrap(ctx context.Context) error {
	if err := s.st.EnsureLayout(); err != nil {
		return err
	}
	lock, err := Lock(s.cfg.DataDir, 2*time.Minute, 15*time.Minute)
	if err != nil {
		return err
	}
	defer lock.Unlock()

	// 1. A staged pending release takes priority: validate and activate it.
	if pm, _ := s.st.ReadPending(); pm != nil {
		if m, err := s.st.ValidateRelease(pm.SafeDir); err == nil {
			if err := s.st.Activate(m); err != nil {
				return err
			}
			s.st.ClearPending()
			s.st.Prune(s.cfg.RetainReleases)
			s.log.Info("activated pending release", "version", m.Version)
			return nil
		} else {
			s.log.Warn("pending release invalid; ignoring", "err", err.Error())
			s.st.ClearPending()
		}
	}

	// 2. A valid active release means we can start immediately.
	active, activeErr := s.st.ReadActive()
	if activeErr == nil {
		s.log.Info("active release valid", "version", active.Version)
		// Opportunistically refresh from remote, but never block startup on it.
		if err := s.tryFetchAndStage(ctx, active); err != nil {
			s.log.Warn("bootstrap refresh skipped", "err", err.Error())
		}
		return nil
	}

	// 3. No valid active release: we MUST obtain one from the remote manifest.
	// In-cluster bake mode: no remote manifest, and the first bake hasn't run
	// yet. Start the pod anyway (it stays not-Ready until the bake produces data
	// and triggers a recreate) so the release reconciles instead of stalling.
	if s.cfg.ManifestURL == "" && s.cfg.AllowEmpty {
		s.log.Warn("no dataset yet and bake mode is on; starting not-ready until a bake completes")
		return nil
	}

	s.log.Info("no valid active release; fetching from manifest")
	m, mb, sig, err := s.fetchVerifiedManifest(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: cannot obtain a valid release: %w", err)
	}
	if err := s.downloadWriteActivate(ctx, m, mb, sig); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	return nil
}

// tryFetchAndStage checks the remote for a newer release during bootstrap and
// stages it as pending without disturbing the active release. Best-effort.
func (s *Syncer) tryFetchAndStage(ctx context.Context, active *Manifest) error {
	m, mb, sig, err := s.fetchVerifiedManifest(ctx)
	if err != nil {
		return err
	}
	if m.Version == active.Version {
		return nil
	}
	if err := s.stageRelease(ctx, m, mb, sig); err != nil {
		return err
	}
	return s.st.WritePending(m)
}

// Watch is the updater loop. Each tick it fetches the signed manifest and, if a
// newer verified release exists, stages it and patches this Deployment so the
// pod is recreated (Recreate strategy) and the init container activates it.
func (s *Syncer) Watch(ctx context.Context) error {
	interval := s.cfg.UpdateInterval
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	// Run one check promptly on start.
	s.checkOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.checkOnce(ctx)
		}
	}
}

func (s *Syncer) checkOnce(ctx context.Context) {
	if err := s.updateStep(ctx); err != nil {
		// A failed update must leave the active release untouched. Log and retry
		// on the next tick.
		s.log.Warn("update check failed", "err", err.Error())
	}
}

func (s *Syncer) updateStep(ctx context.Context) error {
	m, mb, sig, err := s.fetchVerifiedManifest(ctx)
	if err != nil {
		return err
	}
	ptr, _ := s.st.ReadActivePointer()
	if ptr != nil && ptr.Version == m.Version {
		return nil // already current
	}
	if pm, _ := s.st.ReadPending(); pm != nil && pm.Version == m.Version {
		return nil // already staged; waiting for pod recreate
	}

	lock, err := Lock(s.cfg.DataDir, 30*time.Second, 15*time.Minute)
	if err != nil {
		return err
	}
	defer lock.Unlock()

	if err := s.stageRelease(ctx, m, mb, sig); err != nil {
		return err
	}
	if err := s.st.WritePending(m); err != nil {
		return err
	}
	s.log.Info("staged pending release", "version", m.Version)

	if s.kube == nil {
		return errors.New("update staged but no in-cluster client to trigger recreate")
	}
	if err := s.kube.PatchDeploymentPendingVersion(ctx, s.cfg.Namespace, s.cfg.DeploymentName, m.Version); err != nil {
		return err
	}
	s.log.Info("patched deployment to recreate pod", "deployment", s.cfg.DeploymentName)
	return nil
}

// stageRelease downloads (resumable) and fully verifies m into a release dir. It
// is idempotent: an already-present valid release short-circuits the download.
func (s *Syncer) stageRelease(ctx context.Context, m *Manifest, mb, sig string) error {
	safe := safeVersion(m.Version)
	if _, err := s.st.ValidateRelease(safe); err == nil {
		return nil // already have this release, verified
	}
	part := s.st.StagingPart(m)
	if err := s.dl.DownloadArtifact(ctx, m, part); err != nil {
		return err
	}
	// Move into a release dir, then re-validate the release end to end.
	if err := s.st.WriteRelease(m, mb, sig, part); err != nil {
		return err
	}
	if _, err := s.st.ValidateRelease(safe); err != nil {
		return fmt.Errorf("stage: post-write validation failed: %w", err)
	}
	return nil
}

// downloadWriteActivate assumes the caller already holds the volume lock.
func (s *Syncer) downloadWriteActivate(ctx context.Context, m *Manifest, mb, sig string) error {
	if err := s.stageRelease(ctx, m, mb, sig); err != nil {
		return err
	}
	if err := s.st.Activate(m); err != nil {
		return err
	}
	s.st.Prune(s.cfg.RetainReleases)
	s.log.Info("activated release", "version", m.Version)
	return nil
}

// fetchVerifiedManifest downloads manifest.json + manifest.sig and verifies the
// Ed25519 signature over the exact manifest bytes before parsing.
func (s *Syncer) fetchVerifiedManifest(ctx context.Context) (*Manifest, string, string, error) {
	if s.cfg.ManifestURL == "" {
		return nil, "", "", errors.New("no manifest URL configured")
	}
	mb, err := s.dl.FetchBytes(ctx, s.cfg.ManifestURL)
	if err != nil {
		return nil, "", "", err
	}
	sig, err := s.dl.FetchBytes(ctx, sigURLFor(s.cfg.ManifestURL))
	if err != nil {
		return nil, "", "", err
	}
	if s.cfg.PublicKey == nil {
		return nil, "", "", errors.New("no manifest public key configured")
	}
	if err := VerifySignature(s.cfg.PublicKey, mb, sig); err != nil {
		return nil, "", "", err
	}
	m, err := ParseManifest(mb)
	if err != nil {
		return nil, "", "", err
	}
	return m, string(mb), string(sig), nil
}

func sigURLFor(manifestURL string) string {
	if i := strings.LastIndex(manifestURL, "/"); i >= 0 {
		return manifestURL[:i+1] + sigName
	}
	return sigName
}

// LoadPublicKey reads an Ed25519 public key from a file. It accepts either raw
// 32 bytes or a hex/base64-ish text line; here we support raw and hex for
// simplicity and determinism.
func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw = trimSpace(raw)
	if len(raw) == ed25519.PublicKeySize {
		return ed25519.PublicKey(raw), nil
	}
	if pk, err := decodeHexKey(raw); err == nil {
		return pk, nil
	}
	return nil, fmt.Errorf("public key: expected %d raw bytes or 64 hex chars", ed25519.PublicKeySize)
}
