package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dotabuff/manta/cmd/internal/matchviewer"
)

// Store keeps processed matches on disk: the full viewer document, gzipped,
// plus a small summary used for listings.
type Store struct {
	dir string
}

// MatchSummary is what the landing page needs to list a match.
type MatchSummary struct {
	ID        int64           `json:"id"`
	Format    int             `json:"format"`
	Winner    int32           `json:"winner"`
	Duration  float32         `json:"duration"`
	Players   []PlayerSummary `json:"players"`
	SavedAt   time.Time       `json:"savedAt"`
	Map       string          `json:"map"`
	Reference bool            `json:"reference"`
}

type PlayerSummary struct {
	Team int32  `json:"team"`
	Hero string `json:"hero"`
	Name string `json:"name"`
}

func NewStore(dir string) (*Store, error) {
	for _, sub := range []string{"matches", "replays", "opendota"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir}, nil
}

func (s *Store) OpenDotaCacheDir() string { return filepath.Join(s.dir, "opendota") }
func (s *Store) ReplayPath(id int64) string {
	return filepath.Join(s.dir, "replays", strconv.FormatInt(id, 10)+".dem.bz2")
}
func (s *Store) matchPath(id int64) string {
	return filepath.Join(s.dir, "matches", strconv.FormatInt(id, 10)+".json.gz")
}
func (s *Store) summaryPath(id int64) string {
	return filepath.Join(s.dir, "matches", strconv.FormatInt(id, 10)+".summary.json")
}

// Has reports whether a processed document exists for the match in the
// layout the viewer expects; older documents count as missing so that the
// match is extracted again.
func (s *Store) Has(id int64) bool {
	if _, err := os.Stat(s.matchPath(id)); err != nil {
		return false
	}
	b, err := os.ReadFile(s.summaryPath(id))
	if err != nil {
		return false
	}
	var sum struct {
		Format int `json:"format"`
	}
	return json.Unmarshal(b, &sum) == nil && sum.Format == matchviewer.FormatVersion
}

// Save writes the document and its summary atomically.
func (s *Store) Save(id int64, data *matchviewer.MatchData) error {
	if err := writeAtomic(s.matchPath(id), func(f *os.File) error {
		zw := gzip.NewWriter(f)
		if err := json.NewEncoder(zw).Encode(data); err != nil {
			return err
		}
		return zw.Close()
	}); err != nil {
		return err
	}
	return writeAtomic(s.summaryPath(id), func(f *os.File) error {
		return json.NewEncoder(f).Encode(summarize(id, data))
	})
}

// Load reads a processed document.
func (s *Store) Load(id int64) (*matchviewer.MatchData, error) {
	f, err := os.Open(s.matchPath(id))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var data matchviewer.MatchData
	if err := json.NewDecoder(zr).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode match %d: %w", id, err)
	}
	return &data, nil
}

// List returns summaries of every processed match, newest first.
func (s *Store) List() ([]MatchSummary, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "matches"))
	if err != nil {
		return nil, err
	}
	var out []MatchSummary
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".summary.json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, "matches", e.Name()))
		if err != nil {
			continue
		}
		var sum MatchSummary
		if err := json.Unmarshal(b, &sum); err != nil || !s.Has(sum.ID) {
			continue
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt.After(out[j].SavedAt) })
	return out, nil
}

func summarize(id int64, data *matchviewer.MatchData) MatchSummary {
	sum := MatchSummary{ID: id, Format: data.Format, Winner: data.Match.Winner, SavedAt: time.Now().UTC(), Map: data.Match.Map.ID, Reference: data.Reference != nil}
	for i := len(data.Clock) - 1; i >= 0; i-- {
		if data.Clock[i] != -32768 {
			sum.Duration = data.Clock[i]
			break
		}
	}
	for _, p := range data.Players {
		sum.Players = append(sum.Players, PlayerSummary{Team: p.Team, Hero: p.Hero, Name: p.Name})
	}
	return sum
}

func writeAtomic(path string, write func(*os.File) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
