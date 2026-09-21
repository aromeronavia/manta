// Command manta-server is a small web service around the match viewer: enter
// a Dota 2 match id, it locates the replay through OpenDota, downloads it from
// Valve, parses it with manta and serves the interactive viewer.
//
//	manta-server -addr 0.0.0.0:8080 -data /data -assets /assets
//
// Processed matches are kept under the data directory, so they survive
// restarts; downloaded replays are deleted after parsing unless -keep-replays
// is set.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dotabuff/manta/cmd/internal/matchviewer"
	"github.com/dotabuff/manta/cmd/internal/opendota"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address to listen on")
	dataDir := flag.String("data", "data", "directory for processed matches, replays and the OpenDota cache")
	assets := flag.String("assets", "", "comma-separated directories holding minimap/<version>.webp images (a sibling redota checkout is also tried)")
	useOpenDota := flag.Bool("opendota", true, "fetch item popularity, benchmarks and item timings from OpenDota for each match")
	timings := flag.Bool("timings", true, "with -opendota, also fetch timing scenarios for the core items each hero bought")
	keepReplays := flag.Bool("keep-replays", false, "keep downloaded replay files after parsing")
	maxJobs := flag.Int("max-jobs", 2, "matches processed concurrently")
	maxReplayMB := flag.Int64("max-replay-mb", 600, "largest replay download accepted")
	requestWait := flag.Duration("request-wait", 5*time.Minute, "how long to wait for OpenDota to locate a replay it has not seen")
	interval := flag.Uint("interval", 30, "ticks between samples (30 = one per second)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags]\n\nflags:\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()

	store, err := NewStore(*dataDir)
	if err != nil {
		log.Fatalf("unable to prepare data directory: %s", err)
	}

	var client *opendota.Client
	if *useOpenDota {
		client = opendota.New(store.OpenDotaCacheDir())
	}
	// Replay lookup always goes through OpenDota, even when reference data is
	// disabled; it needs no API key.
	lookup := opendota.New(store.OpenDotaCacheDir())

	mgr := NewManager(ManagerOptions{
		Store:       store,
		Resolver:    &openDotaResolver{client: lookup, requestWait: *requestWait, poll: 10 * time.Second},
		Downloader:  &Downloader{HTTP: &http.Client{Timeout: 20 * time.Minute}, MaxBytes: *maxReplayMB << 20},
		OpenDota:    client,
		Reference:   matchviewer.ReferenceOptions{Timings: *timings},
		AssetDirs:   assetDirs(*assets),
		KeepReplays: *keepReplays,
		MaxJobs:     *maxJobs,
		Interval:    uint32(*interval),
	})

	log.Printf("manta-server listening on http://%s (data in %s)", *addr, *dataDir)
	if err := http.ListenAndServe(*addr, NewServer(mgr)); err != nil {
		log.Fatal(err)
	}
}

func assetDirs(flagValue string) []string {
	var dirs []string
	for _, d := range strings.Split(flagValue, ",") {
		if d = strings.TrimSpace(d); d != "" {
			dirs = append(dirs, d)
		}
	}
	dirs = append(dirs, filepath.Join("..", "redota", "public", "images"))
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "development", "redota", "public", "images"))
	}
	return dirs
}
