package runtimetest

import (
	"maps"
	"sync"
)

// Metric is one call a Metrics recorder received.
type Metric struct {
	// Method is "add", "set" or "observe".
	Method string
	Name   string
	Labels map[string]string
	Value  float64
}

// Metrics is a runtime.Recorder that keeps every call it receives, in order.
// It is safe for concurrent use.
type Metrics struct {
	mu    sync.Mutex
	calls []Metric
}

// Add keeps the call.
func (m *Metrics) Add(name string, labels map[string]string, value float64) error {
	m.keep("add", name, labels, value)
	return nil
}

// Set keeps the call.
func (m *Metrics) Set(name string, labels map[string]string, value float64) error {
	m.keep("set", name, labels, value)
	return nil
}

// Observe keeps the call.
func (m *Metrics) Observe(name string, labels map[string]string, value float64) error {
	m.keep("observe", name, labels, value)
	return nil
}

// Calls returns the calls received so far, in order.
func (m *Metrics) Calls() []Metric {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Metric, len(m.calls))
	for i, c := range m.calls {
		c.Labels = maps.Clone(c.Labels)
		out[i] = c
	}
	return out
}

func (m *Metrics) keep(method, name string, labels map[string]string, value float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, Metric{Method: method, Name: name, Labels: maps.Clone(labels), Value: value})
}
