package tilesync

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// synthPMTiles builds a minimal valid PMTiles v3 file with the given zoom range
// and OpenMapTiles-shaped metadata.
func synthPMTiles(minZoom, maxZoom int) []byte {
	meta := []byte(`{"name":"OpenMapTiles","vector_layers":[{"id":"water"},{"id":"waterway"}]}`)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(meta)
	zw.Close()

	hdr := make([]byte, pmtilesHeaderLen)
	copy(hdr[0:7], "PMTiles")
	hdr[7] = 3
	metaOff := uint64(pmtilesHeaderLen)
	binary.LittleEndian.PutUint64(hdr[24:32], metaOff)
	binary.LittleEndian.PutUint64(hdr[32:40], uint64(gz.Len()))
	hdr[97] = 2 // gzip internal compression
	hdr[100] = byte(minZoom)
	hdr[101] = byte(maxZoom)

	out := append([]byte{}, hdr...)
	out = append(out, gz.Bytes()...)
	// A little filler so the file has tile-data-ish size.
	out = append(out, bytes.Repeat([]byte{0xAB}, 256)...)
	return out
}

type artifactServer struct {
	srv      *httptest.Server
	manifest []byte
	sig      []byte
	pmtiles  []byte
	rangeReq int
}

func newArtifactServer(t *testing.T, priv ed25519.PrivateKey, version string, minZoom, maxZoom int) *artifactServer {
	t.Helper()
	as := &artifactServer{pmtiles: synthPMTiles(minZoom, maxZoom)}
	sum := sha256.Sum256(as.pmtiles)

	mux := http.NewServeMux()
	// TLS server so the manifest's https-only invariant is exercised for real.
	as.srv = httptest.NewTLSServer(mux)

	m := Manifest{
		SchemaVersion: 1,
		Version:       version,
		URL:           as.srv.URL + "/planet.pmtiles",
		SHA256:        hex.EncodeToString(sum[:]),
		Size:          int64(len(as.pmtiles)),
		MinZoom:       minZoom,
		MaxZoom:       maxZoom,
		TileSchema:    "openmaptiles",
		CreatedAt:     "2026-07-15T00:00:00Z",
	}
	mb, _ := json.Marshal(m)
	as.manifest = mb
	as.sig = ed25519.Sign(priv, mb)

	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) { w.Write(as.manifest) })
	mux.HandleFunc("/manifest.sig", func(w http.ResponseWriter, r *http.Request) { w.Write(as.sig) })
	mux.HandleFunc("/planet.pmtiles", func(w http.ResponseWriter, r *http.Request) {
		if rng := r.Header.Get("Range"); rng != "" {
			as.rangeReq++
			var start int
			fmt.Sscanf(rng, "bytes=%d-", &start)
			if start < len(as.pmtiles) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(as.pmtiles)-1, len(as.pmtiles)))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(as.pmtiles[start:])
				return
			}
		}
		w.Write(as.pmtiles)
	})
	return as
}

func newSyncerFor(t *testing.T, as *artifactServer, pub ed25519.PublicKey) *Syncer {
	return NewSyncer(Config{
		DataDir:        t.TempDir(),
		ManifestURL:    as.srv.URL + "/manifest.json",
		PublicKey:      pub,
		RetainReleases: 2,
		HTTPClient:     as.srv.Client(), // trusts the test TLS cert
	}, nil)
}

func TestManifestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	m := Manifest{
		SchemaVersion: 1, Version: "v1", URL: "https://x/y.pmtiles",
		SHA256: "ab" + hex.EncodeToString(make([]byte, 31)), Size: 10,
		MinZoom: 0, MaxZoom: 14, TileSchema: "openmaptiles", CreatedAt: "2026-07-15T00:00:00Z",
	}
	mb, _ := json.Marshal(m)
	sig := ed25519.Sign(priv, mb)
	if err := VerifySignature(pub, mb, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// Tamper one byte.
	mb2 := append([]byte{}, mb...)
	mb2[10] ^= 0xFF
	if err := VerifySignature(pub, mb2, sig); err == nil {
		t.Fatal("tampered manifest accepted")
	}
}

func TestParseManifestRejectsBad(t *testing.T) {
	for _, bad := range []string{
		`{"schema_version":2,"version":"v","url":"https://a/b","sha256":"` + hex.EncodeToString(make([]byte, 32)) + `","size":1,"min_zoom":0,"max_zoom":14,"tile_schema":"o","created_at":"2026-07-15T00:00:00Z"}`,
		`{"schema_version":1,"version":"v","url":"http://insecure","sha256":"` + hex.EncodeToString(make([]byte, 32)) + `","size":1,"min_zoom":0,"max_zoom":14,"tile_schema":"o","created_at":"2026-07-15T00:00:00Z"}`,
		`{"schema_version":1,"version":"v","url":"https://a/b","sha256":"tooshort","size":1,"min_zoom":0,"max_zoom":14,"tile_schema":"o","created_at":"2026-07-15T00:00:00Z"}`,
	} {
		if _, err := ParseManifest([]byte(bad)); err == nil {
			t.Fatalf("expected rejection for %s", bad)
		}
	}
}

func TestPMTilesVerify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.pmtiles")
	os.WriteFile(path, synthPMTiles(0, 14), 0o644)
	m := &Manifest{MinZoom: 0, MaxZoom: 14, TileSchema: "openmaptiles"}
	if err := VerifyPMTilesAgainstManifest(path, m); err != nil {
		t.Fatalf("valid pmtiles rejected: %v", err)
	}
	// Zoom mismatch.
	m2 := &Manifest{MinZoom: 0, MaxZoom: 13, TileSchema: "openmaptiles"}
	if err := VerifyPMTilesAgainstManifest(path, m2); err == nil {
		t.Fatal("zoom mismatch accepted")
	}
}

func TestBootstrapEmptyVolume(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	as := newArtifactServer(t, priv, "2026-07-15T00:00:00Z", 0, 14)
	defer as.srv.Close()
	s := newSyncerFor(t, as, pub)

	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	active, err := s.Store().ReadActive()
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	if active.Version != "2026-07-15T00:00:00Z" {
		t.Fatalf("active version %q", active.Version)
	}
}

func TestBootstrapValidLocalRemoteDown(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	as := newArtifactServer(t, priv, "v-a", 0, 14)
	s := newSyncerFor(t, as, pub)
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	as.srv.Close() // remote now unavailable

	// Second bootstrap must still succeed on the valid local release.
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap with remote down should succeed: %v", err)
	}
}

func TestBootstrapFailsWhenNoLocalNoRemote(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	s := NewSyncer(Config{
		DataDir:     t.TempDir(),
		ManifestURL: "https://127.0.0.1:1/manifest.json", // unreachable
		PublicKey:   pub,
	}, nil)
	if err := s.Bootstrap(context.Background()); err == nil {
		t.Fatal("bootstrap must fail with no local and no remote release")
	}
}

func TestBadSignatureLeavesActiveUntouched(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	as := newArtifactServer(t, priv, "v-good", 0, 14)
	defer as.srv.Close()
	s := newSyncerFor(t, as, pub)
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Corrupt the served signature and try an update step.
	as.sig[0] ^= 0xFF
	err := s.updateStep(context.Background())
	if err == nil {
		t.Fatal("update with bad signature should fail")
	}
	active, _ := s.Store().ReadActive()
	if active == nil || active.Version != "v-good" {
		t.Fatal("active release changed after failed update")
	}
	if pm, _ := s.Store().ReadPending(); pm != nil {
		t.Fatal("failed update left a pending marker")
	}
}

func TestResumableDownload(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	as := newArtifactServer(t, priv, "v-r", 0, 14)
	defer as.srv.Close()
	s := newSyncerFor(t, as, pub)

	m, _, _, err := s.fetchVerifiedManifest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.st.EnsureLayout()
	part := s.st.StagingPart(m)
	// Pre-seed a partial file (first 50 bytes) to force a Range resume.
	os.WriteFile(part, as.pmtiles[:50], 0o600)
	if err := s.dl.DownloadArtifact(context.Background(), m, part); err != nil {
		t.Fatalf("resume download: %v", err)
	}
	if as.rangeReq == 0 {
		t.Fatal("expected a Range request")
	}
	got, _ := os.ReadFile(part)
	if !bytes.Equal(got, as.pmtiles) {
		t.Fatal("resumed file bytes differ")
	}
}

func TestPruneRetainsReleases(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	as := newArtifactServer(t, priv, "v1", 0, 14)
	defer as.srv.Close()
	s := newSyncerFor(t, as, pub)
	s.st.EnsureLayout()

	// Create three release dirs with increasing mtime.
	versions := []string{"v1", "v2", "v3"}
	for i, v := range versions {
		dir := s.st.releaseDir(safeVersion(v))
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, pmtilesName), []byte("x"), 0o644)
		mod := time.Now().Add(time.Duration(i) * time.Second)
		os.Chtimes(dir, mod, mod)
	}
	if err := s.st.Prune(2); err != nil {
		t.Fatal(err)
	}
	list, _ := s.st.ListReleases()
	if len(list) != 2 {
		t.Fatalf("prune kept %d, want 2: %v", len(list), list)
	}
	// Oldest (v1) should be gone.
	if _, err := os.Stat(s.st.releaseDir("v1")); !os.IsNotExist(err) {
		t.Fatal("oldest release v1 should have been pruned")
	}
}

func TestImportLocalRelease(t *testing.T) {
	// Bake mode: no manifest URL, no public key. A baked file on the volume is
	// imported and activated with no network or signature.
	s := NewSyncer(Config{DataDir: t.TempDir(), RetainReleases: 2}, nil)
	s.st.EnsureLayout()

	// Simulate the bake writing output onto the tile volume's staging dir.
	baked := filepath.Join(s.st.stagingPath(), "planet.pmtiles")
	os.WriteFile(baked, synthPMTiles(0, 14), 0o644)

	m, err := s.ImportLocal(context.Background(), baked, "planet-2026-07-15")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !m.IsLocal() {
		t.Fatalf("imported manifest should be local, url=%s", m.URL)
	}
	active, err := s.Store().ReadActive()
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	if active.Version != "planet-2026-07-15" || active.MaxZoom != 14 {
		t.Fatalf("unexpected active release: %+v", active)
	}
	// The staged file was moved into the release dir.
	if _, err := os.Stat(baked); !os.IsNotExist(err) {
		t.Fatal("baked file should have been moved into the release")
	}
	if _, err := os.Stat(s.st.PMTilesPathFor(safeVersion("planet-2026-07-15"))); err != nil {
		t.Fatalf("release pmtiles missing: %v", err)
	}
}

func TestImportRejectsNonPMTiles(t *testing.T) {
	s := NewSyncer(Config{DataDir: t.TempDir(), RetainReleases: 2}, nil)
	s.st.EnsureLayout()
	bad := filepath.Join(t.TempDir(), "bad.pmtiles")
	os.WriteFile(bad, []byte("not a pmtiles file"), 0o644)
	if _, err := s.ImportLocal(context.Background(), bad, "v1"); err == nil {
		t.Fatal("import should reject a non-PMTiles file")
	}
}

func TestUpdateStagesAndPatches(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	as := newArtifactServer(t, priv, "v-1", 0, 14)
	defer as.srv.Close()
	s := newSyncerFor(t, as, pub)
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Publish a newer version on the same server.
	as2 := newArtifactServer(t, priv, "v-2", 0, 14)
	defer as2.srv.Close()
	s.cfg.ManifestURL = as2.srv.URL + "/manifest.json"

	// No kube client: staging succeeds but the patch step errors. The pending
	// marker must still be written so a manual/next reconcile can proceed.
	err := s.updateStep(context.Background())
	if err == nil {
		t.Fatal("expected error from missing kube client")
	}
	pm, _ := s.st.ReadPending()
	if pm == nil || pm.Version != "v-2" {
		t.Fatalf("pending marker not written for v-2: %+v", pm)
	}
	// Active is still v-1 until a pod recreate + init activation.
	ptr, _ := s.st.ReadActivePointer()
	if ptr.Version != "v-1" {
		t.Fatalf("active changed prematurely: %s", ptr.Version)
	}

	// Simulate the pod recreate: a fresh bootstrap activates the pending release.
	if err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	ptr, _ = s.st.ReadActivePointer()
	if ptr.Version != "v-2" {
		t.Fatalf("pending release not activated on bootstrap: %s", ptr.Version)
	}
}
