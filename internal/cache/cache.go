// Package cache stores immutable, checksummed representations in a single-writer
// disk LRU. Each atomic file contains its own index record, so restart needs no
// separately committed index. Only the owning process may write this directory.
package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxObjectBytes admits the largest stream: three SCB3 frames (SCB2 has two).
const maxObjectBytes = 3*(65*1024*1024+40) + 20
const maxEntries = 100000

var ErrDisabled = errors.New("cache: persistence disabled")
var ErrCorrupt = errors.New("cache: corrupt entry")
var ErrPinned = errors.New("cache: capacity is pinned by active readers")

type Cache struct {
	dir      string
	maxBytes int64
	mu       sync.Mutex
	entries  map[string]*entry
	total    int64
	inflight map[string]*call
}

type entry struct {
	Key      string `json:"key"`
	Size     int64  `json:"size"`
	Hash     string `json:"sha256"`
	lastUsed time.Time
	offset   int64
	readers  int
}

type call struct {
	wg   sync.WaitGroup
	data []byte
	err  error
}

// Reader owns an open, verified file. Eviction cannot invalidate this handle.
type Reader struct {
	*io.SectionReader
	file      *os.File
	closeOnce sync.Once
	closeErr  error
	release   func()
}

func (r *Reader) Close() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.file.Close()
		r.release()
	})
	return r.closeErr
}

func New(dir string, maxBytes int64) (*Cache, error) {
	c := &Cache{dir: dir, maxBytes: maxBytes, entries: make(map[string]*entry), inflight: make(map[string]*call)}
	if !c.enabled() {
		return c, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		name := file.Name()
		if file.IsDir() {
			continue
		}
		// Discard only this cache's uncommitted files and legacy unindexed data.
		if strings.HasPrefix(name, ".bundle-") || strings.HasSuffix(name, ".scb1gz") {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return nil, err
			}
			continue
		}
		if !strings.HasSuffix(name, ".bundle") {
			continue
		}
		f, e, err := inspect(filepath.Join(dir, name))
		if err == nil {
			err = f.Close()
		}
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				return nil, err
			}
			slog.Warn("discarding corrupt bundle cache entry", "file", name, "err", err)
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return nil, err
			}
			continue
		}
		c.entries[e.Key] = e
		c.total += e.Size
	}
	if err := c.evictLocked(0); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Cache) enabled() bool { return c.dir != "" && c.maxBytes > 0 }
func fileFor(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".bundle"
}

func inspect(path string) (*os.File, *entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	e, err := verify(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if filepath.Base(path) != fileFor(e.Key) {
		f.Close()
		return nil, nil, ErrCorrupt
	}
	return f, e, nil
}

func verify(f *os.File) (*entry, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, ErrCorrupt
	}
	var header [8]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrCorrupt, err)
	}
	n := binary.BigEndian.Uint32(header[4:])
	if string(header[:4]) != "SCC1" || n == 0 || n > 4096 {
		return nil, ErrCorrupt
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(f, raw); err != nil {
		return nil, fmt.Errorf("%w: metadata: %v", ErrCorrupt, err)
	}
	var e entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, ErrCorrupt
	}
	e.offset = 8 + int64(n)
	if e.Key == "" || e.Size < 0 || e.Size > maxObjectBytes || fi.Size() != e.offset+e.Size {
		return nil, ErrCorrupt
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	if hex.EncodeToString(h.Sum(nil)) != e.Hash {
		return nil, ErrCorrupt
	}
	e.lastUsed = fi.ModTime()
	return &e, nil
}

// Open checks integrity before returning a seekable, payload-only file view.
// A missing key is os.ErrNotExist; corruption and IO failures are explicit.
func (c *Cache) Open(key string) (*Reader, error) {
	if !c.enabled() {
		return nil, os.ErrNotExist
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		return nil, os.ErrNotExist
	}
	path := filepath.Join(c.dir, fileFor(key))
	f, checked, err := inspect(path)
	if err != nil {
		if errors.Is(err, ErrCorrupt) || errors.Is(err, os.ErrNotExist) {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return nil, removeErr
			}
			delete(c.entries, key)
			c.total -= e.Size
		}
		return nil, err
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		f.Close()
		return nil, err
	}
	e.lastUsed = now
	e.readers++
	return &Reader{SectionReader: io.NewSectionReader(f, checked.offset, checked.Size), file: f, release: func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		e.readers--
	}}, nil
}

// Get is for stage reuse; HTTP cache hits should use Open instead.
func (c *Cache) Get(key string) ([]byte, bool) {
	f, err := c.Open(key)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Error("bundle cache read failed", "err", err)
		}
		return nil, false
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		slog.Error("bundle cache read failed", "err", err)
		return nil, false
	}
	return data, true
}

// Put commits a complete immutable value, syncing before the atomic rename.
// Oversized objects and disabled persistence are intentionally not stored.
func (c *Cache) Put(key string, data []byte) error {
	if !c.enabled() || int64(len(data)) > c.maxBytes {
		return nil
	}
	if len(data) > maxObjectBytes || key == "" {
		return errors.New("cache: invalid object")
	}
	sum := sha256.Sum256(data)
	e := &entry{Key: key, Size: int64(len(data)), Hash: hex.EncodeToString(sum[:]), lastUsed: time.Now()}
	meta, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(meta) > 4096 {
		return errors.New("cache: key too long")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Same namespace always denotes the same bytes, never replace an open file.
	if old := c.entries[key]; old != nil {
		if old.Hash != e.Hash {
			return errors.New("cache: immutable key collision")
		}
		return nil
	}
	if err := c.evictLocked(e.Size); err != nil {
		return err
	}
	f, err := os.CreateTemp(c.dir, ".bundle-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Error("cache temporary cleanup failed", "err", err)
		}
	}()
	var header [8]byte
	copy(header[:], "SCC1")
	binary.BigEndian.PutUint32(header[4:], uint32(len(meta)))
	_, err = f.Write(header[:])
	if err == nil {
		_, err = f.Write(meta)
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, filepath.Join(c.dir, fileFor(key))); err != nil {
		return err
	}
	c.entries[key] = e
	c.total += e.Size
	return nil
}

func (c *Cache) evictLocked(incoming int64) error {
	all := make([]*entry, 0, len(c.entries))
	available := c.maxBytes - c.total
	for _, e := range c.entries {
		if e.readers == 0 {
			all = append(all, e)
			available += e.Size
		}
	}
	if available < incoming || (len(c.entries) >= maxEntries && len(all) == 0) {
		return ErrPinned
	}
	sort.Slice(all, func(i, j int) bool { return all[i].lastUsed.Before(all[j].lastUsed) })
	for _, e := range all {
		if c.total+incoming <= c.maxBytes && len(c.entries) < maxEntries {
			break
		}
		if err := os.Remove(filepath.Join(c.dir, fileFor(e.Key))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		delete(c.entries, e.Key)
		c.total -= e.Size
	}
	return nil
}

func (c *Cache) Do(key string, build func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if cl, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		cl.wg.Wait()
		return cl.data, cl.err
	}
	cl := &call{}
	cl.wg.Add(1)
	c.inflight[key] = cl
	c.mu.Unlock()
	defer func() {
		panicked := recover()
		if panicked != nil {
			cl.err = errors.New("cache: build panicked")
		}
		c.mu.Lock()
		delete(c.inflight, key)
		c.mu.Unlock()
		cl.wg.Done()
		if panicked != nil {
			panic(panicked)
		}
	}()
	cl.data, cl.err = build()
	return cl.data, cl.err
}

func (c *Cache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}
