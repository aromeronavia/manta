package main

import (
	"os"
	"testing"

	"github.com/dotabuff/manta/cmd/internal/matchviewer"
)

// A match processed by an older extractor lacks fields the viewer now needs,
// so the store must report it missing and let the job re-extract it.
func TestStoreHasRejectsStaleFormat(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	current := &matchviewer.MatchData{Format: matchviewer.FormatVersion}
	if err := store.Save(7, current); err != nil {
		t.Fatal(err)
	}
	if !store.Has(7) {
		t.Error("current-format match reported missing")
	}

	stale := &matchviewer.MatchData{Format: matchviewer.FormatVersion - 1}
	if err := store.Save(8, stale); err != nil {
		t.Fatal(err)
	}
	if store.Has(8) {
		t.Error("stale-format match reported present")
	}

	// Summaries written before formats existed carry no format at all.
	if err := store.Save(9, current); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.summaryPath(9), []byte(`{"id":9}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if store.Has(9) {
		t.Error("pre-format match reported present")
	}
}
