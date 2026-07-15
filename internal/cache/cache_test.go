package cache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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
