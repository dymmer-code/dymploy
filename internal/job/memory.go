package job

import (
	"context"
	"sort"
	"sync"
)

// MemStore is an in-memory Store, for tests and for running without persistence.
type MemStore struct {
	mu      sync.Mutex
	jobs    map[ID]Job
	numbers map[string]int
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore { return &MemStore{jobs: map[ID]Job{}, numbers: map[string]int{}} }

// Put implements Store.
func (m *MemStore) Put(_ context.Context, j Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[j.ID] = clone(j)
	return nil
}

// Get implements Store.
func (m *MemStore) Get(_ context.Context, id ID) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return clone(j), nil
}

// List implements Store.
func (m *MemStore) List(_ context.Context, f Filter) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Job
	for _, j := range m.jobs {
		if f.Matches(j) {
			out = append(out, clone(j))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID > out[b].ID })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// Delete implements Store.
func (m *MemStore) Delete(_ context.Context, id ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.jobs, id)
	return nil
}

// NextNumber implements Store.
func (m *MemStore) NextNumber(_ context.Context, name string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.numbers[name]++
	return m.numbers[name], nil
}

// clone copies the pointer and slice fields so callers cannot alias stored data.
func clone(j Job) Job {
	if j.ExitCode != nil {
		c := *j.ExitCode
		j.ExitCode = &c
	}
	j.Params = append(j.Params[:0:0], j.Params...)
	j.Input = append(j.Input[:0:0], j.Input...)
	j.Details = append(j.Details[:0:0], j.Details...)
	return j
}

// MemLogs is an in-memory Logs.
type MemLogs struct {
	mu   sync.Mutex
	logs map[ID][]Entry
}

// NewMemLogs returns empty in-memory logs.
func NewMemLogs() *MemLogs { return &MemLogs{logs: map[ID][]Entry{}} }

// Append implements Logs.
func (m *MemLogs) Append(id ID, e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs[id] = append(m.logs[id], e)
	return nil
}

// Read implements Logs.
func (m *MemLogs) Read(id ID, after int64, limit int) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Entry
	for _, e := range m.logs[id] {
		if e.Seq > after {
			out = append(out, e)
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// Delete implements Logs.
func (m *MemLogs) Delete(id ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.logs, id)
	return nil
}
