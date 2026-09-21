package main

import (
	"strconv"
	"strings"

	"github.com/dotabuff/manta"
	"github.com/dotabuff/manta/dota"
)

type indexOptions struct{}

// opRecord is one entity operation. Field names live in replayIndex.FieldIDs
// at [FieldsStart, FieldsEnd) as interned ids, keeping the record fixed-size.
type opRecord struct {
	Tick        uint32
	Index       int32
	Serial      int32
	ClassID     int32
	Op          uint8
	FieldsStart uint32
	FieldsEnd   uint32
}

type keyValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gameEventRecord struct {
	Tick uint32
	Name string
	Keys []keyValue
}

type combatLogRecord struct {
	Tick      uint32
	Type      string
	Attacker  string
	Target    string
	Inflictor string
	Value     uint32
	Health    int32
	Timestamp float32
	msg       *dota.CMsgDOTACombatLogEntry
}

type tableEntry struct {
	Index   int32  `json:"index"`
	Key     string `json:"key"`
	Size    int    `json:"size"`
	Preview string `json:"preview"`
}

type stringTableUpdateRecord struct {
	Tick    uint32
	Table   string
	Changed int
	Entries []tableEntry
}

type stringTableSnapshot struct {
	Name    string
	Entries []tableEntry
}

// replayIndex is everything the explorer serves, built in a single pass over
// the replay. Every stream is stored in file order, so ticks are
// non-decreasing and tick ranges can be located by binary search.
type replayIndex struct {
	FileName   string
	Header     *dota.CDemoFileHeader
	FileInfo   *dota.CDemoFileInfo
	GameBuild  uint32
	LastTick   uint32
	ParseError string
	// FieldsRecorded is always false: manta's public API does not expose the
	// field paths an entity update changed, so operations carry no field names.
	FieldsRecorded bool

	ClassNames  map[int32]string
	Ops         []opRecord
	FieldIDs    []uint32
	FieldNames  []string
	fieldIntern map[string]uint32
	ClassCounts map[int32]int
	OpCounts    map[uint8]int

	GameEvents      []gameEventRecord
	GameEventCounts map[string]int

	CombatLog       []combatLogRecord
	CombatLogCounts map[string]int

	StringTableUpdates      []stringTableUpdateRecord
	StringTableUpdateCounts map[string]int
	StringTables            []stringTableSnapshot
}

// keyDesc describes one key of a game event type, from its descriptor.
type keyDesc struct {
	name string
	typ  int32
}

// Game event key types, as carried by CMsgSource1LegacyGameEventList.
const (
	gameEventTypeString = 1
	gameEventTypeFloat  = 2
	gameEventTypeLong   = 3
	gameEventTypeShort  = 4
	gameEventTypeByte   = 5
	gameEventTypeBool   = 6
	gameEventTypeUint64 = 7
)

// buildIndex parses the replay once and records every stream. If the parse
// fails part way, the index built so far is returned along with the error.
func buildIndex(buf []byte, fileName string, opts indexOptions) (*replayIndex, error) {
	p, err := manta.NewParser(buf)
	if err != nil {
		return nil, err
	}

	idx := &replayIndex{
		FileName:                fileName,
		FieldsRecorded:          false,
		ClassNames:              make(map[int32]string),
		fieldIntern:             make(map[string]uint32),
		ClassCounts:             make(map[int32]int),
		OpCounts:                make(map[uint8]int),
		GameEventCounts:         make(map[string]int),
		CombatLogCounts:         make(map[string]int),
		StringTableUpdateCounts: make(map[string]int),
	}

	p.Callbacks.OnCDemoFileHeader(func(m *dota.CDemoFileHeader) error {
		idx.Header = m
		return nil
	})
	p.Callbacks.OnCDemoFileInfo(func(m *dota.CDemoFileInfo) error {
		idx.FileInfo = m
		return nil
	})

	p.OnEntity(func(e *manta.Entity, op manta.EntityOp) error {
		idx.addOp(p.Tick, e, op, nil)
		return nil
	})

	// Game event handlers are registered by name, so subscribe to every name
	// as soon as the descriptor list announces it.
	registered := make(map[string]bool)
	p.Callbacks.OnCMsgSource1LegacyGameEventList(func(m *dota.CMsgSource1LegacyGameEventList) error {
		for _, d := range m.GetDescriptors() {
			name := d.GetName()
			if registered[name] {
				continue
			}
			registered[name] = true

			keys := make([]keyDesc, 0, len(d.GetKeys()))
			for _, k := range d.GetKeys() {
				keys = append(keys, keyDesc{name: k.GetName(), typ: k.GetType()})
			}
			p.OnGameEvent(name, func(ge *manta.GameEvent) error {
				idx.addGameEvent(p.Tick, name, keys, ge)
				return nil
			})
		}
		return nil
	})

	p.Callbacks.OnCMsgDOTACombatLogEntry(func(m *dota.CMsgDOTACombatLogEntry) error {
		idx.addCombatLog(p, m)
		return nil
	})

	// String tables are observed through the raw create/update messages. Manta
	// assigns table ids in creation order, so a list of names indexed by id
	// resolves the id an update carries. The messages give the table and how
	// many entries changed, not the entries themselves.
	var tableNames []string
	tableBounds := map[string]int32{}
	p.Callbacks.OnCSVCMsg_CreateStringTable(func(m *dota.CSVCMsg_CreateStringTable) error {
		tableNames = append(tableNames, m.GetName())
		tableBounds[m.GetName()] += m.GetNumEntries()
		idx.addStringTableUpdate(p.Tick, m.GetName(), int(m.GetNumEntries()))
		return nil
	})
	p.Callbacks.OnCSVCMsg_UpdateStringTable(func(m *dota.CSVCMsg_UpdateStringTable) error {
		id := int(m.GetTableId())
		if id < 0 || id >= len(tableNames) {
			return nil
		}
		tableBounds[tableNames[id]] += m.GetNumChangedEntries()
		idx.addStringTableUpdate(p.Tick, tableNames[id], int(m.GetNumChangedEntries()))
		return nil
	})

	parseErr := p.Start()
	idx.GameBuild = p.GameBuild
	idx.LastTick = p.Tick
	if parseErr != nil {
		idx.ParseError = parseErr.Error()
	}

	// Final table contents: keys are recoverable by index through the public
	// lookup; indices are dense from zero, bounded by the entries ever added.
	for _, name := range tableNames {
		snap := stringTableSnapshot{Name: name}
		for i := int32(0); i < tableBounds[name]; i++ {
			key, ok := p.LookupStringByIndex(name, i)
			if !ok {
				continue
			}
			snap.Entries = append(snap.Entries, tableEntry{Index: i, Key: key})
		}
		idx.StringTables = append(idx.StringTables, snap)
	}

	return idx, parseErr
}

func (idx *replayIndex) addOp(tick uint32, e *manta.Entity, op manta.EntityOp, fields []string) {
	rec := opRecord{
		Tick:    tick,
		Index:   e.GetIndex(),
		Serial:  e.GetSerial(),
		ClassID: e.GetClassId(),
		Op:      uint8(op),
	}
	if _, ok := idx.ClassNames[rec.ClassID]; !ok {
		idx.ClassNames[rec.ClassID] = e.GetClassName()
	}
	if len(fields) > 0 {
		rec.FieldsStart = uint32(len(idx.FieldIDs))
		for _, f := range fields {
			idx.FieldIDs = append(idx.FieldIDs, idx.internField(f))
		}
		rec.FieldsEnd = uint32(len(idx.FieldIDs))
	}
	idx.Ops = append(idx.Ops, rec)
	idx.ClassCounts[rec.ClassID]++
	idx.OpCounts[rec.Op]++
}

func (idx *replayIndex) internField(name string) uint32 {
	if id, ok := idx.fieldIntern[name]; ok {
		return id
	}
	id := uint32(len(idx.FieldNames))
	idx.FieldNames = append(idx.FieldNames, name)
	idx.fieldIntern[name] = id
	return id
}

// opFields returns the changed field names of an operation. It is never nil,
// so it serializes as an empty JSON array rather than null.
func (idx *replayIndex) opFields(rec *opRecord) []string {
	n := int(rec.FieldsEnd - rec.FieldsStart)
	out := make([]string, n)
	for i, id := range idx.FieldIDs[rec.FieldsStart:rec.FieldsEnd] {
		out[i] = idx.FieldNames[id]
	}
	return out
}

func (idx *replayIndex) addGameEvent(tick uint32, name string, keys []keyDesc, ge *manta.GameEvent) {
	rec := gameEventRecord{Tick: tick, Name: name, Keys: make([]keyValue, len(keys))}
	for i, k := range keys {
		rec.Keys[i] = keyValue{Name: k.name, Value: gameEventValue(ge, k)}
	}
	idx.GameEvents = append(idx.GameEvents, rec)
	idx.GameEventCounts[name]++
}

func gameEventValue(ge *manta.GameEvent, k keyDesc) string {
	var err error
	switch k.typ {
	case gameEventTypeString:
		var v string
		if v, err = ge.GetString(k.name); err == nil {
			return v
		}
	case gameEventTypeFloat:
		var v float32
		if v, err = ge.GetFloat32(k.name); err == nil {
			return strconv.FormatFloat(float64(v), 'g', -1, 32)
		}
	case gameEventTypeLong, gameEventTypeShort, gameEventTypeByte:
		var v int32
		if v, err = ge.GetInt32(k.name); err == nil {
			return strconv.FormatInt(int64(v), 10)
		}
	case gameEventTypeBool:
		var v bool
		if v, err = ge.GetBool(k.name); err == nil {
			return strconv.FormatBool(v)
		}
	case gameEventTypeUint64:
		var v uint64
		if v, err = ge.GetUint64(k.name); err == nil {
			return strconv.FormatUint(v, 10)
		}
	default:
		return "<unknown type " + strconv.Itoa(int(k.typ)) + ">"
	}
	return "<error: " + err.Error() + ">"
}

func (idx *replayIndex) addCombatLog(p *manta.Parser, m *dota.CMsgDOTACombatLogEntry) {
	name := func(i uint32) string {
		s, _ := p.LookupStringByIndex("CombatLogNames", int32(i))
		return s
	}
	typ := strings.TrimPrefix(m.GetType().String(), "DOTA_COMBATLOG_")
	idx.CombatLog = append(idx.CombatLog, combatLogRecord{
		Tick:      p.Tick,
		Type:      typ,
		Attacker:  name(m.GetAttackerName()),
		Target:    name(m.GetTargetName()),
		Inflictor: name(m.GetInflictorName()),
		Value:     m.GetValue(),
		Health:    m.GetHealth(),
		Timestamp: m.GetTimestamp(),
		msg:       m,
	})
	idx.CombatLogCounts[typ]++
}

func (idx *replayIndex) addStringTableUpdate(tick uint32, table string, changed int) {
	idx.StringTableUpdates = append(idx.StringTableUpdates, stringTableUpdateRecord{Tick: tick, Table: table, Changed: changed, Entries: []tableEntry{}})
	idx.StringTableUpdateCounts[table]++
}
