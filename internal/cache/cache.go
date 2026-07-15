// Package cache is the completed-bundle cache: atomic disk writes, dataset
// versioned keys, an LRU byte cap, and in-flight request de-duplication.
//
// Only fully assembled, size-checked, gzipped bundles are ever stored. A build
// that fails or is oversized never commits a partial entry.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Cache is a single-node disk LRU with an in-memory index. It is safe for
// concurrent use. A zero maxBytes or empty dir disables persistence; the
// singleflight de-duplication still applies.
type Cache struct {
	dir      string
	maxBytes int64

	mu       sync.Mutex
	entries  map[string]*entry // key -> metadata
	total    int64
	seq      uint64 // logical clock for LRU ordering
	inflight map[string]*call
}

type entry struct {
	file     string
	size     int64
	lastUsed uint64
}

type call struct {
	wg   sync.WaitGroup
	data []byte
	err  error
}

// New creates a cache rooted at dir with the given byte cap. If dir is empty or
// maxBytes <= 0 the cache does not persist to disk but still de-duplicates
// concurrent builds. Any pre-existing files in dir are discarded on startup so
// the byte accounting always matches what this process wrote.
func New(dir string, maxBytes int64) (*Cache, error) {
	c := &Cache{
		dir:      dir,
		maxBytes: maxBytes,
		entries:  make(map[string]*entry),
		inflight: make(map[string]*call),
	}
	if dir != "" && maxBytes > 0 {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		// Start clean: stale files from a prior process are untracked bytes.
		c.purgeDir()
	}
	return c, nil
}

func (c *Cache) enabled() bool { return c.dir != "" && c.maxBytes > 0 }

func (c *Cache) purgeDir() {
	matches, _ := filepath.Glob(filepath.Join(c.dir, "*.scb1gz"))
	for _, m := range matches {
		os.Remove(m)
	}
	tmp, _ := filepath.Glob(filepath.Join(c.dir, "*.tmp"))
	for _, m := range tmp {
		os.Remove(m)
	}
}

func fileFor(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".scb1gz"
}

// Get returns the stored gzipped bundle for key, or ok=false on a miss.
func (c *Cache) Get(key string) ([]byte, bool) {
	if !c.enabled() {
		return nil, false
	}
	c.mu.Lock()
	e, present := c.entries[key]
	if present {
		c.seq++
		e.lastUsed = c.seq
	}
	c.mu.Unlock()
	if !present {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(c.dir, e.file))
	if err != nil {
		// File vanished underneath us; drop the index entry.
		c.mu.Lock()
		if cur, ok := c.entries[key]; ok && cur == e {
			delete(c.entries, key)
			c.total -= e.size
		}
		c.mu.Unlock()
		return nil, false
	}
	return data, true
}

// Put stores data under key with an atomic write, then evicts LRU entries until
// the total is within the cap. A single object larger than the whole cap is not
// stored (it would evict everything and still not fit).
func (c *Cache) Put(key string, data []byte) {
	if !c.enabled() || int64(len(data)) > c.maxBytes {
		return
	}
	file := fileFor(key)
	full := filepath.Join(c.dir, file)
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[key]; ok {
		c.total -= old.size
	}
	c.seq++
	c.entries[key] = &entry{file: file, size: int64(len(data)), lastUsed: c.seq}
	c.total += int64(len(data))
	c.evictLocked()
}

func (c *Cache) evictLocked() {
	if c.total <= c.maxBytes {
		return
	}
	type kv struct {
		key string
		e   *entry
	}
	all := make([]kv, 0, len(c.entries))
	for k, e := range c.entries {
		all = append(all, kv{k, e})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].e.lastUsed < all[j].e.lastUsed })
	for _, it := range all {
		if c.total <= c.maxBytes {
			return
		}
		os.Remove(filepath.Join(c.dir, it.e.file))
		delete(c.entries, it.key)
		c.total -= it.e.size
	}
}

// Do runs build exactly once per key even under concurrent callers: the first
// caller builds while the rest wait and share the result. It de-duplicates the
// expensive bundle assembly regardless of whether persistence is enabled.
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

	cl.data, cl.err = build()

	c.mu.Lock()
	delete(c.inflight, key)
	c.mu.Unlock()
	cl.wg.Done()
	return cl.data, cl.err
}

// Bytes reports the current tracked total, for metrics.
func (c *Cache) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// ErrDisabled is returned by helpers that require a persistent cache.
var ErrDisabled = errors.New("cache: persistence disabled")
