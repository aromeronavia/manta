// Package opendota is a small client for the public OpenDota API
// (https://docs.opendota.com), covering the endpoints manta-map uses to put
// a replay in context: item constants, per-hero item popularity in
// professional games, per-hero stat benchmarks and item timing scenarios.
//
// Responses are cached on disk so a replay can be re-exported offline and so
// repeated runs stay well inside OpenDota's free rate limit; uncached requests
// are paced about one per second.
package opendota

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBaseURL = "https://api.opendota.com/api"
	DefaultTTL     = 7 * 24 * time.Hour
	userAgent      = "manta-map (github.com/dotabuff/manta)"
)

// Client fetches and caches OpenDota responses.
type Client struct {
	BaseURL  string
	CacheDir string
	TTL      time.Duration
	HTTP     *http.Client
	// MinInterval spaces uncached requests; zero disables pacing.
	MinInterval time.Duration

	mu       sync.Mutex
	lastCall time.Time
}

// New returns a client caching under dir (or the user cache directory when
// dir is empty).
func New(dir string) *Client {
	if dir == "" {
		if base, err := os.UserCacheDir(); err == nil {
			dir = filepath.Join(base, "manta-map", "opendota")
		} else {
			dir = filepath.Join(os.TempDir(), "manta-map-opendota")
		}
	}
	return &Client{
		BaseURL:     DefaultBaseURL,
		CacheDir:    dir,
		TTL:         DefaultTTL,
		HTTP:        &http.Client{Timeout: 30 * time.Second},
		MinInterval: 1100 * time.Millisecond,
	}
}

// Item is the subset of OpenDota's item constants the viewer needs.
type Item struct {
	ID   int    `json:"id"`
	Name string `json:"name"` // display name
	Cost int    `json:"cost"`
	Qual string `json:"qual,omitempty"`
}

// Popularity holds per-phase purchase counts from professional matches, keyed
// by item name (OpenDota's short name, e.g. "power_treads").
type Popularity struct {
	Start map[string]int `json:"start"`
	Early map[string]int `json:"early"`
	Mid   map[string]int `json:"mid"`
	Late  map[string]int `json:"late"`
}

// Percentile is one point of a benchmark distribution.
type Percentile struct {
	Percentile float64 `json:"percentile"`
	Value      float64 `json:"value"`
}

// TimingBucket reports games and wins for an item bought within a timing
// bucket (Time is the bucket's upper bound in seconds).
type TimingBucket struct {
	Time  int `json:"time"`
	Games int `json:"games"`
	Wins  int `json:"wins"`
}

// ItemIDs returns the item id -> short name table.
func (c *Client) ItemIDs() (map[int]string, error) {
	var raw map[string]string
	if err := c.getJSON("constants/item_ids", nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[int]string, len(raw))
	for k, v := range raw {
		id, err := strconv.Atoi(k)
		if err != nil {
			continue
		}
		out[id] = v
	}
	return out, nil
}

// Items returns the item constants keyed by short name.
func (c *Client) Items() (map[string]Item, error) {
	var raw map[string]struct {
		ID    int    `json:"id"`
		Dname string `json:"dname"`
		Cost  int    `json:"cost"`
		Qual  string `json:"qual"`
	}
	if err := c.getJSON("constants/items", nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]Item, len(raw))
	for name, v := range raw {
		out[name] = Item{ID: v.ID, Name: v.Dname, Cost: v.Cost, Qual: v.Qual}
	}
	return out, nil
}

// ItemPopularity returns a hero's item popularity by game phase, with item
// ids resolved to short names through ids.
func (c *Client) ItemPopularity(heroID int, ids map[int]string) (*Popularity, error) {
	var raw struct {
		Start map[string]int `json:"start_game_items"`
		Early map[string]int `json:"early_game_items"`
		Mid   map[string]int `json:"mid_game_items"`
		Late  map[string]int `json:"late_game_items"`
	}
	if err := c.getJSON(fmt.Sprintf("heroes/%d/itemPopularity", heroID), nil, &raw); err != nil {
		return nil, err
	}
	resolve := func(m map[string]int) map[string]int {
		out := make(map[string]int, len(m))
		for k, n := range m {
			id, err := strconv.Atoi(k)
			if err != nil {
				continue
			}
			if name, ok := ids[id]; ok {
				out[name] = n
			} else {
				out["#"+k] = n
			}
		}
		return out
	}
	return &Popularity{Start: resolve(raw.Start), Early: resolve(raw.Early), Mid: resolve(raw.Mid), Late: resolve(raw.Late)}, nil
}

// Benchmarks returns a hero's per-minute stat percentiles keyed by stat name
// (gold_per_min, xp_per_min, last_hits_per_min, ...).
func (c *Client) Benchmarks(heroID int) (map[string][]Percentile, error) {
	var raw struct {
		Result map[string][]Percentile `json:"result"`
	}
	if err := c.getJSON("benchmarks", url.Values{"hero_id": {strconv.Itoa(heroID)}}, &raw); err != nil {
		return nil, err
	}
	return raw.Result, nil
}

// ItemTimings returns win rates by purchase timing bucket for an item on a
// hero. OpenDota only tracks items costing at least 1400 gold.
func (c *Client) ItemTimings(heroID int, item string) ([]TimingBucket, error) {
	var raw []struct {
		Time  json.Number `json:"time"`
		Games json.Number `json:"games"`
		Wins  json.Number `json:"wins"`
	}
	if err := c.getJSON("scenarios/itemTimings", url.Values{"hero_id": {strconv.Itoa(heroID)}, "item": {item}}, &raw); err != nil {
		return nil, err
	}
	out := make([]TimingBucket, 0, len(raw))
	for _, r := range raw {
		t, _ := r.Time.Int64()
		g, _ := r.Games.Int64()
		w, _ := r.Wins.Int64()
		out = append(out, TimingBucket{Time: int(t), Games: int(g), Wins: int(w)})
	}
	return out, nil
}

// Replay locates a match's replay on Valve's servers: the cluster and salt
// that form the download URL. OpenDota only knows them for matches it has
// seen.
type Replay struct {
	MatchID int64  `json:"match_id"`
	Cluster int    `json:"cluster"`
	Salt    int64  `json:"replay_salt"`
	URLHint string `json:"replay_url,omitempty"`
}

// URL returns the Valve replay download URL for the replay.
func (r Replay) URL() string {
	if r.URLHint != "" {
		return r.URLHint
	}
	return fmt.Sprintf("http://replay%d.valve.net/570/%d_%d.dem.bz2", r.Cluster, r.MatchID, r.Salt)
}

// FindReplay returns the replay location for a match if OpenDota knows it,
// via the match endpoint (the dedicated replays endpoint no longer answers).
// A match without a salt is not cached, so a later call can pick it up once
// OpenDota has processed the match.
func (c *Client) FindReplay(matchID int64) (Replay, bool, error) {
	var raw Replay
	path := "matches/" + strconv.FormatInt(matchID, 10)
	if err := c.getJSON(path, nil, &raw); err != nil {
		if IsNotFound(err) {
			return Replay{}, false, nil
		}
		return Replay{}, false, err
	}
	if raw.Salt == 0 || raw.MatchID != matchID {
		c.dropCache(cacheKey(path, nil))
		return Replay{}, false, nil
	}
	return raw, true, nil
}

// RequestParse asks OpenDota to fetch and parse a match it has not seen yet,
// which also makes its replay location available. It returns the job id.
func (c *Client) RequestParse(matchID int64) (string, error) {
	u := strings.TrimRight(c.BaseURL, "/") + "/request/" + strconv.FormatInt(matchID, 10)
	c.pace()
	req, err := http.NewRequest(http.MethodPost, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("request parse: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var out struct {
		Job struct {
			JobID json.Number `json:"jobId"`
		} `json:"job"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("request parse: %w", err)
	}
	return out.Job.JobID.String(), nil
}

// getJSON serves path from the cache when fresh, otherwise fetches it, stores
// it and decodes it into out.
func (c *Client) getJSON(path string, query url.Values, out interface{}) error {
	key := cacheKey(path, query)
	if data, ok := c.readCache(key); ok {
		if err := json.Unmarshal(data, out); err == nil {
			return nil
		}
	}

	u := strings.TrimRight(c.BaseURL, "/") + "/" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	data, err := c.fetch(u)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	c.writeCache(key, data)
	return nil
}

// pace spaces requests by MinInterval.
func (c *Client) pace() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.MinInterval > 0 {
		if wait := c.MinInterval - time.Since(c.lastCall); wait > 0 {
			time.Sleep(wait)
		}
	}
	c.lastCall = time.Now()
}

func (c *Client) fetch(u string) ([]byte, error) {
	c.pace()

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d: %s", u, res.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	return body, nil
}

func cacheKey(path string, query url.Values) string {
	key := strings.NewReplacer("/", "_", "?", "_", "&", "_", "=", "-").Replace(path)
	if len(query) > 0 {
		key += "_" + strings.NewReplacer("&", "_", "=", "-", "%", "").Replace(query.Encode())
	}
	return key + ".json"
}

func (c *Client) readCache(key string) ([]byte, bool) {
	if c.CacheDir == "" {
		return nil, false
	}
	path := filepath.Join(c.CacheDir, key)
	info, err := os.Stat(path)
	if err != nil || (c.TTL > 0 && time.Since(info.ModTime()) > c.TTL) {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

func (c *Client) dropCache(key string) {
	if c.CacheDir != "" {
		_ = os.Remove(filepath.Join(c.CacheDir, key))
	}
}

func (c *Client) writeCache(key string, data []byte) {
	if c.CacheDir == "" {
		return
	}
	if err := os.MkdirAll(c.CacheDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(c.CacheDir, key), data, 0o644)
}

// IsNotFound reports whether err is an HTTP 404 from the API.
func IsNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 404")
}

var _ = errors.New
