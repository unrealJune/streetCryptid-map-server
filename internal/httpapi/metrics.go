package httpapi

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Metrics is a tiny Prometheus text-exposition registry. Using stdlib keeps the
// image free of client libraries. Only counters and a couple of gauges are
// needed; label cardinality is fixed and privacy-safe (no coordinates).
type Metrics struct {
	mu       sync.Mutex
	counters map[string]*int64
	gauges   map[string]func() float64
}

func newMetrics() *Metrics {
	return &Metrics{
		counters: make(map[string]*int64),
		gauges:   make(map[string]func() float64),
	}
}

func (m *Metrics) counter(name string) *int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.counters[name]; ok {
		return c
	}
	var v int64
	c := &v
	m.counters[name] = c
	return c
}

func (m *Metrics) inc(name string) { atomic.AddInt64(m.counter(name), 1) }

func (m *Metrics) registerGauge(name string, f func() float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[name] = f
}

// WriteTo renders the registry in Prometheus text format.
func (m *Metrics) render() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	names := make([]string, 0, len(m.counters))
	for n := range m.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "# TYPE %s counter\n%s %d\n", n, n, atomic.LoadInt64(m.counters[n]))
	}
	gnames := make([]string, 0, len(m.gauges))
	for n := range m.gauges {
		gnames = append(gnames, n)
	}
	sort.Strings(gnames)
	for _, n := range gnames {
		fmt.Fprintf(&b, "# TYPE %s gauge\n%s %g\n", n, n, m.gauges[n]())
	}
	return b.String()
}
