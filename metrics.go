// Package metrics provides minimalist instrumentation for your applications in
// the form of counters and gauges.
//
// Counters
//
// A counter is a monotonically-increasing, unsigned, 64-bit integer used to
// represent the number of times an event has occurred. By tracking the deltas
// between measurements of a counter over intervals of time, an aggregation
// layer can derive rates, acceleration, etc.
//
// Gauges
//
// A gauge returns instantaneous measurements of something using 64-bit floating
// point values.
//
// Histograms
//
// A histogram tracks the distribution of a stream of values (e.g. the number of
// milliseconds it takes to handle requests), adding gauges for the values at
// meaningful quantiles: 50th, 75th, 90th, 95th, 99th, 99.9th.
//
// Reporting
//
// Measurements from counters and gauges are available as expvars. Your service
// should return its expvars from an HTTP endpoint (i.e., /debug/vars) as a JSON
// object.
package metrics

import (
	"expvar"
	"sync"
	"time"

	"github.com/codahale/hdrhistogram/hdr"
)

// A Counter is a monotonically increasing unsigned integer.
//
// Use a counter to derive rates (e.g., record total number of requests, derive
// requests per second).
type Counter string

// Add increments the counter by one.
func (c Counter) Add() {
	c.AddN(1)
}

// AddN increments the counter by N.
func (c Counter) AddN(delta uint64) {
	cm.Lock()
	counters[string(c)] += delta
	cm.Unlock()
}

// SetFunc sets the counter's value to the lazily-called return value of the
// given function.
func (c Counter) SetFunc(f func() uint64) {
	cm.Lock()
	defer cm.Unlock()

	counterFuncs[string(c)] = f
}

// SetBatchFunc sets the counter's value to the lazily-called return value of
// the given function, with an additional initializer function for a related
// batch of counters, all of which are keyed by an arbitrary value.
func (c Counter) SetBatchFunc(key interface{}, init func(), f func() uint64) {
	cm.Lock()
	defer cm.Unlock()

	gm.Lock()
	defer gm.Unlock()

	counterFuncs[string(c)] = f
	if _, ok := inits[key]; !ok {
		inits[key] = init
	}
}

// A Gauge is an instantaneous measurement of a value.
//
// Use a gauge to track metrics which increase and decrease (e.g., amount of
// free memory).
type Gauge string

// Set the gauge's value to the given value.
func (g Gauge) Set(value float64) {
	gm.Lock()
	defer gm.Unlock()

	gauges[string(g)] = func() float64 {
		return value
	}
}

// SetFunc sets the gauge's value to the lazily-called return value of the given
// function.
func (g Gauge) SetFunc(f func() float64) {
	gm.Lock()
	defer gm.Unlock()

	gauges[string(g)] = f
}

// SetBatchFunc sets the gauge's value to the lazily-called return value of the
// given function, with an additional initializer function for a related batch
// of gauges, all of which are keyed by an arbitrary value.
func (g Gauge) SetBatchFunc(key interface{}, init func(), f func() float64) {
	gm.Lock()
	defer gm.Unlock()

	gauges[string(g)] = f
	if _, ok := inits[key]; !ok {
		inits[key] = init
	}
}

// Reset removes all existing counters and gauges.
func Reset() {
	cm.Lock()
	defer cm.Unlock()

	gm.Lock()
	defer gm.Unlock()

	hm.Lock()
	defer hm.Unlock()

	counters = make(map[string]uint64)
	counterFuncs = make(map[string]func() uint64)
	gauges = make(map[string]func() float64)
	histograms = make(map[string]*Histogram)
	inits = make(map[interface{}]func())
}

// Snapshot returns a copy of the values of all registered counters and gauges.
func Snapshot() (c map[string]uint64, g map[string]float64) {
	cm.Lock()
	defer cm.Unlock()

	gm.Lock()
	defer gm.Unlock()

	hm.Lock()
	defer hm.Unlock()

	for _, init := range inits {
		init()
	}

	c = make(map[string]uint64, len(counters)+len(counterFuncs))
	for n, v := range counters {
		c[n] = v
	}

	for n, f := range counterFuncs {
		c[n] = f()
	}

	g = make(map[string]float64, len(gauges))
	for n, f := range gauges {
		g[n] = f()
	}

	return
}

// NewHistogram returns a windowed HDR histogram which drops data older than
// five minutes. The returned histogram is safe to use from multiple goroutines.
//
// Use a histogram to track the distribution of a stream of values (e.g., the
// latency associated with HTTP requests).
func NewHistogram(name string, minValue, maxValue int64, sigfigs int) *Histogram {
	hm.Lock()
	defer hm.Unlock()

	if _, ok := histograms[name]; ok {
		panic(name + " already exists")
	}

	hist := &Histogram{
		hist: hdr.NewWindowedHistogram(5, minValue, maxValue, sigfigs),
	}
	histograms[name] = hist

	Gauge(name+".P50").SetBatchFunc(hname(name), hist.merge, hist.valueAt(50))
	Gauge(name+".P75").SetBatchFunc(hname(name), hist.merge, hist.valueAt(75))
	Gauge(name+".P90").SetBatchFunc(hname(name), hist.merge, hist.valueAt(90))
	Gauge(name+".P95").SetBatchFunc(hname(name), hist.merge, hist.valueAt(95))
	Gauge(name+".P99").SetBatchFunc(hname(name), hist.merge, hist.valueAt(99))
	Gauge(name+".P999").SetBatchFunc(hname(name), hist.merge, hist.valueAt(99.9))

	return hist
}

type hname string // unexported to prevent collisions

// A Histogram measures the distribution of a stream of values.
type Histogram struct {
	hist *hdr.WindowedHistogram
	m    *hdr.Histogram
	rw   sync.RWMutex
}

// RecordValue records the given value, or returns an error if the value is out
// of range.
func (h *Histogram) RecordValue(v int64) error {
	h.rw.Lock()
	defer h.rw.Unlock()

	return h.hist.Current.RecordValue(v)
}

func (h *Histogram) rotate() {
	h.rw.Lock()
	defer h.rw.Unlock()

	h.hist.Rotate()
}

func (h *Histogram) merge() {
	h.rw.Lock()
	defer h.rw.Unlock()

	h.m = h.hist.Merge()
}

func (h *Histogram) valueAt(q float64) func() float64 {
	return func() float64 {
		h.rw.RLock()
		defer h.rw.RUnlock()

		if h.m == nil {
			return 0
		}

		return float64(h.m.ValueAtQuantile(q))
	}
}

var (
	counters     = make(map[string]uint64)
	counterFuncs = make(map[string]func() uint64)
	gauges       = make(map[string]func() float64)
	inits        = make(map[interface{}]func())
	histograms   = make(map[string]*Histogram)

	cm, gm, hm sync.Mutex
)

func init() {
	expvar.Publish("metrics", expvar.Func(func() interface{} {
		counters, gauges := Snapshot()
		return map[string]interface{}{
			"Counters": counters,
			"Gauges":   gauges,
		}
	}))

	go func() {
		for _ = range time.NewTicker(1 * time.Minute).C {
			hm.Lock()
			for _, h := range histograms {
				h.rotate()
			}
			hm.Unlock()
		}
	}()
}
