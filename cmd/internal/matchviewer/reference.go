package matchviewer

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dotabuff/manta/cmd/internal/opendota"
)

// coreItemCost is OpenDota's threshold for items with timing scenarios.
const coreItemCost = 1400

// HeroReference is the OpenDota context for one hero.
type HeroReference struct {
	Popularity *opendota.Popularity               `json:"popularity,omitempty"`
	Benchmarks map[string][]opendota.Percentile   `json:"benchmarks,omitempty"`
	Timings    map[string][]opendota.TimingBucket `json:"timings,omitempty"`
}

// ReferenceData is everything the viewer needs to compare the match with
// OpenDota's aggregate data: item constants for the items involved, and per
// hero item popularity, benchmarks and core-item timings.
type ReferenceData struct {
	Source    string                   `json:"source"`
	FetchedAt string                   `json:"fetchedAt"`
	Items     map[string]opendota.Item `json:"items"`
	Heroes    map[int32]*HeroReference `json:"heroes"`
	Errors    []string                 `json:"errors,omitempty"`
}

type ReferenceOptions struct {
	Timings bool
}

// FetchReference gathers OpenDota context for every hero in the match. It
// never fails outright: whatever could not be fetched is reported in Errors
// and the viewer degrades to what is present.
func FetchReference(c *opendota.Client, data *MatchData, opts ReferenceOptions, progress func(format string, args ...interface{})) *ReferenceData {
	ref := &ReferenceData{
		Source:    "OpenDota",
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Items:     map[string]opendota.Item{},
		Heroes:    map[int32]*HeroReference{},
	}
	fail := func(what string, err error) {
		ref.Errors = append(ref.Errors, fmt.Sprintf("%s: %v", what, err))
		progress("opendota: %s: %v", what, err)
	}

	ids, err := c.ItemIDs()
	if err != nil {
		fail("item ids", err)
	}
	items, err := c.Items()
	if err != nil {
		fail("item constants", err)
	}

	// Items bought per hero, in OpenDota's short names.
	bought := map[int32]map[string]bool{}
	for _, p := range data.Purchases {
		pl := findPlayerByID(data, p.Player)
		if pl == nil || pl.HeroID <= 0 {
			continue
		}
		if bought[pl.HeroID] == nil {
			bought[pl.HeroID] = map[string]bool{}
		}
		bought[pl.HeroID][strings.TrimPrefix(p.Item, "item_")] = true
	}

	referenced := map[string]bool{}
	heroIDs := make([]int32, 0, len(data.Players))
	seen := map[int32]bool{}
	for _, p := range data.Players {
		if p.HeroID > 0 && !seen[p.HeroID] {
			seen[p.HeroID] = true
			heroIDs = append(heroIDs, p.HeroID)
		}
	}
	sort.Slice(heroIDs, func(i, j int) bool { return heroIDs[i] < heroIDs[j] })

	for _, id := range heroIDs {
		hr := &HeroReference{}
		if ids != nil {
			if pop, err := c.ItemPopularity(int(id), ids); err != nil {
				fail(fmt.Sprintf("hero %d item popularity", id), err)
			} else {
				hr.Popularity = pop
				for _, m := range []map[string]int{pop.Start, pop.Early, pop.Mid, pop.Late} {
					for name := range m {
						referenced[name] = true
					}
				}
			}
		}
		if bench, err := c.Benchmarks(int(id)); err != nil {
			fail(fmt.Sprintf("hero %d benchmarks", id), err)
		} else {
			hr.Benchmarks = bench
		}
		if opts.Timings && items != nil {
			core := make([]string, 0)
			for name := range bought[id] {
				if it, ok := items[name]; ok && it.Cost >= coreItemCost {
					core = append(core, name)
				}
			}
			sort.Strings(core)
			for _, name := range core {
				buckets, err := c.ItemTimings(int(id), name)
				if err != nil {
					fail(fmt.Sprintf("hero %d %s timings", id, name), err)
					continue
				}
				if len(buckets) == 0 {
					continue
				}
				if hr.Timings == nil {
					hr.Timings = map[string][]opendota.TimingBucket{}
				}
				sort.Slice(buckets, func(i, j int) bool { return buckets[i].Time < buckets[j].Time })
				hr.Timings[name] = buckets
			}
		}
		ref.Heroes[id] = hr
		progress("opendota: hero %d ready (%d core items with timings)", id, len(hr.Timings))
	}

	for name := range bought {
		_ = name
	}
	for _, m := range bought {
		for name := range m {
			referenced[name] = true
		}
	}
	for name := range referenced {
		if it, ok := items[name]; ok {
			ref.Items[name] = it
		}
	}
	return ref
}

func findPlayerByID(d *MatchData, id int32) *playerTrack {
	for _, p := range d.Players {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// ReferenceStore hands the reference data to the handler once a background
// fetch completes.
type ReferenceStore struct {
	mu    sync.RWMutex
	ref   *ReferenceData
	ready bool
}

func (s *ReferenceStore) Set(ref *ReferenceData) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ref, s.ready = ref, true
}

func (s *ReferenceStore) Get() (*ReferenceData, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ref, s.ready
}
