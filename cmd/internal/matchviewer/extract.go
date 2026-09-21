// Package matchviewer turns a Dota 2 replay into the sampled match document
// the map viewer renders, and serves or exports that viewer. It is shared by
// the manta-map command line tool and the manta-server web service.
package matchviewer

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dotabuff/manta"
	"github.com/dotabuff/manta/dota"
)

const (
	invalidHandle = 16777215 // 0xFFFFFF: an unset entity handle
	teamRadiant   = 2
	teamDire      = 3
	maxPlayerID   = 32
	absent        = -1
	// clockUnknown marks samples taken before the game clock exists.
	clockUnknown = -32768
	// tickRate is the demo's simulation rate in ticks per second.
	tickRate = 30
)

// playerColors are Dota 2's fixed per-slot colours: Radiant slots 0-4, then
// Dire slots 0-4.
var playerColors = [10]string{
	"#3375FF", "#66FFBF", "#BF00BF", "#F3F00B", "#FF6B00",
	"#FE86C2", "#A1B447", "#65D9F7", "#008321", "#A46900",
}

// itemSlots are the inventory slots recorded per sample: six inventory, three
// backpack, the teleport scroll and the neutral item.
var itemSlots = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 15, 16}

// MatchData is the document the viewer consumes. Every per-sample array has
// one entry per element of Ticks; absent values are -1.
type MatchData struct {
	Match     matchInfo      `json:"match"`
	Timing    timing         `json:"timing"`
	Interval  uint32         `json:"interval"`
	Ticks     []uint32       `json:"ticks"`
	Clock     []float32      `json:"clock"`
	TimeOfDay []int32        `json:"timeOfDay"`
	Players   []*playerTrack `json:"players"`
	ItemNames []string       `json:"itemNames"`
	Couriers  []*unitTrack   `json:"couriers"`
	Roshan    *unitTrack     `json:"roshan"`
	Buildings []*building    `json:"buildings"`
	Kills     []kill         `json:"kills"`
	Wards     []*ward        `json:"wards"`
	Chat      []chatLine     `json:"chat"`
	Events    []chatEvent    `json:"events"`

	// Farming: every creep kill credited to a hero, item purchases, and the
	// interned target names farmEvent.Target indexes into.
	FarmEvents  []farmEvent `json:"farmEvents"`
	FarmTargets []string    `json:"farmTargets"`
	Purchases   []purchase  `json:"purchases"`

	// Reference is OpenDota context, embedded in exports; the server offers it
	// separately at api/reference once fetched.
	Reference *ReferenceData `json:"reference,omitempty"`
}

// timing records how ticks were mapped to the game clock. Server time advances
// one tick interval per tick; the offset is calibrated from the game rules
// entity's running game time (older replays) or from combat log timestamps
// (newer replays, which no longer network the running time).
type timing struct {
	TickInterval     float64  `json:"tickInterval"`
	ServerTimeOffset *float64 `json:"serverTimeOffset"`
	Source           string   `json:"source"`
	GameStartTime    float64  `json:"gameStartTime"`
	PreGameStartTime float64  `json:"preGameStartTime"`
	TransitionTime   float64  `json:"transitionTime"`
}

type matchInfo struct {
	ID        string `json:"id"`
	FileName  string `json:"fileName"`
	Winner    int32  `json:"winner"`
	GameMode  int32  `json:"gameMode"`
	EndTime   string `json:"endTime"`
	GameBuild uint32 `json:"gameBuild"`
	LastTick  uint32 `json:"lastTick"`
	TickRate  int    `json:"tickRate"`
	Map       mapDef `json:"map"`
}

type playerTrack struct {
	ID      int32  `json:"id"`
	Slot    int32  `json:"slot"`
	Team    int32  `json:"team"`
	Name    string `json:"name"`
	SteamID string `json:"steamId"`
	Hero    string `json:"hero"`
	HeroID  int32  `json:"heroId"`
	Color   string `json:"color"`

	X        []int32   `json:"x"`
	Y        []int32   `json:"y"`
	Alive    []int8    `json:"alive"`
	HP       []int8    `json:"hp"`
	Mana     []int8    `json:"mana"`
	Respawn  []int16   `json:"respawn"`
	Level    []int8    `json:"level"`
	Kills    []int16   `json:"kills"`
	Deaths   []int16   `json:"deaths"`
	Assists  []int16   `json:"assists"`
	NetWorth []int32   `json:"netWorth"`
	XP       []int32   `json:"xp"`
	Items    [][]int16 `json:"items"`

	// Cumulative farming statistics from the team data entity; -1 when the
	// replay's build does not network a field.
	LastHits     []int16 `json:"lastHits"`
	Denies       []int16 `json:"denies"`
	EarnedGold   []int32 `json:"earnedGold"`
	CreepGold    []int32 `json:"creepGold"`
	NeutralGold  []int32 `json:"neutralGold"`
	HeroGold     []int32 `json:"heroGold"`
	BuildingGold []int32 `json:"buildingGold"`
	IncomeGold   []int32 `json:"incomeGold"`
	Stacks       []int8  `json:"stacks"`
}

// pad extends every per-sample array to n entries with absent markers.
func (pt *playerTrack) pad(n int) {
	for len(pt.X) < n {
		pt.X = append(pt.X, absent)
		pt.Y = append(pt.Y, absent)
		pt.Alive = append(pt.Alive, absent)
		pt.HP = append(pt.HP, absent)
		pt.Mana = append(pt.Mana, absent)
		pt.Respawn = append(pt.Respawn, absent)
		pt.Level = append(pt.Level, absent)
		pt.Kills = append(pt.Kills, absent)
		pt.Deaths = append(pt.Deaths, absent)
		pt.Assists = append(pt.Assists, absent)
		pt.NetWorth = append(pt.NetWorth, absent)
		pt.XP = append(pt.XP, absent)
		pt.Items = append(pt.Items, nil)
		pt.LastHits = append(pt.LastHits, absent)
		pt.Denies = append(pt.Denies, absent)
		pt.EarnedGold = append(pt.EarnedGold, absent)
		pt.CreepGold = append(pt.CreepGold, absent)
		pt.NeutralGold = append(pt.NeutralGold, absent)
		pt.HeroGold = append(pt.HeroGold, absent)
		pt.BuildingGold = append(pt.BuildingGold, absent)
		pt.IncomeGold = append(pt.IncomeGold, absent)
		pt.Stacks = append(pt.Stacks, absent)
	}
}

// unitTrack samples a non-hero unit's position: couriers and Roshan.
type unitTrack struct {
	Kind  string  `json:"kind"`
	Team  int32   `json:"team"`
	Owner int32   `json:"owner"`
	X     []int32 `json:"x"`
	Y     []int32 `json:"y"`
	Alive []int8  `json:"alive"`

	e *manta.Entity
}

func (ut *unitTrack) pad(n int) {
	for len(ut.X) < n {
		ut.X = append(ut.X, absent)
		ut.Y = append(ut.Y, absent)
		ut.Alive = append(ut.Alive, absent)
	}
}

type building struct {
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	Team          int32   `json:"team"`
	X             int32   `json:"x"`
	Y             int32   `json:"y"`
	DestroyedTick *uint32 `json:"destroyedTick"`
}

type kill struct {
	Tick       uint32  `json:"tick"`
	Clock      float32 `json:"clock"`
	Victim     int32   `json:"victim"`
	Killer     int32   `json:"killer"`
	KillerName string  `json:"killerName"`
	X          int32   `json:"x"`
	Y          int32   `json:"y"`
	Assists    []int32 `json:"assists"`
}

type ward struct {
	Kind        string  `json:"kind"`
	Team        int32   `json:"team"`
	Owner       int32   `json:"owner"`
	X           int32   `json:"x"`
	Y           int32   `json:"y"`
	PlacedTick  uint32  `json:"placedTick"`
	RemovedTick *uint32 `json:"removedTick"`
}

type chatLine struct {
	Tick    uint32  `json:"tick"`
	Clock   float32 `json:"clock"`
	Player  int32   `json:"player"`
	Name    string  `json:"name,omitempty"`
	Channel string  `json:"channel"`
	Text    string  `json:"text"`
}

type chatEvent struct {
	Tick    uint32  `json:"tick"`
	Clock   float32 `json:"clock"`
	Type    string  `json:"type"`
	Value   uint32  `json:"value"`
	Players []int32 `json:"players"`
}

// farmEvent is a creep kill credited to a hero, positioned where the hero
// stood. Kind is lane, deny, neutral, ancient or roshan.
type farmEvent struct {
	Tick   uint32  `json:"tick"`
	Clock  float32 `json:"clock"`
	Player int32   `json:"player"`
	Kind   string  `json:"kind"`
	Target int16   `json:"target"`
	X      int32   `json:"x"`
	Y      int32   `json:"y"`
}

type purchase struct {
	Tick   uint32  `json:"tick"`
	Clock  float32 `json:"clock"`
	Player int32   `json:"player"`
	Item   string  `json:"item"`
}

// ancientNeutrals lists the ancient camp creeps; any neutral whose name
// contains "ancient" also counts.
var ancientNeutrals = map[string]bool{
	"npc_dota_neutral_black_drake":          true,
	"npc_dota_neutral_black_dragon":         true,
	"npc_dota_neutral_granite_golem":        true,
	"npc_dota_neutral_rock_golem":           true,
	"npc_dota_neutral_big_thunder_lizard":   true,
	"npc_dota_neutral_small_thunder_lizard": true,
	"npc_dota_neutral_prowler_acolyte":      true,
	"npc_dota_neutral_prowler_shaman":       true,
	"npc_dota_neutral_frostbitten_golem":    true,
	"npc_dota_neutral_ice_shaman":           true,
}

// buildingKinds maps entity classes to the building kinds the viewer draws.
var buildingKinds = map[string]string{
	"CDOTA_BaseNPC_Tower":       "tower",
	"CDOTA_BaseNPC_Barracks":    "rax",
	"CDOTA_BaseNPC_Fort":        "fort",
	"CDOTA_BaseNPC_Watch_Tower": "outpost",
}

var wardKinds = map[string]string{
	"CDOTA_NPC_Observer_Ward":           "observer",
	"CDOTA_NPC_Observer_Ward_TrueSight": "sentry",
}

// playerKeys caches the PlayerResource field names for one player id.
type playerKeys struct {
	team, slot, name, steam, hero, heroID, kills, deaths, assists, level, respawn string
}

type ExtractOptions struct {
	Interval   uint32
	FileName   string
	MapVersion string // optional override of the map edition
}

type extractor struct {
	p    *manta.Parser
	opts ExtractOptions
	data *MatchData

	nextSample uint32

	gamerules, playerResource, dataRadiant, dataDire *manta.Entity

	players   map[int32]*playerTrack
	keys      map[int32]*playerKeys
	teamKeys  map[int32]*teamSlotKeys
	itemIDs   map[string]int16
	targetIDs map[string]int16
	couriers  map[int32]*unitTrack // by entity index
	roshan    *unitTrack
	buildings map[int32]*building
	wards     map[int32]*ward
	heroOwner map[string]int32 // hero unit name -> player id
	nameCache map[int32]string // EntityNames index -> key

	// Clock calibration, resolved after the parse (see finishClocks).
	tickInterval float64
	ruleOffsets  []float64 // m_fGameTime - tick*interval, when the field exists
	logOffsets   []float64 // combat log timestamp - tick*interval
	startTime    float64
	preStartTime float64
	transition   float64
}

// maxOffsetSamples bounds the calibration samples kept for the median.
const maxOffsetSamples = 4096

// extract parses the replay once and builds the viewer document.
func Extract(buf []byte, opts ExtractOptions) (*MatchData, error) {
	p, err := manta.NewParser(buf)
	if err != nil {
		return nil, err
	}
	if opts.Interval == 0 {
		opts.Interval = tickRate
	}

	x := &extractor{
		p:    p,
		opts: opts,
		data: &MatchData{
			Interval:    opts.Interval,
			Players:     []*playerTrack{},
			ItemNames:   []string{},
			Couriers:    []*unitTrack{},
			Buildings:   []*building{},
			Kills:       []kill{},
			Wards:       []*ward{},
			Chat:        []chatLine{},
			Events:      []chatEvent{},
			FarmEvents:  []farmEvent{},
			FarmTargets: []string{},
			Purchases:   []purchase{},
		},
		players:   make(map[int32]*playerTrack),
		keys:      make(map[int32]*playerKeys),
		teamKeys:  make(map[int32]*teamSlotKeys),
		itemIDs:   make(map[string]int16),
		targetIDs: make(map[string]int16),
		couriers:  make(map[int32]*unitTrack),
		roshan:    &unitTrack{Kind: "roshan", Owner: absent, X: []int32{}, Y: []int32{}, Alive: []int8{}},
		buildings: make(map[int32]*building),
		wards:     make(map[int32]*ward),
		heroOwner: make(map[string]int32),
		nameCache: make(map[int32]string),

		tickInterval: 1.0 / tickRate,
	}
	x.data.Roshan = x.roshan
	x.data.Match.FileName = opts.FileName
	x.data.Match.TickRate = tickRate

	var fileInfo *dota.CDemoFileInfo
	p.Callbacks.OnCDemoFileInfo(func(m *dota.CDemoFileInfo) error {
		fileInfo = m
		return nil
	})

	p.Callbacks.OnCSVCMsg_ServerInfo(func(m *dota.CSVCMsg_ServerInfo) error {
		if ti := float64(m.GetTickInterval()); ti > 0 {
			x.tickInterval = ti
		}
		return nil
	})

	p.OnEntity(x.onEntity)

	// Sample after each packet has been applied. Manta dispatches the internal
	// packet handler first, so entity state is current here.
	p.Callbacks.OnCDemoPacket(func(m *dota.CDemoPacket) error {
		x.maybeSample()
		return nil
	})
	p.Callbacks.OnCDemoFullPacket(func(m *dota.CDemoFullPacket) error {
		x.maybeSample()
		return nil
	})

	p.Callbacks.OnCMsgDOTACombatLogEntry(x.onCombatLog)
	p.Callbacks.OnCDOTAUserMsg_ChatMessage(x.onChatMessage)
	p.Callbacks.OnCUserMessageSayText2(x.onSayText2)
	p.Callbacks.OnCDOTAUserMsg_ChatEvent(x.onChatEvent)

	parseErr := p.Start()

	x.data.Match.GameBuild = p.GameBuild
	x.data.Match.LastTick = p.Tick

	endTime := time.Time{}
	if fileInfo != nil {
		if gi := fileInfo.GetGameInfo().GetDota(); gi != nil {
			x.data.Match.ID = strconv.FormatUint(gi.GetMatchId(), 10)
			x.data.Match.Winner = gi.GetGameWinner()
			x.data.Match.GameMode = gi.GetGameMode()
			endTime = time.Unix(int64(gi.GetEndTime()), 0).UTC()
			x.data.Match.EndTime = endTime.Format(time.RFC3339)
		}
	}
	if m, ok := mapByID(opts.MapVersion); ok {
		x.data.Match.Map = m
	} else {
		x.data.Match.Map = mapForTime(endTime)
	}

	x.finishClocks()

	// Present players in scoreboard order: Radiant first, then by slot.
	for _, pt := range x.players {
		x.data.Players = append(x.data.Players, pt)
	}
	sort.Slice(x.data.Players, func(i, j int) bool {
		a, b := x.data.Players[i], x.data.Players[j]
		if a.Team != b.Team {
			return a.Team < b.Team
		}
		return a.Slot < b.Slot
	})

	return x.data, parseErr
}

// --- sampling ------------------------------------------------------------------

func (x *extractor) maybeSample() {
	if x.p.Tick < x.nextSample {
		return
	}
	x.sample()
	// Ticks are not contiguous in the demo; schedule the next sample on the
	// interval grid rather than repeating a stale state across a gap.
	x.nextSample = (x.p.Tick/x.opts.Interval + 1) * x.opts.Interval
}

func (x *extractor) sample() {
	n := len(x.data.Ticks)
	x.data.Ticks = append(x.data.Ticks, x.p.Tick)
	x.data.Clock = append(x.data.Clock, clockUnknown)
	x.data.TimeOfDay = append(x.data.TimeOfDay, x.timeOfDay())
	x.observeGameRules()

	if x.playerResource != nil {
		for id := int32(0); id < maxPlayerID; id++ {
			x.samplePlayer(id, n)
		}
	}
	for _, pt := range x.players {
		pt.pad(n + 1)
	}

	for _, c := range x.couriers {
		c.pad(n)
		x.sampleUnit(c)
	}
	for _, c := range x.couriers {
		c.pad(n + 1)
	}

	x.roshan.pad(n)
	x.sampleUnit(x.roshan)
}

func (x *extractor) samplePlayer(id int32, n int) {
	k := x.keysFor(id)
	team, ok := getNum(x.playerResource, k.team)
	if !ok || (team != teamRadiant && team != teamDire) {
		return
	}

	pt := x.players[id]
	if pt == nil {
		pt = &playerTrack{ID: id, Team: int32(team), Items: [][]int16{}}
		x.players[id] = pt
	}
	pt.pad(n)
	pt.Team = int32(team)
	if slot, ok := getNum(x.playerResource, k.slot); ok {
		pt.Slot = int32(slot)
	}
	pt.Color = playerColor(pt.Team, pt.Slot)
	if name, ok := x.playerResource.GetString(k.name); ok && name != "" {
		pt.Name = name
	}
	if steam, ok := x.playerResource.GetUint64(k.steam); ok && steam != 0 {
		pt.SteamID = strconv.FormatUint(steam, 10)
	}
	if id, ok := getNum(x.playerResource, k.heroID); ok && id > 0 {
		pt.HeroID = int32(id)
	}

	var hero *manta.Entity
	if h, ok := x.playerResource.GetUint64(k.hero); ok && h != invalidHandle {
		hero = x.p.FindEntityByHandle(h)
	}

	kills, _ := getNum(x.playerResource, k.kills)
	deaths, _ := getNum(x.playerResource, k.deaths)
	assists, _ := getNum(x.playerResource, k.assists)
	pt.Kills = append(pt.Kills, int16(kills))
	pt.Deaths = append(pt.Deaths, int16(deaths))
	pt.Assists = append(pt.Assists, int16(assists))
	if respawn, ok := getNum(x.playerResource, k.respawn); ok {
		pt.Respawn = append(pt.Respawn, int16(respawn))
	} else {
		pt.Respawn = append(pt.Respawn, absent)
	}

	st := x.teamStats(pt.Team, pt.Slot)
	pt.NetWorth = append(pt.NetWorth, st.netWorth)
	pt.XP = append(pt.XP, st.xp)
	pt.LastHits = append(pt.LastHits, int16(st.lastHits))
	pt.Denies = append(pt.Denies, int16(st.denies))
	pt.EarnedGold = append(pt.EarnedGold, st.earned)
	pt.CreepGold = append(pt.CreepGold, st.creepGold)
	pt.NeutralGold = append(pt.NeutralGold, st.neutralGold)
	pt.HeroGold = append(pt.HeroGold, st.heroGold)
	pt.BuildingGold = append(pt.BuildingGold, st.buildingGold)
	pt.IncomeGold = append(pt.IncomeGold, st.incomeGold)
	pt.Stacks = append(pt.Stacks, int8(st.stacks))

	if hero == nil {
		pt.X = append(pt.X, absent)
		pt.Y = append(pt.Y, absent)
		pt.Alive = append(pt.Alive, absent)
		pt.HP = append(pt.HP, absent)
		pt.Mana = append(pt.Mana, absent)
		lvl, ok := getNum(x.playerResource, k.level)
		if !ok {
			lvl = absent
		}
		pt.Level = append(pt.Level, int8(lvl))
		pt.Items = append(pt.Items, nil)
		return
	}

	if pt.Hero == "" {
		if name := x.entityName(hero); strings.HasPrefix(name, "npc_dota_hero_") {
			pt.Hero = name
			x.heroOwner[name] = id
		}
	}

	px, py := position(hero)
	pt.X = append(pt.X, px)
	pt.Y = append(pt.Y, py)

	life, _ := getNum(hero, "m_lifeState")
	if life == 0 {
		pt.Alive = append(pt.Alive, 1)
	} else {
		pt.Alive = append(pt.Alive, 0)
	}

	hp, _ := getNum(hero, "m_iHealth")
	maxHP, _ := getNum(hero, "m_iMaxHealth")
	if maxHP > 0 {
		pt.HP = append(pt.HP, int8(hp*100/maxHP+0.5))
	} else {
		pt.HP = append(pt.HP, absent)
	}
	mana, _ := getNum(hero, "m_flMana")
	maxMana, _ := getNum(hero, "m_flMaxMana")
	if maxMana > 0 {
		pt.Mana = append(pt.Mana, int8(mana*100/maxMana+0.5))
	} else {
		pt.Mana = append(pt.Mana, absent)
	}

	lvl, ok := getNum(hero, "m_iCurrentLevel")
	if !ok {
		lvl, _ = getNum(x.playerResource, k.level)
	}
	pt.Level = append(pt.Level, int8(lvl))

	items := make([]int16, len(itemSlots))
	for i, slot := range itemSlots {
		items[i] = absent
		h, ok := hero.GetUint32(fmt.Sprintf("m_hItems.%04d", slot))
		if !ok || h == invalidHandle {
			continue
		}
		item := x.p.FindEntityByHandle(uint64(h))
		if item == nil {
			continue
		}
		if name := x.entityName(item); name != "" {
			items[i] = x.itemID(name)
		}
	}
	pt.Items = append(pt.Items, items)
}

// teamSlotKeys caches the team data field names for one team slot.
type teamSlotKeys struct {
	netWorth, totalGold, xp, lastHits, denies, creepGold, neutralGold, heroGold, buildingGold, incomeGold, stacks string
}

func teamSlotKeysFor(slot int32) *teamSlotKeys {
	f := func(name string) string { return fmt.Sprintf("m_vecDataTeam.%04d.%s", slot, name) }
	return &teamSlotKeys{
		netWorth:     f("m_iNetWorth"),
		totalGold:    f("m_iTotalEarnedGold"),
		xp:           f("m_iTotalEarnedXP"),
		lastHits:     f("m_iLastHitCount"),
		denies:       f("m_iDenyCount"),
		creepGold:    f("m_iCreepKillGold"),
		neutralGold:  f("m_iNeutralKillGold"),
		heroGold:     f("m_iHeroKillGold"),
		buildingGold: f("m_iBuildingGold"),
		incomeGold:   f("m_iIncomeGold"),
		stacks:       f("m_iCampsStacked"),
	}
}

// slotStats are one player's cumulative statistics at a sample; fields the
// replay does not network are -1.
type slotStats struct {
	netWorth, xp, earned, lastHits, denies, creepGold, neutralGold, heroGold, buildingGold, incomeGold, stacks int32
}

// teamStats reads a team slot's statistics from the team data entity. Older
// replays have no net worth field; total earned gold stands in.
func (x *extractor) teamStats(team, slot int32) slotStats {
	st := slotStats{absent, absent, absent, absent, absent, absent, absent, absent, absent, absent, absent}
	var e *manta.Entity
	switch team {
	case teamRadiant:
		e = x.dataRadiant
	case teamDire:
		e = x.dataDire
	}
	if e == nil {
		return st
	}
	keys, ok := x.teamKeys[slot]
	if !ok {
		keys = teamSlotKeysFor(slot)
		x.teamKeys[slot] = keys
	}
	read := func(key string) int32 {
		if v, ok := getNum(e, key); ok {
			return int32(v)
		}
		return absent
	}
	st.earned = read(keys.totalGold)
	if st.netWorth = read(keys.netWorth); st.netWorth == absent {
		st.netWorth = st.earned
	}
	st.xp = read(keys.xp)
	st.lastHits = read(keys.lastHits)
	st.denies = read(keys.denies)
	st.creepGold = read(keys.creepGold)
	st.neutralGold = read(keys.neutralGold)
	st.heroGold = read(keys.heroGold)
	st.buildingGold = read(keys.buildingGold)
	st.incomeGold = read(keys.incomeGold)
	st.stacks = read(keys.stacks)
	return st
}

func (x *extractor) sampleUnit(u *unitTrack) {
	if u.e == nil {
		u.X = append(u.X, absent)
		u.Y = append(u.Y, absent)
		u.Alive = append(u.Alive, absent)
		return
	}
	px, py := position(u.e)
	u.X = append(u.X, px)
	u.Y = append(u.Y, py)
	if life, _ := getNum(u.e, "m_lifeState"); life == 0 {
		u.Alive = append(u.Alive, 1)
	} else {
		u.Alive = append(u.Alive, 0)
	}
	if u.Team == 0 {
		if t, ok := getNum(u.e, "m_iTeamNum"); ok {
			u.Team = int32(t)
		}
	}
	if u.Owner == absent && u.Kind == "courier" {
		u.Owner = x.ownerPlayer(u.e)
	}
}

// timeOfDay returns the game rules' networked time of day, a 16-bit counter
// where day spans the middle half of the cycle, or -1 when unavailable.
func (x *extractor) timeOfDay() int32 {
	if x.gamerules == nil {
		return absent
	}
	if v, ok := getNum(x.gamerules, "m_pGameRules.m_iNetTimeOfDay"); ok {
		return int32(v)
	}
	return absent
}

// observeGameRules records the game rules timing fields and, when the running
// game time is networked, a server-time calibration sample.
func (x *extractor) observeGameRules() {
	g := x.gamerules
	if g == nil {
		return
	}
	if v, ok := getNum(g, "m_pGameRules.m_flGameStartTime"); ok && v > 0 {
		x.startTime = v
	}
	if v, ok := getNum(g, "m_pGameRules.m_flPreGameStartTime"); ok && v > 0 {
		x.preStartTime = v
	}
	if v, ok := getNum(g, "m_pGameRules.m_flStateTransitionTime"); ok && v > 0 {
		x.transition = v
	}
	if gameTime, ok := getNum(g, "m_pGameRules.m_fGameTime"); ok && gameTime > 0 && len(x.ruleOffsets) < maxOffsetSamples {
		x.ruleOffsets = append(x.ruleOffsets, gameTime-float64(x.p.Tick)*x.tickInterval)
	}
}

// observeCombatLogTime records a calibration sample from a combat log entry's
// server timestamp.
func (x *extractor) observeCombatLogTime(m *dota.CMsgDOTACombatLogEntry) {
	if ts := float64(m.GetTimestamp()); ts > 0 && len(x.logOffsets) < maxOffsetSamples {
		x.logOffsets = append(x.logOffsets, ts-float64(x.p.Tick)*x.tickInterval)
	}
}

// finishClocks resolves the tick -> clock mapping and fills in every clock
// value in the document.
func (x *extractor) finishClocks() {
	t := &x.data.Timing
	t.TickInterval = x.tickInterval
	t.GameStartTime = x.startTime
	t.PreGameStartTime = x.preStartTime
	t.TransitionTime = x.transition

	offset := math.NaN()
	switch {
	case len(x.ruleOffsets) > 0:
		offset = median(x.ruleOffsets)
		t.Source = "gamerules"
	case len(x.logOffsets) > 0:
		offset = median(x.logOffsets)
		t.Source = "combatlog"
	default:
		t.Source = "none"
	}
	if !math.IsNaN(offset) {
		v := offset
		t.ServerTimeOffset = &v
	}

	for i, tick := range x.data.Ticks {
		x.data.Clock[i] = x.clockForTick(tick, offset)
	}
	for i := range x.data.Kills {
		x.data.Kills[i].Clock = x.clockForTick(x.data.Kills[i].Tick, offset)
	}
	for i := range x.data.Chat {
		x.data.Chat[i].Clock = x.clockForTick(x.data.Chat[i].Tick, offset)
	}
	for i := range x.data.Events {
		x.data.Events[i].Clock = x.clockForTick(x.data.Events[i].Tick, offset)
	}
	for i := range x.data.FarmEvents {
		x.data.FarmEvents[i].Clock = x.clockForTick(x.data.FarmEvents[i].Tick, offset)
	}
	for i := range x.data.Purchases {
		x.data.Purchases[i].Clock = x.clockForTick(x.data.Purchases[i].Tick, offset)
	}
}

// clockForTick converts a tick to the in-game clock: seconds since the horn,
// negative during the pre-game countdown. Pauses are not subtracted.
func (x *extractor) clockForTick(tick uint32, offset float64) float32 {
	if math.IsNaN(offset) {
		return clockUnknown
	}
	serverTime := float64(tick)*x.tickInterval + offset
	switch {
	case x.startTime > 0:
		return float32(serverTime - x.startTime)
	case x.preStartTime > 0 && x.transition > 0:
		return float32(serverTime - x.transition)
	}
	return clockUnknown
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// --- entity tracking -------------------------------------------------------------

func (x *extractor) onEntity(e *manta.Entity, op manta.EntityOp) error {
	class := e.GetClassName()
	switch class {
	case "CDOTAGamerulesProxy":
		x.gamerules = keepUnlessDeleted(e, op)
	case "CDOTA_PlayerResource":
		x.playerResource = keepUnlessDeleted(e, op)
	case "CDOTA_DataRadiant":
		x.dataRadiant = keepUnlessDeleted(e, op)
	case "CDOTA_DataDire":
		x.dataDire = keepUnlessDeleted(e, op)
	case "CDOTA_Unit_Roshan":
		x.roshan.e = keepUnlessDeleted(e, op)
	case "CDOTA_Unit_Courier":
		x.onCourier(e, op)
	default:
		if kind, ok := buildingKinds[class]; ok {
			x.onBuilding(e, op, kind)
		} else if kind, ok := wardKinds[class]; ok {
			x.onWard(e, op, kind)
		}
	}
	return nil
}

func keepUnlessDeleted(e *manta.Entity, op manta.EntityOp) *manta.Entity {
	if op.Flag(manta.EntityOpDeleted) {
		return nil
	}
	return e
}

func (x *extractor) onCourier(e *manta.Entity, op manta.EntityOp) {
	idx := e.GetIndex()
	switch {
	case op.Flag(manta.EntityOpCreated):
		c := &unitTrack{Kind: "courier", Owner: absent, X: []int32{}, Y: []int32{}, Alive: []int8{}, e: e}
		if t, ok := getNum(e, "m_iTeamNum"); ok {
			c.Team = int32(t)
		}
		x.couriers[idx] = c
		x.data.Couriers = append(x.data.Couriers, c)
	case op.Flag(manta.EntityOpDeleted):
		if c := x.couriers[idx]; c != nil {
			c.e = nil
			delete(x.couriers, idx)
		}
	}
}

func (x *extractor) onBuilding(e *manta.Entity, op manta.EntityOp, kind string) {
	idx := e.GetIndex()
	switch {
	case op.Flag(manta.EntityOpCreated):
		b := &building{Kind: kind, Name: x.entityName(e)}
		if t, ok := getNum(e, "m_iTeamNum"); ok {
			b.Team = int32(t)
		}
		b.X, b.Y = position(e)
		x.buildings[idx] = b
		x.data.Buildings = append(x.data.Buildings, b)
	case op.Flag(manta.EntityOpDeleted):
		if b := x.buildings[idx]; b != nil {
			if b.DestroyedTick == nil {
				tick := x.p.Tick
				b.DestroyedTick = &tick
			}
			delete(x.buildings, idx)
		}
	default:
		b := x.buildings[idx]
		if b == nil {
			return
		}
		if b.Name == "" {
			b.Name = x.entityName(e)
		}
		if b.X == 0 && b.Y == 0 {
			b.X, b.Y = position(e)
		}
		if b.DestroyedTick == nil {
			if life, ok := getNum(e, "m_lifeState"); ok && life != 0 {
				tick := x.p.Tick
				b.DestroyedTick = &tick
			}
		}
	}
}

func (x *extractor) onWard(e *manta.Entity, op manta.EntityOp, kind string) {
	idx := e.GetIndex()
	switch {
	case op.Flag(manta.EntityOpCreated):
		w := &ward{Kind: kind, Owner: absent, PlacedTick: x.p.Tick}
		if t, ok := getNum(e, "m_iTeamNum"); ok {
			w.Team = int32(t)
		}
		w.X, w.Y = position(e)
		w.Owner = x.ownerPlayer(e)
		x.wards[idx] = w
		x.data.Wards = append(x.data.Wards, w)
	case op.Flag(manta.EntityOpDeleted):
		if w := x.wards[idx]; w != nil {
			tick := x.p.Tick
			w.RemovedTick = &tick
			delete(x.wards, idx)
		}
	default:
		w := x.wards[idx]
		if w == nil {
			return
		}
		if w.X == 0 && w.Y == 0 {
			w.X, w.Y = position(e)
		}
		if w.Owner == absent {
			w.Owner = x.ownerPlayer(e)
		}
		if w.Team == 0 {
			if t, ok := getNum(e, "m_iTeamNum"); ok {
				w.Team = int32(t)
			}
		}
	}
}

// --- messages --------------------------------------------------------------------

func (x *extractor) onCombatLog(m *dota.CMsgDOTACombatLogEntry) error {
	x.observeCombatLogTime(m)
	switch m.GetType() {
	case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_DEATH:
		x.onDeath(m)
	case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_PURCHASE:
		x.onPurchase(m)
	}
	return nil
}

func (x *extractor) onDeath(m *dota.CMsgDOTACombatLogEntry) {
	if m.GetIsTargetHero() && !m.GetIsTargetIllusion() {
		x.recordKill(m)
		return
	}
	if m.GetIsAttackerHero() && !m.GetIsAttackerIllusion() && !m.GetIsTargetBuilding() {
		x.recordFarm(m)
	}
}

func (x *extractor) recordKill(m *dota.CMsgDOTACombatLogEntry) {
	victimName := x.combatName(m.GetTargetName())
	victim, ok := x.heroOwner[victimName]
	if !ok {
		return
	}

	k := kill{
		Tick:       x.p.Tick,
		Clock:      clockUnknown,
		Victim:     victim,
		Killer:     absent,
		KillerName: x.combatName(m.GetAttackerName()),
		X:          absent,
		Y:          absent,
		Assists:    []int32{},
	}
	if m.GetIsAttackerHero() && !m.GetIsAttackerIllusion() {
		if id, ok := x.heroOwner[k.KillerName]; ok {
			k.Killer = id
		}
	}
	for _, a := range m.GetAssistPlayers() {
		k.Assists = append(k.Assists, a)
	}

	// The combat log carries no coordinates; the victim's hero still exists at
	// this point, so take its position.
	if hero := x.heroEntity(victim); hero != nil {
		k.X, k.Y = position(hero)
	}

	x.data.Kills = append(x.data.Kills, k)
}

// recordFarm credits a creep kill to the attacking hero, positioned where the
// hero stands. Summons, wards and other non-creep units are ignored.
func (x *extractor) recordFarm(m *dota.CMsgDOTACombatLogEntry) {
	attacker := x.combatName(m.GetAttackerName())
	player, ok := x.heroOwner[attacker]
	if !ok {
		return
	}
	target := x.combatName(m.GetTargetName())
	kind := farmKind(target, m.GetAttackerTeam() == m.GetTargetTeam())
	if kind == "" {
		return
	}
	ev := farmEvent{Tick: x.p.Tick, Clock: clockUnknown, Player: player, Kind: kind, Target: x.farmTargetID(target), X: absent, Y: absent}
	if hero := x.heroEntity(player); hero != nil {
		ev.X, ev.Y = position(hero)
	}
	x.data.FarmEvents = append(x.data.FarmEvents, ev)
}

// farmKind classifies a killed unit for farming analysis, or returns "" for
// units that are not farm.
func farmKind(target string, sameTeam bool) string {
	switch {
	case strings.HasPrefix(target, "npc_dota_creep_"):
		if sameTeam {
			return "deny"
		}
		return "lane"
	case strings.HasPrefix(target, "npc_dota_neutral_"):
		if ancientNeutrals[target] || strings.Contains(target, "ancient") {
			return "ancient"
		}
		return "neutral"
	case target == "npc_dota_roshan":
		return "roshan"
	}
	return ""
}

func (x *extractor) onPurchase(m *dota.CMsgDOTACombatLogEntry) {
	player, ok := x.heroOwner[x.combatName(m.GetTargetName())]
	if !ok {
		return
	}
	item := x.combatName(m.GetValue())
	if !strings.HasPrefix(item, "item_") {
		return
	}
	x.data.Purchases = append(x.data.Purchases, purchase{Tick: x.p.Tick, Clock: clockUnknown, Player: player, Item: item})
}

// heroEntity returns the player's current hero entity, if any.
func (x *extractor) heroEntity(player int32) *manta.Entity {
	if x.playerResource == nil {
		return nil
	}
	h, ok := x.playerResource.GetUint64(x.keysFor(player).hero)
	if !ok || h == invalidHandle {
		return nil
	}
	return x.p.FindEntityByHandle(h)
}

func (x *extractor) farmTargetID(name string) int16 {
	if id, ok := x.targetIDs[name]; ok {
		return id
	}
	id := int16(len(x.data.FarmTargets))
	x.data.FarmTargets = append(x.data.FarmTargets, name)
	x.targetIDs[name] = id
	return id
}

func (x *extractor) combatName(index uint32) string {
	name, _ := x.p.LookupStringByIndex("CombatLogNames", int32(index))
	return name
}

func (x *extractor) onChatMessage(m *dota.CDOTAUserMsg_ChatMessage) error {
	x.data.Chat = append(x.data.Chat, chatLine{
		Tick:    x.p.Tick,
		Clock:   clockUnknown,
		Player:  m.GetSourcePlayerId(),
		Channel: chatChannelName(m.GetChannelType()),
		Text:    m.GetMessageText(),
	})
	return nil
}

func (x *extractor) onSayText2(m *dota.CUserMessageSayText2) error {
	line := chatLine{
		Tick:    x.p.Tick,
		Clock:   clockUnknown,
		Player:  absent,
		Name:    m.GetParam1(),
		Channel: "all",
		Text:    m.GetParam2(),
	}
	for _, pt := range x.players {
		if pt.Name == line.Name {
			line.Player = pt.ID
			break
		}
	}
	x.data.Chat = append(x.data.Chat, line)
	return nil
}

func (x *extractor) onChatEvent(m *dota.CDOTAUserMsg_ChatEvent) error {
	ev := chatEvent{
		Tick:    x.p.Tick,
		Clock:   clockUnknown,
		Type:    strings.TrimPrefix(m.GetType().String(), "CHAT_MESSAGE_"),
		Value:   m.GetValue(),
		Players: []int32{},
	}
	// Unused player slots are absent rather than zero (player 0 is a real
	// player), so only fields that are present count.
	for _, id := range []*int32{m.Playerid_1, m.Playerid_2, m.Playerid_3, m.Playerid_4, m.Playerid_5, m.Playerid_6} {
		if id != nil && *id >= 0 && *id < maxPlayerID {
			ev.Players = append(ev.Players, *id)
		}
	}
	x.data.Events = append(x.data.Events, ev)
	return nil
}

// --- helpers ---------------------------------------------------------------------

// chatChannelName names the in-game chat channels the viewer distinguishes;
// other channel types keep their enum name.
func chatChannelName(t uint32) string {
	switch dota.DOTAChatChannelTypeT(t) {
	case dota.DOTAChatChannelTypeT_DOTAChannelType_GameAll:
		return "all"
	case dota.DOTAChatChannelTypeT_DOTAChannelType_GameAllies:
		return "team"
	case dota.DOTAChatChannelTypeT_DOTAChannelType_GameSpectator:
		return "spectator"
	}
	return strings.TrimPrefix(dota.DOTAChatChannelTypeT(t).String(), "DOTAChannelType_")
}

func (x *extractor) keysFor(id int32) *playerKeys {
	if k := x.keys[id]; k != nil {
		return k
	}
	d := fmt.Sprintf("m_vecPlayerData.%04d.", id)
	t := fmt.Sprintf("m_vecPlayerTeamData.%04d.", id)
	k := &playerKeys{
		team:    d + "m_iPlayerTeam",
		name:    d + "m_iszPlayerName",
		steam:   d + "m_iPlayerSteamID",
		slot:    t + "m_iTeamSlot",
		hero:    t + "m_hSelectedHero",
		heroID:  t + "m_nSelectedHeroID",
		kills:   t + "m_iKills",
		deaths:  t + "m_iDeaths",
		assists: t + "m_iAssists",
		level:   t + "m_iLevel",
		respawn: t + "m_iRespawnSeconds",
	}
	x.keys[id] = k
	return k
}

// entityName resolves a unit's internal name (npc_dota_hero_kez, item_blink,
// dota_goodguys_tower1_top) through the EntityNames string table.
func (x *extractor) entityName(e *manta.Entity) string {
	idx, ok := getNum(e, "m_pEntity.m_nameStringTableIndex")
	if !ok {
		if idx, ok = getNum(e, "m_pEntity.m_nameStringableIndex"); !ok {
			return ""
		}
	}
	i := int32(idx)
	if name, ok := x.nameCache[i]; ok {
		return name
	}
	name, _ := x.p.LookupStringByIndex("EntityNames", i)
	if name != "" {
		x.nameCache[i] = name
	}
	return name
}

// ownerPlayer follows m_hOwnerEntity to a player id, via either a hero or a
// player controller entity.
func (x *extractor) ownerPlayer(e *manta.Entity) int32 {
	h, ok := e.GetUint64("m_hOwnerEntity")
	if !ok || h == invalidHandle {
		return absent
	}
	owner := x.p.FindEntityByHandle(h)
	if owner == nil {
		return absent
	}
	if id, ok := getNum(owner, "m_nPlayerID"); ok {
		return int32(id)
	}
	if id, ok := getNum(owner, "m_iPlayerID"); ok {
		return int32(id)
	}
	return absent
}

func (x *extractor) itemID(name string) int16 {
	if id, ok := x.itemIDs[name]; ok {
		return id
	}
	id := int16(len(x.data.ItemNames))
	x.data.ItemNames = append(x.data.ItemNames, name)
	x.itemIDs[name] = id
	return id
}

// position returns an entity's world coordinates, rounded to whole units.
func position(e *manta.Entity) (int32, int32) {
	cx, okX := getNum(e, "CBodyComponent.m_cellX")
	cy, okY := getNum(e, "CBodyComponent.m_cellY")
	if !okX || !okY {
		return absent, absent
	}
	vx, _ := getNum(e, "CBodyComponent.m_vecX")
	vy, _ := getNum(e, "CBodyComponent.m_vecY")
	return int32(cx*cellSize + vx + 0.5), int32(cy*cellSize + vy + 0.5)
}

// getNum reads a numeric field of any integer or float type as a float64.
func getNum(e *manta.Entity, name string) (float64, bool) {
	switch v := e.Get(name).(type) {
	case int32:
		return float64(v), true
	case uint32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float32:
		return float64(v), true
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int8:
		return float64(v), true
	case uint8:
		return float64(v), true
	case int16:
		return float64(v), true
	case uint16:
		return float64(v), true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func playerColor(team, slot int32) string {
	i := int(slot)
	if team == teamDire {
		i += 5
	}
	if i < 0 || i >= len(playerColors) {
		return "#DDDDDD"
	}
	return playerColors[i]
}

// heroDisplayName turns npc_dota_hero_shadow_shaman into "Shadow Shaman".
func heroDisplayName(npcName string) string {
	s := strings.TrimPrefix(npcName, "npc_dota_hero_")
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}
