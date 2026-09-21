package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dotabuff/manta"
)

func TestTickBoundsAndPaging(t *testing.T) {
	ticks := []uint32{0, 0, 5, 5, 5, 9, 12}
	tickAt := func(i int) uint32 { return ticks[i] }

	check := func(name string, p pageParams, wantLo, wantHi int) {
		lo, hi := tickBounds(len(ticks), tickAt, p)
		if lo != wantLo || hi != wantHi {
			t.Errorf("%s: tickBounds = [%d, %d), want [%d, %d)", name, lo, hi, wantLo, wantHi)
		}
	}
	check("unbounded", pageParams{}, 0, 7)
	check("from only", pageParams{from: 5, hasFrom: true}, 2, 7)
	check("to only", pageParams{to: 5, hasTo: true}, 0, 5)
	check("from and to inclusive", pageParams{from: 5, hasFrom: true, to: 9, hasTo: true}, 2, 6)
	check("past the end", pageParams{from: 100, hasFrom: true}, 7, 7)
	check("inverted range is empty", pageParams{from: 9, hasFrom: true, to: 5, hasTo: true}, 5, 5)

	odd := func(i int) bool { return i%2 == 1 }
	ids, total := pageOver(0, len(ticks), odd, pageParams{offset: 1, limit: 2})
	if total != 3 || len(ids) != 2 || ids[0] != 3 || ids[1] != 5 {
		t.Errorf("pageOver odd offset=1 limit=2: ids=%v total=%d, want [3 5] 3", ids, total)
	}
	ids, total = pageOver(0, len(ticks), odd, pageParams{offset: 10, limit: 2})
	if total != 3 || len(ids) != 0 {
		t.Errorf("pageOver past end: ids=%v total=%d, want [] 3", ids, total)
	}
}

// fixtureReplay is downloaded by the root package's tests; this integration
// test is skipped when it is not present.
const fixtureReplay = "../../replays/2159568145.dem"

func TestIndexAndServer(t *testing.T) {
	buf, err := os.ReadFile(fixtureReplay)
	if err != nil {
		t.Skipf("fixture replay not available (%v); run the root package tests once to download it", err)
	}

	idx, err := buildIndex(buf, "fixture.dem", indexOptions{})
	if err != nil {
		t.Fatalf("buildIndex: %v", err)
	}
	if len(idx.Ops) == 0 || len(idx.CombatLog) == 0 || len(idx.StringTableUpdates) == 0 || len(idx.StringTables) == 0 {
		t.Fatalf("index is missing a stream: ops=%d combat=%d updates=%d tables=%d",
			len(idx.Ops), len(idx.CombatLog), len(idx.StringTableUpdates), len(idx.StringTables))
	}

	// Locate the first creation of a hero entity; it anchors the checks below.
	var created *opRecord
	createdID := -1
	for i := range idx.Ops {
		rec := &idx.Ops[i]
		if rec.Op == uint8(manta.EntityOpCreatedEntered) && strings.HasPrefix(idx.ClassNames[rec.ClassID], "CDOTA_Unit_Hero_") {
			created, createdID = rec, i
			break
		}
	}
	if created == nil {
		t.Fatal("no hero creation found in index")
	}
	if created.FieldsEnd != created.FieldsStart || idx.FieldsRecorded {
		t.Error("field names are not available through the public API and must not be recorded")
	}

	ts := httptest.NewServer(newServer(idx, buf))
	defer ts.Close()

	var summary struct {
		Counts  map[string]int `json:"counts"`
		Classes []idNameCount  `json:"classes"`
		Ops     []idNameCount  `json:"ops"`
		Tables  []tableSummary `json:"stringTables"`
	}
	getJSON(t, ts, "/api/summary", &summary)
	if summary.Counts["entities"] != len(idx.Ops) || summary.Counts["combatLog"] != len(idx.CombatLog) {
		t.Errorf("summary counts %v disagree with index", summary.Counts)
	}
	if len(summary.Classes) != len(idx.ClassCounts) || len(summary.Ops) != len(idx.OpCounts) || len(summary.Tables) != len(idx.StringTables) {
		t.Errorf("summary lists are incomplete")
	}

	var page struct {
		Total int         `json:"total"`
		Rows  []entityRow `json:"rows"`
	}
	getJSON(t, ts, fmt.Sprintf("/api/entities?class=%d&op=%d&limit=1", created.ClassID, created.Op), &page)
	if page.Total == 0 || len(page.Rows) != 1 || page.Rows[0].ID != createdID {
		t.Fatalf("filtered entities: total=%d rows=%+v, want first row id %d", page.Total, page.Rows, createdID)
	}
	row := page.Rows[0]
	if row.FieldCount != int(created.FieldsEnd-created.FieldsStart) || len(row.Fields) > rowFieldPreview {
		t.Errorf("row fields: count=%d preview=%d", row.FieldCount, len(row.Fields))
	}

	var det entityRow
	getJSON(t, ts, fmt.Sprintf("/api/entities/%d", createdID), &det)
	if len(det.Fields) != det.FieldCount {
		t.Errorf("detail returned %d of %d fields", len(det.Fields), det.FieldCount)
	}

	// State at the creation tick must show the new entity, with the changed
	// fields resolvable; one tick earlier it must not exist yet.
	var st entityState
	getJSON(t, ts, fmt.Sprintf("/api/entity-state?index=%d&tick=%d", created.Index, created.Tick), &st)
	if st.Missing || st.Serial != created.Serial || st.Class != idx.ClassNames[created.ClassID] || len(st.Fields) == 0 {
		t.Fatalf("state at creation tick: %+v", st)
	}
	names := make(map[string]bool, len(st.Fields))
	for _, f := range st.Fields {
		names[f.Name] = true
	}
	for _, f := range det.Fields {
		if !names[f] {
			t.Errorf("changed field %q not present in entity state", f)
		}
	}
	if created.Tick > 0 {
		var before entityState
		getJSON(t, ts, fmt.Sprintf("/api/entity-state?index=%d&tick=%d", created.Index, created.Tick-1), &before)
		if !before.Missing && before.Serial == created.Serial {
			t.Errorf("entity already present one tick before creation: %+v", before)
		}
		if before.StateTick >= created.Tick {
			t.Errorf("state before creation applied tick %d >= %d", before.StateTick, created.Tick)
		}
	}

	// Operations without fields (leave/delete) must serialize an empty array,
	// not null, so the front end can treat the field list uniformly.
	for i := range idx.Ops {
		if idx.Ops[i].Op != uint8(manta.EntityOpDeletedLeft) {
			continue
		}
		res, err := http.Get(ts.URL + fmt.Sprintf("/api/entities/%d", i))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(body), `"fields":[]`) {
			t.Errorf("delete op detail should carry an empty field array: %s", body)
		}
		break
	}

	var combat struct {
		Total int            `json:"total"`
		Rows  []combatLogRow `json:"rows"`
	}
	getJSON(t, ts, "/api/combat-log?limit=1", &combat)
	if combat.Total != len(idx.CombatLog) || len(combat.Rows) != 1 {
		t.Fatalf("combat log page: %+v", combat)
	}
	var combatDetail struct {
		Message json.RawMessage `json:"message"`
	}
	getJSON(t, ts, fmt.Sprintf("/api/combat-log/%d", combat.Rows[0].ID), &combatDetail)
	if len(combatDetail.Message) == 0 || combatDetail.Message[0] != '{' {
		t.Errorf("combat log detail has no protojson message: %s", combatDetail.Message)
	}

	var entries struct {
		Total int          `json:"total"`
		Rows  []tableEntry `json:"rows"`
	}
	getJSON(t, ts, "/api/string-tables/EntityNames?q=npc_dota_hero&limit=5", &entries)
	if entries.Total == 0 || len(entries.Rows) == 0 || !strings.Contains(entries.Rows[0].Key, "npc_dota_hero") {
		t.Errorf("EntityNames search: %+v", entries)
	}

	for _, c := range []struct {
		path string
		code int
	}{
		{"/api/entity-state?index=1&tick=abc", http.StatusBadRequest},
		{"/api/entities?class=x", http.StatusBadRequest},
		{"/api/entities/999999999", http.StatusNotFound},
		{"/api/string-tables/no_such_table", http.StatusNotFound},
		{"/app.js", http.StatusOK},
	} {
		res, err := http.Get(ts.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != c.code {
			t.Errorf("GET %s: status %d, want %d", c.path, res.StatusCode, c.code)
		}
	}
}

func getJSON(t *testing.T, ts *httptest.Server, path string, out interface{}) {
	t.Helper()
	res, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
}
