package opendota

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/constants/item_ids":
			w.Write([]byte(`{"1":"blink","63":"power_treads","29":"boots"}`))
		case "/constants/items":
			w.Write([]byte(`{"blink":{"id":1,"dname":"Blink Dagger","cost":2250,"qual":"component"},"power_treads":{"id":63,"dname":"Power Treads","cost":1400,"qual":"common"},"boots":{"id":29,"dname":"Boots of Speed","cost":500}}`))
		case "/heroes/145/itemPopularity":
			w.Write([]byte(`{"start_game_items":{"29":10},"early_game_items":{"63":8,"29":3},"mid_game_items":{"1":5,"9999":2},"late_game_items":{}}`))
		case "/benchmarks":
			if r.URL.Query().Get("hero_id") != "145" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(`{"result":{"gold_per_min":[{"percentile":0.5,"value":575},{"percentile":0.9,"value":745}]}}`))
		case "/matches/9002441262":
			w.Write([]byte(`{"match_id":9002441262,"cluster":118,"replay_salt":123456789,"replay_url":"http://replay118.valve.net/570/9002441262_123456789.dem.bz2","duration":2239}`))
		case "/matches/1":
			http.NotFound(w, r)
		case "/matches/2":
			w.Write([]byte(`{"match_id":2,"duration":100}`))
		case "/request/1":
			if r.Method != http.MethodPost {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(`{"job":{"jobId":42}}`))
		case "/scenarios/itemTimings":
			w.Write([]byte(`[{"hero_id":145,"item":"manta","time":1200,"games":"28","wins":"21"},{"hero_id":145,"item":"manta","time":1500,"games":71,"wins":35}]`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func newTestClient(t *testing.T, ts *httptest.Server) *Client {
	c := New(t.TempDir())
	c.BaseURL = ts.URL
	c.MinInterval = 0
	return c
}

func TestEndpoints(t *testing.T) {
	var hits int32
	ts := newTestServer(t, &hits)
	defer ts.Close()
	c := newTestClient(t, ts)

	ids, err := c.ItemIDs()
	if err != nil || ids[63] != "power_treads" {
		t.Fatalf("ItemIDs: %v %v", ids, err)
	}
	items, err := c.Items()
	if err != nil || items["blink"].Name != "Blink Dagger" || items["blink"].Cost != 2250 {
		t.Fatalf("Items: %+v %v", items["blink"], err)
	}
	pop, err := c.ItemPopularity(145, ids)
	if err != nil {
		t.Fatal(err)
	}
	if pop.Early["power_treads"] != 8 || pop.Start["boots"] != 10 || pop.Mid["blink"] != 5 || pop.Mid["#9999"] != 2 || len(pop.Late) != 0 {
		t.Errorf("ItemPopularity: %+v", pop)
	}
	bench, err := c.Benchmarks(145)
	if err != nil || len(bench["gold_per_min"]) != 2 || bench["gold_per_min"][0].Value != 575 {
		t.Errorf("Benchmarks: %+v %v", bench, err)
	}
	timings, err := c.ItemTimings(145, "manta")
	if err != nil || len(timings) != 2 || timings[0] != (TimingBucket{Time: 1200, Games: 28, Wins: 21}) || timings[1].Games != 71 {
		t.Errorf("ItemTimings: %+v %v", timings, err)
	}
	if _, err := c.Benchmarks(1); !IsNotFound(err) {
		t.Errorf("expected a not-found error, got %v", err)
	}
}

func TestCache(t *testing.T) {
	var hits int32
	ts := newTestServer(t, &hits)
	defer ts.Close()
	c := newTestClient(t, ts)

	if _, err := c.ItemIDs(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ItemIDs(); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("second call should be served from cache; server saw %d hits", hits)
	}

	// A second client on the same directory shares the cache.
	c2 := New(c.CacheDir)
	c2.BaseURL = ts.URL
	c2.MinInterval = 0
	if _, err := c2.ItemIDs(); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("cache not shared across clients; server saw %d hits", hits)
	}

	// Expired entries are refetched.
	c2.TTL = time.Nanosecond
	time.Sleep(2 * time.Millisecond)
	if _, err := c2.ItemIDs(); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Errorf("expired cache entry should be refetched; server saw %d hits", hits)
	}
}

func TestReplays(t *testing.T) {
	var hits int32
	ts := newTestServer(t, &hits)
	defer ts.Close()
	c := newTestClient(t, ts)

	rep, ok, err := c.FindReplay(9002441262)
	if err != nil || !ok || rep.Cluster != 118 || rep.Salt != 123456789 {
		t.Fatalf("FindReplay: %+v %v %v", rep, ok, err)
	}
	if got, want := rep.URL(), "http://replay118.valve.net/570/9002441262_123456789.dem.bz2"; got != want {
		t.Errorf("URL = %s, want %s", got, want)
	}

	// Unknown matches (404) and matches without a salt yet are "not found"
	// and are not cached, so the next call asks again.
	if _, ok, err := c.FindReplay(1); err != nil || ok {
		t.Fatalf("FindReplay(1): ok=%v err=%v", ok, err)
	}
	before := hits
	if _, ok, err := c.FindReplay(2); err != nil || ok {
		t.Fatalf("FindReplay(2): ok=%v err=%v", ok, err)
	}
	if _, _, err := c.FindReplay(2); err != nil {
		t.Fatal(err)
	}
	if hits-before != 2 {
		t.Errorf("saltless match answers should not be cached; server saw %d extra hits", hits-before)
	}

	job, err := c.RequestParse(1)
	if err != nil || job != "42" {
		t.Errorf("RequestParse: %q %v", job, err)
	}
}
