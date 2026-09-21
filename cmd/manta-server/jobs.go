package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/dotabuff/manta/cmd/internal/matchviewer"
	"github.com/dotabuff/manta/cmd/internal/opendota"
	"github.com/dotabuff/manta/cmd/internal/replayfile"
)

type JobState string

const (
	JobQueued      JobState = "queued"
	JobResolving   JobState = "resolving"
	JobDownloading JobState = "downloading"
	JobParsing     JobState = "parsing"
	JobReady       JobState = "ready"
	JobFailed      JobState = "failed"
)

// Job is the progress of turning a match id into a viewable match.
type Job struct {
	MatchID int64     `json:"matchId"`
	State   JobState  `json:"state"`
	Message string    `json:"message,omitempty"`
	Done    int64     `json:"done,omitempty"`
	Total   int64     `json:"total,omitempty"`
	Error   string    `json:"error,omitempty"`
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`
	Viewer  string    `json:"viewer,omitempty"`
}

// ManagerOptions configures the job manager.
type ManagerOptions struct {
	Store       *Store
	Resolver    ReplayResolver
	Downloader  *Downloader
	OpenDota    *opendota.Client // nil disables reference data
	Reference   matchviewer.ReferenceOptions
	AssetDirs   []string
	KeepReplays bool
	MaxJobs     int
	Interval    uint32
	Logf        func(format string, args ...interface{})
}

// Manager runs match jobs and serves the resulting viewers.
type Manager struct {
	opts ManagerOptions
	sem  chan struct{}

	mu       sync.Mutex
	jobs     map[int64]*Job
	handlers map[int64]http.Handler
	refs     map[int64]*matchviewer.ReferenceStore
	minimaps map[string]*matchviewer.MinimapAsset
}

func NewManager(opts ManagerOptions) *Manager {
	if opts.MaxJobs <= 0 {
		opts.MaxJobs = 2
	}
	if opts.Interval == 0 {
		opts.Interval = 30
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	return &Manager{
		opts:     opts,
		sem:      make(chan struct{}, opts.MaxJobs),
		jobs:     make(map[int64]*Job),
		handlers: make(map[int64]http.Handler),
		refs:     make(map[int64]*matchviewer.ReferenceStore),
		minimaps: make(map[string]*matchviewer.MinimapAsset),
	}
}

func viewerPath(id int64) string { return fmt.Sprintf("/match/%d/", id) }

// Start returns the job for a match, creating it when needed. Matches already
// in the store are reported ready at once.
func (m *Manager) Start(id int64) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok && j.State != JobFailed {
		return j.clone()
	}
	now := time.Now()
	j := &Job{MatchID: id, State: JobQueued, Started: now, Updated: now}
	m.jobs[id] = j
	if m.opts.Store.Has(id) {
		j.State, j.Viewer, j.Message = JobReady, viewerPath(id), "already processed"
		return j.clone()
	}
	go m.run(j)
	return j.clone()
}

// Get returns a job by match id.
func (m *Manager) Get(id int64) (*Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		if m.opts.Store.Has(id) {
			now := time.Now()
			return &Job{MatchID: id, State: JobReady, Viewer: viewerPath(id), Started: now, Updated: now}, true
		}
		return nil, false
	}
	return j.clone(), true
}

// Active lists jobs that are not finished.
func (m *Manager) Active() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Job
	for _, j := range m.jobs {
		if j.State != JobReady {
			out = append(out, j.clone())
		}
	}
	return out
}

func (j *Job) clone() *Job { c := *j; return &c }

func (m *Manager) update(j *Job, fn func(*Job)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(j)
	j.Updated = time.Now()
}

func (m *Manager) run(j *Job) {
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	fail := func(err error) {
		m.opts.Logf("match %d: %v", j.MatchID, err)
		m.update(j, func(j *Job) { j.State, j.Error = JobFailed, err.Error() })
	}

	m.update(j, func(j *Job) { j.State, j.Message = JobResolving, "locating the replay" })
	url, err := m.opts.Resolver.Resolve(ctx, j.MatchID, func(msg string) {
		m.update(j, func(j *Job) { j.Message = msg })
	})
	if err != nil {
		fail(err)
		return
	}

	replayPath := m.opts.Store.ReplayPath(j.MatchID)
	if _, err := os.Stat(replayPath); err != nil {
		m.update(j, func(j *Job) { j.State, j.Message = JobDownloading, "downloading the replay from Valve" })
		start := time.Now()
		err = m.opts.Downloader.Download(ctx, url, replayPath, func(done, total int64) {
			m.update(j, func(j *Job) { j.Done, j.Total = done, total })
		})
		if err != nil {
			fail(err)
			return
		}
		m.opts.Logf("match %d: downloaded %s in %s", j.MatchID, url, time.Since(start).Round(time.Millisecond))
	}

	m.update(j, func(j *Job) { j.State, j.Message = JobParsing, "parsing the replay" })
	f, err := os.Open(replayPath)
	if err != nil {
		fail(err)
		return
	}
	buf, err := replayfile.Decode(f)
	f.Close()
	if err != nil {
		os.Remove(replayPath) // a bad download should not be reused
		fail(fmt.Errorf("decoding replay: %w", err))
		return
	}
	data, parseErr := matchviewer.Extract(buf, matchviewer.ExtractOptions{Interval: m.opts.Interval, FileName: fmt.Sprintf("match %d", j.MatchID)})
	if data == nil {
		fail(fmt.Errorf("parsing replay: %v", parseErr))
		return
	}
	if parseErr != nil {
		m.opts.Logf("match %d: parse stopped early: %v", j.MatchID, parseErr)
	}
	if len(data.Players) == 0 {
		fail(errors.New("the replay contains no players"))
		return
	}
	if err := m.opts.Store.Save(j.MatchID, data); err != nil {
		fail(fmt.Errorf("saving match: %w", err))
		return
	}
	if !m.opts.KeepReplays {
		os.Remove(replayPath)
	}
	m.update(j, func(j *Job) { j.State, j.Message, j.Viewer = JobReady, "", viewerPath(j.MatchID) })
	m.opts.Logf("match %d: ready (%d samples, %d players)", j.MatchID, len(data.Ticks), len(data.Players))

	// Reference data arrives in the background; the viewer polls for it.
	if m.opts.OpenDota != nil {
		refs := m.referenceStore(j.MatchID)
		ref := matchviewer.FetchReference(m.opts.OpenDota, data, m.opts.Reference, func(format string, args ...interface{}) {
			m.opts.Logf("match %d: "+format, append([]interface{}{j.MatchID}, args...)...)
		})
		data.Reference = ref
		if err := m.opts.Store.Save(j.MatchID, data); err != nil {
			m.opts.Logf("match %d: saving reference data: %v", j.MatchID, err)
		}
		refs.Set(ref)
	}
}

func (m *Manager) referenceStore(id int64) *matchviewer.ReferenceStore {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.refs[id]; ok {
		return s
	}
	s := &matchviewer.ReferenceStore{}
	if m.opts.OpenDota == nil {
		s.Set(nil)
	}
	m.refs[id] = s
	return s
}

// Handler returns the viewer for a processed match, loading it on first use.
func (m *Manager) Handler(id int64) (http.Handler, error) {
	m.mu.Lock()
	if h, ok := m.handlers[id]; ok {
		m.mu.Unlock()
		return h, nil
	}
	m.mu.Unlock()

	data, err := m.opts.Store.Load(id)
	if err != nil {
		return nil, err
	}
	refs := m.referenceStore(id)
	if data.Reference != nil {
		refs.Set(data.Reference)
	}
	h, err := matchviewer.NewHandler(data, m.minimap(data.Match.Map.ID), refs)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[id] = h
	return h, nil
}

func (m *Manager) minimap(mapID string) *matchviewer.MinimapAsset {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.minimaps[mapID]; ok {
		return a
	}
	a, path := matchviewer.FindMinimap(m.opts.AssetDirs, mapID)
	if a == nil {
		m.opts.Logf("no minimap image for map %s in %v; the viewer draws a schematic map", mapID, m.opts.AssetDirs)
	} else {
		m.opts.Logf("using minimap image %s", path)
	}
	m.minimaps[mapID] = a
	return a
}
