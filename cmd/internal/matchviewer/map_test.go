package matchviewer

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dotabuff/manta/cmd/internal/opendota"
)

func TestMapForTime(t *testing.T) {
	cases := []struct {
		when time.Time
		want string
	}{
		{time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC), "7.40"},
		{time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), "7.38"},
		{time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), "7.33"},
		{time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC), "7.29"},
		{time.Date(2016, 2, 18, 0, 0, 0, 0, time.UTC), "7.23"},
		{time.Time{}, "7.23"},
	}
	for _, c := range cases {
		if got := mapForTime(c.when); got.ID != c.want {
			t.Errorf("mapForTime(%s) = %s, want %s", c.when.Format("2006-01-02"), got.ID, c.want)
		}
	}
	if m, ok := mapByID("7.33"); !ok || m.Size != 19134 {
		t.Errorf("mapByID(7.33) = %+v, %v", m, ok)
	}
	if _, ok := mapByID("1.0"); ok {
		t.Error("mapByID accepted an unknown edition")
	}
}

func TestHeroDisplayName(t *testing.T) {
	cases := map[string]string{
		"npc_dota_hero_shadow_shaman": "Shadow Shaman",
		"npc_dota_hero_kez":           "Kez",
		"npc_dota_hero_doom_bringer":  "Doom Bringer",
	}
	for in, want := range cases {
		if got := heroDisplayName(in); got != want {
			t.Errorf("heroDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlayerColor(t *testing.T) {
	if playerColor(teamRadiant, 0) != "#3375FF" || playerColor(teamDire, 0) != "#FE86C2" {
		t.Error("slot colours do not follow Dota's Radiant/Dire order")
	}
	if playerColor(teamDire, 9) != "#DDDDDD" {
		t.Error("out-of-range slot should fall back to grey")
	}
}

// fixtureReplay is downloaded by the root package's tests; the integration
// tests are skipped when it is not present.
const fixtureReplay = "../../../replays/2159568145.dem"

func loadFixture(t *testing.T) *MatchData {
	t.Helper()
	buf, err := os.ReadFile(fixtureReplay)
	if err != nil {
		t.Skipf("fixture replay not available (%v); run the root package tests once to download it", err)
	}
	data, err := Extract(buf, ExtractOptions{Interval: 30, FileName: "fixture.dem"})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return data
}

func TestExtractFixture(t *testing.T) {
	d := loadFixture(t)
	n := len(d.Ticks)
	if n == 0 {
		t.Fatal("no samples")
	}
	if got := len(d.Players); got != 10 {
		t.Fatalf("players = %d, want 10", got)
	}
	if d.Match.ID != "2159568145" || d.Match.Map.ID != "7.23" {
		t.Errorf("match info: %+v", d.Match)
	}

	// Samples are in tick order and roughly one per interval.
	for i := 1; i < n; i++ {
		if d.Ticks[i] <= d.Ticks[i-1] {
			t.Fatalf("ticks not increasing at %d: %d then %d", i, d.Ticks[i-1], d.Ticks[i])
		}
	}
	if expect := int(d.Match.LastTick / d.Interval); n < expect*9/10 {
		t.Errorf("only %d samples for %d ticks at interval %d", n, d.Match.LastTick, d.Interval)
	}

	if len(d.TimeOfDay) != n {
		t.Errorf("timeOfDay has %d entries, want %d", len(d.TimeOfDay), n)
	}

	// The clock becomes known and then runs forward.
	known := 0
	for i := 1; i < n; i++ {
		if d.Clock[i] == clockUnknown || d.Clock[i-1] == clockUnknown {
			continue
		}
		known++
		if d.Clock[i] < d.Clock[i-1]-0.5 {
			t.Errorf("clock went backwards at sample %d: %f -> %f", i, d.Clock[i-1], d.Clock[i])
		}
	}
	if known == 0 {
		t.Error("game clock never became known")
	}
	if d.Timing.ServerTimeOffset == nil || d.Timing.GameStartTime <= 0 || d.Timing.TickInterval <= 0 {
		t.Errorf("timing not calibrated: %+v", d.Timing)
	}
	// Kills carry clocks consistent with the sample clocks around them.
	for _, k := range d.Kills[:3] {
		i := 0
		for i < n-1 && d.Ticks[i+1] <= k.Tick {
			i++
		}
		if d.Clock[i] == clockUnknown || math.Abs(float64(k.Clock-d.Clock[i])) > float64(d.Interval)*d.Timing.TickInterval+0.5 {
			t.Errorf("kill at tick %d clock %.1f disagrees with sample clock %.1f", k.Tick, k.Clock, d.Clock[i])
		}
	}

	teams := map[int32]int{}
	for _, p := range d.Players {
		teams[p.Team]++
		if !strings.HasPrefix(p.Hero, "npc_dota_hero_") {
			t.Errorf("player %d has no hero (%q)", p.ID, p.Hero)
		}
		if p.Name == "" || p.Color == "" || p.HeroID <= 0 {
			t.Errorf("player %d missing name, colour or hero id: name=%q color=%q heroId=%d", p.ID, p.Name, p.Color, p.HeroID)
		}
		for name, l := range map[string]int{
			"x": len(p.X), "y": len(p.Y), "alive": len(p.Alive), "hp": len(p.HP), "mana": len(p.Mana), "respawn": len(p.Respawn), "level": len(p.Level),
			"kills": len(p.Kills), "netWorth": len(p.NetWorth), "xp": len(p.XP), "items": len(p.Items),
		} {
			if l != n {
				t.Errorf("player %d series %s has %d entries, want %d", p.ID, name, l, n)
			}
		}
		// Most late-game samples should have the hero on the map.
		present, inBounds, alive := 0, 0, 0
		for i := n / 2; i < n; i++ {
			if p.X[i] == absent {
				continue
			}
			present++
			if p.X[i] > 0 && p.X[i] < 2*mapCenter && p.Y[i] > 0 && p.Y[i] < 2*mapCenter {
				inBounds++
			}
			if p.Alive[i] == 1 {
				alive++
			}
		}
		if present == 0 || inBounds != present || alive == 0 {
			t.Errorf("player %d positions: present=%d inBounds=%d alive=%d", p.ID, present, inBounds, alive)
		}
		last := n - 1
		manaKnown := 0
		for i := n / 2; i < n; i++ {
			if p.Mana[i] >= 0 && p.Mana[i] <= 100 {
				manaKnown++
			}
		}
		if manaKnown == 0 {
			t.Errorf("player %d has no mana samples in the second half", p.ID)
		}
		if p.Level[last] <= 0 || p.NetWorth[last] <= 0 || p.XP[last] <= 0 {
			t.Errorf("player %d final stats look empty: level=%d nw=%d xp=%d", p.ID, p.Level[last], p.NetWorth[last], p.XP[last])
		}
		if items := p.Items[last]; len(items) != len(itemSlots) {
			t.Errorf("player %d final items has %d slots", p.ID, len(items))
		}
	}
	if teams[teamRadiant] != 5 || teams[teamDire] != 5 {
		t.Errorf("team split %v", teams)
	}
	if len(d.ItemNames) == 0 || !strings.HasPrefix(d.ItemNames[0], "item_") {
		t.Errorf("item names: %v", d.ItemNames)
	}

	// Farming series are cumulative and present.
	for _, p := range d.Players {
		if len(p.LastHits) != n || len(p.Denies) != n || len(p.EarnedGold) != n || len(p.CreepGold) != n || len(p.IncomeGold) != n || len(p.Stacks) != n {
			t.Errorf("player %d farm series misaligned", p.ID)
			continue
		}
		if p.LastHits[n-1] <= 0 || p.EarnedGold[n-1] <= 0 || p.CreepGold[n-1] < 0 || p.IncomeGold[n-1] <= 0 {
			t.Errorf("player %d final farm stats: lh=%d earned=%d creepGold=%d", p.ID, p.LastHits[n-1], p.EarnedGold[n-1], p.CreepGold[n-1])
		}
		for i := 1; i < n; i++ {
			if p.LastHits[i] != absent && p.LastHits[i-1] != absent && p.LastHits[i] < p.LastHits[i-1] {
				t.Errorf("player %d last hits decreased at sample %d", p.ID, i)
				break
			}
		}
	}
	kinds := map[string]int{}
	unlocated := 0
	for _, e := range d.FarmEvents {
		kinds[e.Kind]++
		if findPlayer(d, e.Player) == nil {
			t.Errorf("farm event credited to unknown player %d", e.Player)
		}
		if e.X == absent {
			unlocated++
		}
		if int(e.Target) >= len(d.FarmTargets) || d.FarmTargets[e.Target] == "" {
			t.Errorf("farm event target %d out of range", e.Target)
		}
	}
	if kinds["lane"] == 0 || kinds["neutral"] == 0 || kinds["deny"] == 0 {
		t.Errorf("farm event kinds: %v", kinds)
	}
	if unlocated > len(d.FarmEvents)/10 {
		t.Errorf("%d of %d farm events have no position", unlocated, len(d.FarmEvents))
	}
	if len(d.Purchases) == 0 {
		t.Error("no purchases recorded")
	}
	for _, pu := range d.Purchases {
		if !strings.HasPrefix(pu.Item, "item_") || findPlayer(d, pu.Player) == nil {
			t.Errorf("bad purchase %+v", pu)
		}
	}

	if len(d.Kills) == 0 {
		t.Fatal("no hero kills recorded")
	}
	located, byHero := 0, 0
	for _, k := range d.Kills {
		if p := findPlayer(d, k.Victim); p == nil {
			t.Errorf("kill victim %d is not a player", k.Victim)
		}
		if k.X != absent {
			located++
		}
		if k.Killer != absent {
			byHero++
		}
		if k.Tick == 0 {
			t.Error("kill without a tick")
		}
	}
	if located < len(d.Kills)*9/10 || byHero == 0 {
		t.Errorf("kills: %d total, %d located, %d by heroes", len(d.Kills), located, byHero)
	}

	towers, destroyed := 0, 0
	for _, b := range d.Buildings {
		if b.Kind == "tower" {
			towers++
		}
		if b.DestroyedTick != nil {
			destroyed++
		}
		if b.X == 0 || b.Y == 0 || (b.Team != teamRadiant && b.Team != teamDire) {
			t.Errorf("building %+v lacks position or team", b)
		}
		if !strings.Contains(b.Name, "good") && !strings.Contains(b.Name, "bad") {
			t.Errorf("building %q has an unexpected name", b.Name)
		}
	}
	if towers < 18 || destroyed == 0 {
		t.Errorf("buildings: %d towers, %d destroyed", towers, destroyed)
	}

	if len(d.Couriers) == 0 {
		t.Error("no couriers tracked")
	}
	for _, c := range d.Couriers {
		if len(c.X) != n || len(c.Alive) != n {
			t.Errorf("courier series length %d/%d, want %d", len(c.X), len(c.Alive), n)
		}
	}
	if d.Roshan == nil || len(d.Roshan.X) != n {
		t.Fatal("roshan track missing or misaligned")
	}
	roshAlive := 0
	for _, a := range d.Roshan.Alive {
		if a == 1 {
			roshAlive++
		}
	}
	if roshAlive == 0 {
		t.Error("roshan never alive")
	}

	if len(d.Wards) == 0 {
		t.Error("no wards recorded")
	}
	for _, w := range d.Wards {
		if w.X == 0 && w.Y == 0 {
			t.Errorf("ward without position: %+v", w)
		}
		if w.RemovedTick != nil && *w.RemovedTick < w.PlacedTick {
			t.Errorf("ward removed before placed: %+v", w)
		}
	}
}

func findPlayer(d *MatchData, id int32) *playerTrack {
	for _, p := range d.Players {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func TestServerAndExport(t *testing.T) {
	d := loadFixture(t)
	minimap := &MinimapAsset{data: []byte("RIFFfakewebp"), contentType: "image/webp"}

	refs := &ReferenceStore{}
	srv, err := NewHandler(d, minimap, refs)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Reference data is pending until the background fetch stores it.
	var pending struct {
		Pending bool `json:"pending"`
	}
	getJSONInto(t, ts.URL+"/api/reference", &pending)
	if !pending.Pending {
		t.Error("reference should be pending before it is set")
	}
	refs.Set(&ReferenceData{Source: "test", Heroes: map[int32]*HeroReference{}, Items: map[string]opendota.Item{}})
	var ready ReferenceData
	getJSONInto(t, ts.URL+"/api/reference", &ready)
	if ready.Source != "test" {
		t.Errorf("reference not served after set: %+v", ready)
	}

	res, err := http.Get(ts.URL + "/api/match")
	if err != nil {
		t.Fatal(err)
	}
	var got MatchData
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(got.Players) != 10 || len(got.Ticks) != len(d.Ticks) || got.Match.Map.Image != "assets/minimap" {
		t.Errorf("served document differs: players=%d ticks=%d image=%q", len(got.Players), len(got.Ticks), got.Match.Map.Image)
	}
	for _, path := range []string{"/", "/app.js", "/style.css", "/assets/minimap"} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %d", path, res.StatusCode)
		}
	}

	html, err := ExportHTML(d, minimap)
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	for _, want := range []string{"window.MATCH = {", "<style>", "data:image/webp;base64,", `"id":"2159568145"`} {
		if !strings.Contains(page, want) {
			t.Errorf("export missing %q", want)
		}
	}
	if strings.Contains(page, `href="style.css"`) || strings.Contains(page, `src="app.js"`) {
		t.Error("export still references external assets")
	}
	// Exactly two script elements: the document and the viewer.
	if n := strings.Count(page, "</script>"); n != 2 {
		t.Errorf("export has %d script closers, want 2", n)
	}
}

func TestExportEscapesScriptCloser(t *testing.T) {
	d := &MatchData{Ticks: []uint32{1}, Clock: []float32{0}, Players: []*playerTrack{}, Chat: []chatLine{{Text: "</script><script>alert(1)"}}}
	html, err := ExportHTML(d, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(html), "</script>") != 2 {
		t.Errorf("chat text broke out of the data script element:\n%s", html)
	}
}

func getJSONInto(t *testing.T, url string, out interface{}) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
}

// TestFetchReference runs the reference fetch against a fake OpenDota.
func TestFetchReference(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/constants/item_ids":
			w.Write([]byte(`{"1":"blink","63":"power_treads","29":"boots","44":"tango"}`))
		case "/constants/items":
			w.Write([]byte(`{"blink":{"id":1,"dname":"Blink Dagger","cost":2250},"power_treads":{"id":63,"dname":"Power Treads","cost":1400},"boots":{"id":29,"dname":"Boots","cost":500},"tango":{"id":44,"dname":"Tango","cost":90}}`))
		case "/heroes/145/itemPopularity":
			w.Write([]byte(`{"start_game_items":{"44":120},"early_game_items":{"29":90,"63":80},"mid_game_items":{"1":70},"late_game_items":{}}`))
		case "/benchmarks":
			w.Write([]byte(`{"result":{"gold_per_min":[{"percentile":0.5,"value":575}]}}`))
		case "/scenarios/itemTimings":
			if r.URL.Query().Get("item") == "blink" {
				w.Write([]byte(`[{"hero_id":145,"item":"blink","time":900,"games":"10","wins":"6"}]`))
			} else {
				w.Write([]byte(`[]`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	c := opendota.New(t.TempDir())
	c.BaseURL = api.URL
	c.MinInterval = 0

	d := &MatchData{
		Players:   []*playerTrack{{ID: 5, HeroID: 145, Hero: "npc_dota_hero_kez"}},
		Purchases: []purchase{{Player: 5, Item: "item_blink"}, {Player: 5, Item: "item_boots"}, {Player: 5, Item: "item_power_treads"}},
	}
	ref := FetchReference(c, d, ReferenceOptions{Timings: true}, func(string, ...interface{}) {})
	if len(ref.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", ref.Errors)
	}
	hr := ref.Heroes[145]
	if hr == nil || hr.Popularity == nil || hr.Popularity.Early["power_treads"] != 80 || hr.Popularity.Start["tango"] != 120 {
		t.Fatalf("popularity not resolved: %+v", hr)
	}
	if len(hr.Benchmarks["gold_per_min"]) != 1 {
		t.Errorf("benchmarks missing: %+v", hr.Benchmarks)
	}
	if _, ok := hr.Timings["blink"]; !ok {
		t.Errorf("timings for the core item blink missing: %+v", hr.Timings)
	}
	if _, ok := hr.Timings["boots"]; ok {
		t.Error("boots is below the core item cost and should have no timings")
	}
	for _, name := range []string{"blink", "boots", "power_treads", "tango"} {
		if _, ok := ref.Items[name]; !ok {
			t.Errorf("item %s missing from the referenced item subset", name)
		}
	}
}
