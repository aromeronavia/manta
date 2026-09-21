// Command manta-map replays a Dota 2 match on a minimap: hero movement with
// trails, kills, buildings, couriers, Roshan and wards on a scrubbable
// timeline, next to a scoreboard, net worth / XP graphs and the chat feed.
//
// Usage:
//
//	manta-map [-addr 127.0.0.1:8081] [-assets DIR] [-interval 30] [-map 7.40] <replay.dem>
//	manta-map -out match.html <replay.dem>
//
// Minimap images are looked up as <assets>/minimap/<version>.webp; a ReDota
// checkout (../redota/public/images) is used when present. Without an image
// the viewer draws a schematic map.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dotabuff/manta/cmd/internal/matchviewer"
	"github.com/dotabuff/manta/cmd/internal/opendota"
	"github.com/dotabuff/manta/cmd/internal/replayfile"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8081", "address to listen on")
	assets := flag.String("assets", "", "directory holding minimap/<version>.webp images (default: a sibling redota checkout)")
	interval := flag.Uint("interval", 30, "ticks between samples (30 = one per second)")
	mapVersion := flag.String("map", "", "force a map edition (7.23, 7.29, 7.33, 7.38, 7.40) instead of picking by match date")
	out := flag.String("out", "", "write a self-contained HTML file to this path instead of serving")
	useOpenDota := flag.Bool("opendota", true, "fetch item popularity, benchmarks and item timings from the OpenDota API")
	timings := flag.Bool("timings", true, "with -opendota, also fetch timing scenarios for the core items each hero bought (one request per hero and item)")
	cacheDir := flag.String("opendota-cache", "", "directory for cached OpenDota responses (default: the user cache directory)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <replay.dem>\n\nflags:\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	path := flag.Arg(0)

	start := time.Now()
	buf, err := replayfile.Load(path)
	if err != nil {
		log.Fatalf("unable to load replay: %s", err)
	}
	log.Printf("loaded %s (%.1f MB) in %s", path, float64(len(buf))/(1<<20), time.Since(start).Round(time.Millisecond))

	start = time.Now()
	data, parseErr := matchviewer.Extract(buf, matchviewer.ExtractOptions{
		Interval:   uint32(*interval),
		FileName:   filepath.Base(path),
		MapVersion: *mapVersion,
	})
	if data == nil {
		log.Fatalf("unable to extract replay: %s", parseErr)
	}
	if parseErr != nil {
		log.Printf("parse stopped early: %s (using what was extracted)", parseErr)
	}
	log.Printf("extracted %d samples, %d players, %d kills, %d buildings, %d wards, %d chat lines in %s",
		len(data.Ticks), len(data.Players), len(data.Kills), len(data.Buildings), len(data.Wards), len(data.Chat),
		time.Since(start).Round(time.Millisecond))
	log.Printf("match %s, map edition %s", data.Match.ID, data.Match.Map.ID)

	minimap, minimapPath := matchviewer.FindMinimap(assetDirs(*assets), data.Match.Map.ID)
	if minimap != nil {
		log.Printf("using minimap image %s", minimapPath)
	} else {
		log.Printf("no minimap image for %s found (pass -assets); drawing a schematic map", data.Match.Map.ID)
	}

	var client *opendota.Client
	if *useOpenDota {
		client = opendota.New(*cacheDir)
	}
	refOpts := matchviewer.ReferenceOptions{Timings: *timings}

	if *out != "" {
		if client != nil {
			start = time.Now()
			data.Reference = matchviewer.FetchReference(client, data, refOpts, log.Printf)
			log.Printf("fetched OpenDota reference data for %d heroes in %s", len(data.Reference.Heroes), time.Since(start).Round(time.Millisecond))
		}
		html, err := matchviewer.ExportHTML(data, minimap)
		if err != nil {
			log.Fatalf("unable to build export: %s", err)
		}
		if err := os.WriteFile(*out, html, 0o644); err != nil {
			log.Fatalf("unable to write %s: %s", *out, err)
		}
		log.Printf("wrote %s (%.1f MB)", *out, float64(len(html))/(1<<20))
		return
	}

	refs := &matchviewer.ReferenceStore{}
	if client == nil {
		refs.Set(nil)
	} else {
		go func() {
			start := time.Now()
			ref := matchviewer.FetchReference(client, data, refOpts, log.Printf)
			refs.Set(ref)
			log.Printf("OpenDota reference data ready for %d heroes in %s", len(ref.Heroes), time.Since(start).Round(time.Millisecond))
		}()
	}

	srv, err := matchviewer.NewHandler(data, minimap, refs)
	if err != nil {
		log.Fatalf("unable to start server: %s", err)
	}
	log.Printf("manta-map listening on http://%s", *addr)
	if err := http.ListenAndServe(*addr, srv); err != nil {
		log.Fatal(err)
	}
}

// assetDirs lists candidate asset directories: the flag, then a redota
// checkout beside the working directory or the repository.
func assetDirs(flagValue string) []string {
	dirs := []string{flagValue, filepath.Join("..", "redota", "public", "images")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "development", "redota", "public", "images"))
	}
	return dirs
}
