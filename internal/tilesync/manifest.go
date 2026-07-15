// Package tilesync bootstraps and updates the map dataset from a signed remote
// artifact manifest. It never bakes the planet into an image: it verifies and
// activates PMTiles releases on a shared volume and, as an updater, restarts the
// pod so a fresh init container activates the pending release before Martin.
package tilesync

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SchemaVersion is the only manifest schema this build understands.
const SchemaVersion = 1

// Manifest is the signed description of one PMTiles release. manifest.sig is an
// Ed25519 signature over the exact manifest.json bytes.
type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	Version       string `json:"version"`
	URL           string `json:"url"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	MinZoom       int    `json:"min_zoom"`
	MaxZoom       int    `json:"max_zoom"`
	TileSchema    string `json:"tile_schema"`
	CreatedAt     string `json:"created_at"`
}

var (
	// ErrBadSchema is returned for an unsupported schema_version.
	ErrBadSchema = errors.New("manifest: unsupported schema_version")
	// ErrBadSignature is returned when the Ed25519 signature does not verify.
	ErrBadSignature = errors.New("manifest: signature verification failed")
)

// ParseManifest decodes and structurally validates manifest bytes. It does not
// verify the signature; call VerifySignature separately with the raw bytes.
func ParseManifest(raw []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: decode: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if m.SchemaVersion != SchemaVersion {
		return ErrBadSchema
	}
	if m.Version == "" {
		return errors.New("manifest: empty version")
	}
	if !isHTTPS(m.URL) {
		return errors.New("manifest: url must be https")
	}
	if len(m.SHA256) != 64 || !isHex(m.SHA256) {
		return errors.New("manifest: sha256 must be 64 hex chars")
	}
	if m.Size <= 0 {
		return errors.New("manifest: size must be positive")
	}
	if m.MinZoom < 0 || m.MaxZoom < m.MinZoom || m.MaxZoom > 24 {
		return errors.New("manifest: invalid zoom range")
	}
	if m.TileSchema == "" {
		return errors.New("manifest: empty tile_schema")
	}
	if _, err := time.Parse(time.RFC3339, m.CreatedAt); err != nil {
		return fmt.Errorf("manifest: created_at not RFC3339: %w", err)
	}
	return nil
}

// VerifySignature checks that sig is a valid Ed25519 signature over the exact
// manifest bytes (not a re-serialization). This binds every field, including
// the download URL and hash, to the offline signing key.
func VerifySignature(pub ed25519.PublicKey, manifestBytes, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("manifest: bad public key size")
	}
	if !ed25519.Verify(pub, manifestBytes, sig) {
		return ErrBadSignature
	}
	return nil
}

func isHTTPS(u string) bool {
	return len(u) > 8 && u[:8] == "https://"
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
