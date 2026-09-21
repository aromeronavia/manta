package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dotabuff/manta"
	"google.golang.org/protobuf/encoding/protojson"
)

//go:embed static
var staticFiles embed.FS

const (
	defaultLimit = 100
	maxLimit     = 1000
	// Rows list only a prefix of their fields/entries; the detail endpoint
	// returns everything.
	rowFieldPreview = 8
	rowEntryPreview = 6
	stateCacheSize  = 32
)

type server struct {
	idx *replayIndex
	buf []byte
	mux *http.ServeMux

	// Re-parses are CPU bound and hold a full parser; run one at a time and
	// memoize recent results so re-clicking a row is instant.
	stateMu    sync.Mutex
	stateCache map[stateKey]*entityState
}

type stateKey struct {
	tick  uint32
	index int32
}

func newServer(idx *replayIndex, buf []byte) *server {
	s := &server{idx: idx, buf: buf, mux: http.NewServeMux(), stateCache: make(map[stateKey]*entityState)}

	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("/", http.FileServer(http.FS(static)))

	s.mux.HandleFunc("GET /api/summary", s.handleSummary)
	s.mux.HandleFunc("GET /api/entities", s.handleEntities)
	s.mux.HandleFunc("GET /api/entities/{id}", s.handleEntityDetail)
	s.mux.HandleFunc("GET /api/entity-state", s.handleEntityState)
	s.mux.HandleFunc("GET /api/game-events", s.handleGameEvents)
	s.mux.HandleFunc("GET /api/combat-log", s.handleCombatLog)
	s.mux.HandleFunc("GET /api/combat-log/{id}", s.handleCombatLogDetail)
	s.mux.HandleFunc("GET /api/string-table-updates", s.handleStringTableUpdates)
	s.mux.HandleFunc("GET /api/string-table-updates/{id}", s.handleStringTableUpdateDetail)
	s.mux.HandleFunc("GET /api/string-tables/{name}", s.handleStringTable)
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// --- summary ---------------------------------------------------------------

type nameCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type idNameCount struct {
	ID    int32  `json:"id"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type tableSummary struct {
	Name    string `json:"name"`
	Updates int    `json:"updates"`
	Entries int    `json:"entries"`
}

type playerSummary struct {
	Team    int32  `json:"team"`
	SteamID string `json:"steamId"`
	Hero    string `json:"hero"`
	Name    string `json:"name"`
}

func (s *server) handleSummary(w http.ResponseWriter, r *http.Request) {
	idx := s.idx
	out := map[string]interface{}{}

	file := map[string]interface{}{
		"name":           idx.FileName,
		"gameBuild":      idx.GameBuild,
		"lastTick":       idx.LastTick,
		"parseError":     idx.ParseError,
		"fieldsRecorded": idx.FieldsRecorded,
	}
	if h := idx.Header; h != nil {
		file["demoStamp"] = h.GetDemoFileStamp()
		file["server"] = h.GetServerName()
		file["client"] = h.GetClientName()
		file["map"] = h.GetMapName()
		file["buildNum"] = h.GetBuildNum()
	}
	if fi := idx.FileInfo; fi != nil {
		file["playbackTime"] = fi.GetPlaybackTime()
		file["playbackTicks"] = fi.GetPlaybackTicks()
		file["playbackFrames"] = fi.GetPlaybackFrames()
	}
	out["file"] = file

	match := map[string]interface{}{}
	if fi := idx.FileInfo; fi != nil {
		if gi := fi.GetGameInfo().GetDota(); gi != nil {
			match["matchId"] = strconv.FormatUint(gi.GetMatchId(), 10)
			match["gameMode"] = gi.GetGameMode()
			match["winner"] = gi.GetGameWinner()
			match["leagueId"] = gi.GetLeagueid()
			match["endTime"] = time.Unix(int64(gi.GetEndTime()), 0).UTC().Format(time.RFC3339)
			players := make([]playerSummary, 0, len(gi.GetPlayerInfo()))
			for _, pl := range gi.GetPlayerInfo() {
				players = append(players, playerSummary{
					Team:    pl.GetGameTeam(),
					SteamID: strconv.FormatUint(pl.GetSteamid(), 10),
					Hero:    pl.GetHeroName(),
					Name:    pl.GetPlayerName(),
				})
			}
			match["players"] = players
		}
	}
	out["match"] = match

	out["counts"] = map[string]int{
		"entities":           len(idx.Ops),
		"gameEvents":         len(idx.GameEvents),
		"combatLog":          len(idx.CombatLog),
		"stringTableUpdates": len(idx.StringTableUpdates),
	}

	classes := make([]idNameCount, 0, len(idx.ClassCounts))
	for id, n := range idx.ClassCounts {
		classes = append(classes, idNameCount{ID: id, Name: idx.ClassNames[id], Count: n})
	}
	sortIDNameCounts(classes)
	out["classes"] = classes

	ops := make([]idNameCount, 0, len(idx.OpCounts))
	for op, n := range idx.OpCounts {
		ops = append(ops, idNameCount{ID: int32(op), Name: manta.EntityOp(op).String(), Count: n})
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].ID < ops[j].ID })
	out["ops"] = ops

	out["gameEventNames"] = sortedNameCounts(idx.GameEventCounts)
	out["combatLogTypes"] = sortedNameCounts(idx.CombatLogCounts)

	tables := make([]tableSummary, 0, len(idx.StringTables))
	for _, t := range idx.StringTables {
		tables = append(tables, tableSummary{Name: t.Name, Updates: idx.StringTableUpdateCounts[t.Name], Entries: len(t.Entries)})
	}
	out["stringTables"] = tables

	writeJSON(w, out)
}

func sortedNameCounts(m map[string]int) []nameCount {
	out := make([]nameCount, 0, len(m))
	for name, n := range m {
		out = append(out, nameCount{Name: name, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func sortIDNameCounts(xs []idNameCount) {
	sort.Slice(xs, func(i, j int) bool {
		if xs[i].Count != xs[j].Count {
			return xs[i].Count > xs[j].Count
		}
		return xs[i].Name < xs[j].Name
	})
}

// --- paging -----------------------------------------------------------------

type pageParams struct {
	from, to       uint32
	hasFrom, hasTo bool
	offset, limit  int
}

func parsePage(r *http.Request) (pageParams, error) {
	q := r.URL.Query()
	p := pageParams{limit: defaultLimit}
	var err error
	if v := q.Get("from"); v != "" {
		if p.from, err = parseUint32(v); err != nil {
			return p, fmt.Errorf("from: %v", err)
		}
		p.hasFrom = true
	}
	if v := q.Get("to"); v != "" {
		if p.to, err = parseUint32(v); err != nil {
			return p, fmt.Errorf("to: %v", err)
		}
		p.hasTo = true
	}
	if v := q.Get("offset"); v != "" {
		if p.offset, err = strconv.Atoi(v); err != nil || p.offset < 0 {
			return p, fmt.Errorf("offset: invalid")
		}
	}
	if v := q.Get("limit"); v != "" {
		if p.limit, err = strconv.Atoi(v); err != nil || p.limit < 1 {
			return p, fmt.Errorf("limit: invalid")
		}
		p.limit = min(p.limit, maxLimit)
	}
	return p, nil
}

func parseUint32(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	return uint32(v), err
}

// tickBounds narrows [0, n) to the records whose tick lies within the page's
// tick range, using binary search over the tick-ordered stream.
func tickBounds(n int, tickAt func(i int) uint32, p pageParams) (lo, hi int) {
	lo, hi = 0, n
	if p.hasFrom {
		lo = sort.Search(n, func(i int) bool { return tickAt(i) >= p.from })
	}
	if p.hasTo {
		hi = sort.Search(n, func(i int) bool { return tickAt(i) > p.to })
	}
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

// pageOver scans [lo, hi), collecting the indices of matching records that
// fall within the requested page, and counts all matches.
func pageOver(lo, hi int, match func(i int) bool, p pageParams) (ids []int, total int) {
	for i := lo; i < hi; i++ {
		if !match(i) {
			continue
		}
		if total >= p.offset && len(ids) < p.limit {
			ids = append(ids, i)
		}
		total++
	}
	return ids, total
}

// parseSet parses a comma-separated query parameter into a set. A missing or
// empty parameter yields nil, meaning "no filter".
func parseSet(r *http.Request, key string) map[string]bool {
	v := r.URL.Query().Get(key)
	if v == "" {
		return nil
	}
	set := make(map[string]bool)
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			set[s] = true
		}
	}
	return set
}

func parseIntSet(r *http.Request, key string) (map[int32]bool, error) {
	strs := parseSet(r, key)
	if strs == nil {
		return nil, nil
	}
	set := make(map[int32]bool, len(strs))
	for s := range strs {
		v, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not an integer", key, s)
		}
		set[int32(v)] = true
	}
	return set, nil
}

type pageResponse struct {
	Total  int         `json:"total"`
	Offset int         `json:"offset"`
	Rows   interface{} `json:"rows"`
}

// --- entities ----------------------------------------------------------------

type entityRow struct {
	ID         int      `json:"id"`
	Tick       uint32   `json:"tick"`
	Index      int32    `json:"index"`
	Serial     int32    `json:"serial"`
	ClassID    int32    `json:"classId"`
	Class      string   `json:"class"`
	Op         uint8    `json:"op"`
	OpName     string   `json:"opName"`
	FieldCount int      `json:"fieldCount"`
	Fields     []string `json:"fields"`
}

func (s *server) entityRow(id int, limit int) entityRow {
	rec := &s.idx.Ops[id]
	fields := s.idx.opFields(rec)
	row := entityRow{
		ID:         id,
		Tick:       rec.Tick,
		Index:      rec.Index,
		Serial:     rec.Serial,
		ClassID:    rec.ClassID,
		Class:      s.idx.ClassNames[rec.ClassID],
		Op:         rec.Op,
		OpName:     manta.EntityOp(rec.Op).String(),
		FieldCount: len(fields),
		Fields:     fields,
	}
	if limit > 0 && len(row.Fields) > limit {
		row.Fields = row.Fields[:limit]
	}
	return row
}

func (s *server) handleEntities(w http.ResponseWriter, r *http.Request) {
	p, err := parsePage(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	classes, err := parseIntSet(r, "class")
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	opsSet, err := parseIntSet(r, "op")
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	var index int32
	hasIndex := false
	if v := r.URL.Query().Get("index"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			httpError(w, http.StatusBadRequest, fmt.Errorf("index: invalid"))
			return
		}
		index, hasIndex = int32(n), true
	}

	ops := s.idx.Ops
	lo, hi := tickBounds(len(ops), func(i int) uint32 { return ops[i].Tick }, p)
	ids, total := pageOver(lo, hi, func(i int) bool {
		rec := &ops[i]
		if classes != nil && !classes[rec.ClassID] {
			return false
		}
		if opsSet != nil && !opsSet[int32(rec.Op)] {
			return false
		}
		if hasIndex && rec.Index != index {
			return false
		}
		return true
	}, p)

	rows := make([]entityRow, len(ids))
	for i, id := range ids {
		rows[i] = s.entityRow(id, rowFieldPreview)
	}
	writeJSON(w, pageResponse{Total: total, Offset: p.offset, Rows: rows})
}

func (s *server) handleEntityDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, len(s.idx.Ops))
	if !ok {
		return
	}
	writeJSON(w, s.entityRow(id, 0))
}

func (s *server) handleEntityState(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tick, err := parseUint32(q.Get("tick"))
	if err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("tick: invalid"))
		return
	}
	idx, err := strconv.ParseInt(q.Get("index"), 10, 32)
	if err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("index: invalid"))
		return
	}

	key := stateKey{tick: tick, index: int32(idx)}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	st, ok := s.stateCache[key]
	if !ok {
		start := time.Now()
		st, err = entityStateAt(s.buf, tick, int32(idx))
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		log.Printf("entity state index=%d tick=%d re-parsed in %s", idx, tick, time.Since(start).Round(time.Millisecond))
		if len(s.stateCache) >= stateCacheSize {
			clear(s.stateCache)
		}
		s.stateCache[key] = st
	}
	writeJSON(w, st)
}

// --- game events ---------------------------------------------------------------

type gameEventRow struct {
	ID   int        `json:"id"`
	Tick uint32     `json:"tick"`
	Name string     `json:"name"`
	Keys []keyValue `json:"keys"`
}

func (s *server) handleGameEvents(w http.ResponseWriter, r *http.Request) {
	p, err := parsePage(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	names := parseSet(r, "name")

	evs := s.idx.GameEvents
	lo, hi := tickBounds(len(evs), func(i int) uint32 { return evs[i].Tick }, p)
	ids, total := pageOver(lo, hi, func(i int) bool {
		return names == nil || names[evs[i].Name]
	}, p)

	rows := make([]gameEventRow, len(ids))
	for i, id := range ids {
		ev := &evs[id]
		rows[i] = gameEventRow{ID: id, Tick: ev.Tick, Name: ev.Name, Keys: ev.Keys}
	}
	writeJSON(w, pageResponse{Total: total, Offset: p.offset, Rows: rows})
}

// --- combat log ------------------------------------------------------------------

type combatLogRow struct {
	ID        int     `json:"id"`
	Tick      uint32  `json:"tick"`
	Type      string  `json:"type"`
	Attacker  string  `json:"attacker"`
	Target    string  `json:"target"`
	Inflictor string  `json:"inflictor"`
	Value     uint32  `json:"value"`
	Health    int32   `json:"health"`
	Timestamp float32 `json:"timestamp"`
}

func combatRow(id int, rec *combatLogRecord) combatLogRow {
	return combatLogRow{
		ID: id, Tick: rec.Tick, Type: rec.Type,
		Attacker: rec.Attacker, Target: rec.Target, Inflictor: rec.Inflictor,
		Value: rec.Value, Health: rec.Health, Timestamp: rec.Timestamp,
	}
}

func (s *server) handleCombatLog(w http.ResponseWriter, r *http.Request) {
	p, err := parsePage(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	types := parseSet(r, "type")

	log := s.idx.CombatLog
	lo, hi := tickBounds(len(log), func(i int) uint32 { return log[i].Tick }, p)
	ids, total := pageOver(lo, hi, func(i int) bool {
		return types == nil || types[log[i].Type]
	}, p)

	rows := make([]combatLogRow, len(ids))
	for i, id := range ids {
		rows[i] = combatRow(id, &log[id])
	}
	writeJSON(w, pageResponse{Total: total, Offset: p.offset, Rows: rows})
}

func (s *server) handleCombatLogDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, len(s.idx.CombatLog))
	if !ok {
		return
	}
	rec := &s.idx.CombatLog[id]
	raw, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(rec.msg)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, struct {
		combatLogRow
		Message json.RawMessage `json:"message"`
	}{combatRow(id, rec), raw})
}

// --- string tables ---------------------------------------------------------------

type stringTableUpdateRow struct {
	ID      int          `json:"id"`
	Tick    uint32       `json:"tick"`
	Table   string       `json:"table"`
	Count   int          `json:"count"`
	Entries []tableEntry `json:"entries"`
}

func stringTableUpdateRowOf(id int, rec *stringTableUpdateRecord, limit int) stringTableUpdateRow {
	row := stringTableUpdateRow{ID: id, Tick: rec.Tick, Table: rec.Table, Count: rec.Changed, Entries: rec.Entries}
	if limit > 0 && len(row.Entries) > limit {
		row.Entries = row.Entries[:limit]
	}
	return row
}

func (s *server) handleStringTableUpdates(w http.ResponseWriter, r *http.Request) {
	p, err := parsePage(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	tables := parseSet(r, "table")

	ups := s.idx.StringTableUpdates
	lo, hi := tickBounds(len(ups), func(i int) uint32 { return ups[i].Tick }, p)
	ids, total := pageOver(lo, hi, func(i int) bool {
		return tables == nil || tables[ups[i].Table]
	}, p)

	rows := make([]stringTableUpdateRow, len(ids))
	for i, id := range ids {
		rows[i] = stringTableUpdateRowOf(id, &ups[id], rowEntryPreview)
	}
	writeJSON(w, pageResponse{Total: total, Offset: p.offset, Rows: rows})
}

func (s *server) handleStringTableUpdateDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, len(s.idx.StringTableUpdates))
	if !ok {
		return
	}
	writeJSON(w, stringTableUpdateRowOf(id, &s.idx.StringTableUpdates[id], 0))
}

// handleStringTable pages through the final contents of one table, optionally
// filtered by a case-insensitive substring of the key or value preview.
func (s *server) handleStringTable(w http.ResponseWriter, r *http.Request) {
	p, err := parsePage(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("name")
	var table *stringTableSnapshot
	for i := range s.idx.StringTables {
		if s.idx.StringTables[i].Name == name {
			table = &s.idx.StringTables[i]
			break
		}
	}
	if table == nil {
		httpError(w, http.StatusNotFound, fmt.Errorf("no string table named %q", name))
		return
	}

	q := strings.ToLower(r.URL.Query().Get("q"))
	entries := table.Entries
	ids, total := pageOver(0, len(entries), func(i int) bool {
		if q == "" {
			return true
		}
		e := &entries[i]
		return strings.Contains(strings.ToLower(e.Key), q) || strings.Contains(strings.ToLower(e.Preview), q)
	}, p)

	rows := make([]tableEntry, len(ids))
	for i, id := range ids {
		rows[i] = entries[id]
	}
	writeJSON(w, pageResponse{Total: total, Offset: p.offset, Rows: rows})
}

// --- helpers -------------------------------------------------------------------------

func pathID(w http.ResponseWriter, r *http.Request, n int) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 0 || id >= n {
		httpError(w, http.StatusNotFound, fmt.Errorf("no such row"))
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func httpError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
