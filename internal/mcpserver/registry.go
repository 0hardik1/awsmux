package mcpserver

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// registry tracks in-flight async executions so get_aws_execution can report
// progress and cancel_aws_execution can stop them. Entries are removed once
// the execution is persisted, so absence means unknown or finished.
type registry struct {
	mu      sync.Mutex
	entries map[string]*execEntry
}

type execEntry struct {
	cancel    context.CancelFunc
	startedAt time.Time
	completed int // target results delivered so far
}

// execSnapshot is a copy-out view of a running entry.
type execSnapshot struct {
	startedAt time.Time
	completed int
}

func newRegistry() *registry {
	return &registry{entries: make(map[string]*execEntry)}
}

func (r *registry) add(id string, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[id] = &execEntry{cancel: cancel, startedAt: time.Now().UTC()}
}

// bump increments the completed-result counter, fed by the executor's
// onResult callback.
func (r *registry) bump(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		e.completed++
	}
}

func (r *registry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, id)
}

// snapshot reports whether id is still running and, if so, its progress.
func (r *registry) snapshot(id string) (execSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return execSnapshot{}, false
	}
	return execSnapshot{startedAt: e.startedAt, completed: e.completed}, true
}

// cancel stops a running execution; unknown or finished ids are an error.
func (r *registry) cancel(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return fmt.Errorf("no running execution with id %q (unknown or already finished)", id)
	}
	e.cancel()
	return nil
}
