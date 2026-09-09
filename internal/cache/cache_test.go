package cache

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRestartRestoresIntegrityAndLRU(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir, 12)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a", "b", "c"} {
		if err := c.Put(key, bytes.Repeat([]byte(key), 4)); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	for i, key := range []string{"a", "b", "c"} {
		at := old.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(filepath.Join(dir, fileFor(key)), at, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("missing a")
	}
	restored, err := New(dir, 8)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Bytes() != 8 {
		t.Fatal(restored.Bytes())
	}
	if _, ok := restored.Get("b"); ok {
		t.Fatal("LRU ordering not restored")
	}
	for _, key := range []string{"a", "c"} {
		if got, ok := restored.Get(key); !ok || len(got) != 4 {
			t.Fatal("lost valid entry", key)
		}
	}
}

func TestCorruptionRestartAndLiveRead(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmtBool(restart), func(t *testing.T) {
			dir := t.TempDir()
			c, err := New(dir, 100)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Put("k", []byte("payload")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, fileFor("k"))
			f, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			fi, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt([]byte{0}, fi.Size()-1); err != nil {
				t.Fatal(err)
			}
			f.Close()
			if restart {
				c, err = New(dir, 100)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := c.Open("k"); !errors.Is(err, ErrCorrupt) {
					t.Fatalf("error = %v", err)
				}
			}
			if c.Bytes() != 0 {
				t.Fatal("corrupt entry still counted")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("corrupt file still present", err)
			}
			if err := c.Put("k", []byte("rebuilt")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func fmtBool(b bool) string {
	if b {
		return "restart"
	}
	return "live"
}

func TestInterruptedWriteAndIOErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".bundle-interrupted"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileFor("bad")), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	if c.Bytes() != 0 {
		t.Fatal("partial files restored")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("cleanup: %v %v", files, err)
	}
	// Replace the cache directory with a regular file: writes must surface IO failure.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("k", []byte("abc")); err == nil {
		t.Fatal("IO failure hidden")
	}
}

func TestOpenSurvivesEvictionAndImmutableKeys(t *testing.T) {
	c, err := New(t.TempDir(), 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put("a", []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("a", []byte("efgh")); err == nil {
		t.Fatal("immutable collision accepted")
	}
	f, err := c.Open("a")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := c.Put("b", []byte("efgh")); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("c", []byte("ijkl")); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("b"); ok {
		t.Fatal("unpinned LRU entry should be evicted")
	}
	var got [4]byte
	if _, err := f.ReadAt(got[:], 0); err != nil || string(got[:]) != "abcd" {
		t.Fatal("open reader invalidated", err)
	}

}

func TestPinnedCapacityIsBounded(t *testing.T) {
	c, err := New(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put("a", []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	f, err := c.Open("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put("b", []byte("efgh")); !errors.Is(err, ErrPinned) {
		t.Fatalf("pinned capacity: %v", err)
	}
	if c.Bytes() != 4 {
		t.Fatal("pinned bytes not accounted")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("b", []byte("efgh")); err != nil {
		t.Fatal(err)
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	c, err := New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	c.Put("k", []byte("hello"))
	got, ok := c.Get("k")
	if !ok || string(got) != "hello" {
		t.Fatalf("get = %q %v", got, ok)
	}
	if _, ok := c.Get("missing"); ok {
		t.Fatal("unexpected hit")
	}
}

func TestLRUEviction(t *testing.T) {
	// Cap of 10 bytes; each value is 4 bytes so at most 2 fit.
	c, err := New(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	c.Put("a", []byte("aaaa"))
	c.Put("b", []byte("bbbb"))
	// Touch a so b becomes least-recently-used.
	c.Get("a")
	c.Put("c", []byte("cccc")) // total would be 12 > 10 -> evict LRU (b)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should survive")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c should be present")
	}
	if c.Bytes() > 10 {
		t.Fatalf("total %d exceeds cap", c.Bytes())
	}
}

func TestOversizedNotStored(t *testing.T) {
	c, _ := New(t.TempDir(), 4)
	c.Put("big", []byte("aaaaaaaa"))
	if _, ok := c.Get("big"); ok {
		t.Fatal("oversized object should not be stored")
	}
}

func TestSingleFlightDedup(t *testing.T) {
	c, _ := New(t.TempDir(), 1<<20)
	var builds int64
	release := make(chan struct{})
	entered := make(chan struct{}, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entered <- struct{}{}
			c.Do("same", func() ([]byte, error) {
				atomic.AddInt64(&builds, 1)
				<-release // hold the build so concurrent callers coalesce
				return []byte("x"), nil
			})
		}()
	}
	// Wait until all callers have entered before releasing the single build.
	for i := 0; i < 20; i++ {
		<-entered
	}
	// Give latecomers a moment to register on the in-flight call, then release.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := atomic.LoadInt64(&builds); got < 1 || got > 5 {
		t.Fatalf("build ran %d times; dedup ineffective", got)
	}
}

func TestDisabledCache(t *testing.T) {
	c, _ := New("", 0)
	c.Put("k", []byte("v"))
	if _, ok := c.Get("k"); ok {
		t.Fatal("disabled cache should not persist")
	}
	// Do still de-duplicates.
	out, err := c.Do("k", func() ([]byte, error) { return []byte("built"), nil })
	if err != nil || string(out) != "built" {
		t.Fatalf("Do failed: %q %v", out, err)
	}
}
