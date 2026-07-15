package tilesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Downloader fetches and verifies PMTiles artifacts with HTTP Range resume.
type Downloader struct {
	http  *http.Client
	token string // optional bearer token for the artifact host
}

// NewDownloader builds a downloader. token may be empty for public artifacts.
func NewDownloader(client *http.Client, token string) *Downloader {
	if client == nil {
		client = http.DefaultClient
	}
	return &Downloader{http: client, token: strings.TrimSpace(token)}
}

// FetchManifest downloads manifest.json and manifest.sig relative to the
// manifest URL. sigURL is derived by swapping the trailing filename.
func (d *Downloader) FetchBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	d.auth(req)
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
}

func (d *Downloader) auth(req *http.Request) {
	if d.token != "" {
		req.Header.Set("Authorization", "Bearer "+d.token)
	}
}

// DownloadArtifact downloads m.URL into partPath, resuming from any existing
// bytes via a Range request, then verifies size and SHA-256. It checks free
// space before writing. The completed file is left at partPath for the caller
// to atomically rename into place.
func (d *Downloader) DownloadArtifact(ctx context.Context, m *Manifest, partPath string) error {
	var start int64
	if fi, err := os.Stat(partPath); err == nil {
		start = fi.Size()
		if start > m.Size {
			// Corrupt/oversized partial; restart clean.
			os.Remove(partPath)
			start = 0
		}
	}

	remaining := m.Size - start
	if err := ensureFreeSpace(dirOf(partPath), remaining); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return err
	}
	d.auth(req)
	if start > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case start > 0 && resp.StatusCode == http.StatusPartialContent:
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		// Server ignored Range (or nothing to resume); rewrite from scratch.
		start = 0
		flags |= os.O_TRUNC
	default:
		return fmt.Errorf("download: unexpected status %d", resp.StatusCode)
	}

	f, err := os.OpenFile(partPath, flags, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if start+written != m.Size {
		return fmt.Errorf("download: size %d != manifest %d", start+written, m.Size)
	}
	return verifyDigest(partPath, m)
}

// verifyDigest streams the file and checks its size and SHA-256 against m.
func verifyDigest(path string, m *Manifest) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() != m.Size {
		return fmt.Errorf("verify: size %d != manifest %d", fi.Size(), m.Size)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, m.SHA256) {
		return fmt.Errorf("verify: sha256 %s != manifest %s", got, m.SHA256)
	}
	return nil
}

func dirOf(path string) string {
	i := strings.LastIndexAny(path, `/\`)
	if i < 0 {
		return "."
	}
	return path[:i]
}
