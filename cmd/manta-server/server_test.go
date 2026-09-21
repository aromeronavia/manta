package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const fixtureReplay = "../../replays/2159568145.dem"

// TestMatchPipeline runs a match through resolve, download, parse and serve
// against a fake replay host, then checks persistence across a restart.
func TestMatchPipeline(t *testing.T) {
	replay, err := os.ReadFile(fixtureReplay)
	if err != nil {
		t.Skipf("fixture replay not available (%v); run the root package tests once to download it", err)
	}

	valve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/570/2159568145_1.dem.bz2" {
			http.NotFound(w, r)
			return
		}
		w.Write(replay)
	}))
	defer valve.Close()

	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	newManager := func() *Manager {
		return NewManager(ManagerOptions{
			Store: store,
			Resolver: ResolverFunc(func(ctx context.Context, id int64, progress func(string)) (string, error) {
				resolved++
				return valve.URL + "/570/2159568145_1.dem.bz2", nil
			}),
			Downloader: &Downloader{HTTP: valve.Client(), MaxBytes: 512 << 20},
			Interval:   60,
			Logf:       t.Logf,
		})
	}
	mgr := newManager()
	ts := httptest.NewServer(NewServer(mgr))
	defer ts.Close()

	// Bad ids are rejected before any work happens.
	res, _ := http.Post(ts.URL+"/api/matches/abc", "", nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("bad id: status %d", res.StatusCode)
	}
	res.Body.Close()

	res, err = http.Post(ts.URL+"/api/matches/2159568145", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var job Job
	json.NewDecoder(res.Body).Decode(&job)
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted || job.State == JobReady {
		t.Fatalf("start: status %d job %+v", res.StatusCode, job)
	}

	deadline := time.Now().Add(60 * time.Second)
	for job.State != JobReady && job.State != JobFailed && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		res, err := http.Get(ts.URL + "/api/jobs/2159568145")
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(res.Body).Decode(&job)
		res.Body.Close()
	}
	if job.State != JobReady {
		t.Fatalf("job did not finish: %+v", job)
	}
	if job.Viewer != "/match/2159568145/" {
		t.Errorf("viewer path %q", job.Viewer)
	}

	// The viewer is served under its prefix with the document and static files.
	var doc struct {
		Players []struct{ Hero string } `json:"players"`
		Ticks   []uint32                `json:"ticks"`
	}
	res, err = http.Get(ts.URL + "/match/2159568145/api/match")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(res.Body).Decode(&doc)
	res.Body.Close()
	if len(doc.Players) != 10 || len(doc.Ticks) == 0 {
		t.Errorf("served document: %d players, %d ticks", len(doc.Players), len(doc.Ticks))
	}
	for _, path := range []string{"/match/2159568145/", "/match/2159568145/app.js", "/match/2159568145/api/reference", "/", "/healthz"} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %d", path, res.StatusCode)
		}
	}
	// Reference data is disabled here, so the viewer is told so.
	res, _ = http.Get(ts.URL + "/match/2159568145/api/reference")
	body := make([]byte, 64)
	n, _ := res.Body.Read(body)
	res.Body.Close()
	if !strings.Contains(string(body[:n]), `"disabled":true`) {
		t.Errorf("reference endpoint: %s", body[:n])
	}

	// The listing shows the match; the replay was not kept.
	var list struct {
		Matches []MatchSummary `json:"matches"`
	}
	res, _ = http.Get(ts.URL + "/api/matches")
	json.NewDecoder(res.Body).Decode(&list)
	res.Body.Close()
	if len(list.Matches) != 1 || list.Matches[0].ID != 2159568145 || len(list.Matches[0].Players) != 10 || list.Matches[0].Duration <= 0 {
		t.Errorf("listing: %+v", list.Matches)
	}
	if _, err := os.Stat(store.ReplayPath(2159568145)); !os.IsNotExist(err) {
		t.Error("replay file should be removed after parsing")
	}

	// An unknown match redirects to the landing page with the id prefilled.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, _ = client.Get(ts.URL + "/match/12345/")
	res.Body.Close()
	if res.StatusCode != http.StatusFound || !strings.Contains(res.Header.Get("Location"), "match=12345") {
		t.Errorf("unknown match: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	// After a restart the stored match is ready without resolving again.
	mgr2 := newManager()
	ts2 := httptest.NewServer(NewServer(mgr2))
	defer ts2.Close()
	before := resolved
	res, _ = http.Post(ts2.URL+"/api/matches/2159568145", "", nil)
	json.NewDecoder(res.Body).Decode(&job)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || job.State != JobReady || resolved != before {
		t.Errorf("restart: status %d job %+v resolved %d->%d", res.StatusCode, job, before, resolved)
	}
	res, _ = http.Get(ts2.URL + "/match/2159568145/api/match")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("viewer after restart: %d", res.StatusCode)
	}
}

func TestDownloadFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			http.NotFound(w, r)
		case "/big":
			w.Header().Set("Content-Length", "10485760")
			w.Write(make([]byte, 10485760))
		}
	}))
	defer srv.Close()
	d := &Downloader{HTTP: srv.Client(), MaxBytes: 1 << 20}
	dst := t.TempDir() + "/out.bin"
	if err := d.Download(context.Background(), srv.URL+"/missing", dst, func(int64, int64) {}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404 should be reported: %v", err)
	}
	if err := d.Download(context.Background(), srv.URL+"/big", dst, func(int64, int64) {}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("oversize download should be refused: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("failed downloads must not leave a file behind")
	}
}
