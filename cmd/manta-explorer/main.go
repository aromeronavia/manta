// Command manta-explorer parses a Dota 2 replay with Manta and serves a local
// web UI for browsing the raw event streams the parser produces: entity
// operations, game events, combat log entries and string table updates.
//
// Usage:
//
//	manta-explorer [-addr 127.0.0.1:8080] [-fields=false] <replay.dem>
//
// The replay may be a plain .dem, or bzip2 / Zstandard compressed (the latter
// requires the zstd binary on PATH). The whole replay is indexed once at
// startup; inspecting an entity's state at a given tick re-parses on demand.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/dotabuff/manta/cmd/internal/replayfile"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address to listen on")
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
	idx, parseErr := buildIndex(buf, filepath.Base(path), indexOptions{})
	if idx == nil {
		log.Fatalf("unable to index replay: %s", parseErr)
	}
	if parseErr != nil {
		log.Printf("parse stopped early: %s (serving what was indexed)", parseErr)
	}
	log.Printf("indexed %d entity ops, %d game events, %d combat log entries, %d string table updates in %s",
		len(idx.Ops), len(idx.GameEvents), len(idx.CombatLog), len(idx.StringTableUpdates), time.Since(start).Round(time.Millisecond))

	// Growing the index leaves a lot of freed slack behind; hand it back and
	// report what is actually retained.
	debug.FreeOSMemory()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	log.Printf("index holds %.0f MB in heap (%d field references across %d distinct field names)",
		float64(ms.HeapInuse)/(1<<20), len(idx.FieldIDs), len(idx.FieldNames))

	srv := newServer(idx, buf)
	log.Printf("manta-explorer listening on http://%s", *addr)
	if err := http.ListenAndServe(*addr, srv); err != nil {
		log.Fatal(err)
	}
}
