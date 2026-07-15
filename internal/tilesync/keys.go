package tilesync

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
)

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpaceByte(b[start]) {
		start++
	}
	for end > start && isSpaceByte(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func decodeHexKey(raw []byte) (ed25519.PublicKey, error) {
	if len(raw) != hex.EncodedLen(ed25519.PublicKeySize) {
		return nil, fmt.Errorf("hex key: wrong length")
	}
	out := make([]byte, ed25519.PublicKeySize)
	if _, err := hex.Decode(out, raw); err != nil {
		return nil, err
	}
	return ed25519.PublicKey(out), nil
}
