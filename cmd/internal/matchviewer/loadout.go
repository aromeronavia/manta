package matchviewer

import (
	"fmt"
	"math"
	"strings"

	"github.com/dotabuff/manta"
)

// FormatVersion identifies the layout of MatchData. The server re-extracts
// matches saved by an older extractor, so bump it whenever the viewer comes
// to depend on a new field.
const FormatVersion = 2

// maxAbilitySlots bounds the hero ability array walked per sample.
const maxAbilitySlots = 32

// cooldownTolerance is how far a slot's cooldown end may drift between
// samples before it counts as a new cast.
const cooldownTolerance = 1.0

// abilityEvent records a change in one of a hero's ability slots: which
// ability sits there (an index into AbilityNames, absent when the slot is
// empty or holds nothing worth showing) and its level.
type abilityEvent struct {
	Tick   uint32 `json:"tick"`
	Player int32  `json:"player"`
	Slot   int8   `json:"slot"`
	Name   int16  `json:"name"`
	Level  int8   `json:"level"`
}

// cooldownEvent records a cooldown starting on an ability or item slot, or
// being cut short. End is the clock at which it ends and Len its full length;
// a reset has Len 0 and ends at once. Item slots index itemSlots, the same
// order as playerTrack.Items.
type cooldownEvent struct {
	Tick   uint32  `json:"tick"`
	Player int32   `json:"player"`
	Kind   string  `json:"kind"`
	Slot   int8    `json:"slot"`
	End    float32 `json:"end"`
	Len    float32 `json:"len"`

	remaining float64 // seconds left at Tick; becomes End once clocks are known
}

// chargeEvent records the charge count shown on an item slot changing;
// absent when the item there displays no charges.
type chargeEvent struct {
	Tick    uint32 `json:"tick"`
	Player  int32  `json:"player"`
	Slot    int8   `json:"slot"`
	Charges int16  `json:"charges"`
}

type abilitySlot struct {
	name  int16
	level int8
}

// loadout is the last recorded state of one player's ability and item slots,
// so that only changes are written to the document.
type loadout struct {
	abilities []abilitySlot
	abilityCD []float64 // server time at which the recorded cooldown ends
	items     []int16
	itemCD    []float64
	charges   []int16
}

func newLoadout() *loadout {
	l := &loadout{
		abilities: make([]abilitySlot, maxAbilitySlots),
		abilityCD: make([]float64, maxAbilitySlots),
		items:     make([]int16, len(itemSlots)),
		itemCD:    make([]float64, len(itemSlots)),
		charges:   make([]int16, len(itemSlots)),
	}
	for i := range l.abilities {
		l.abilities[i] = abilitySlot{name: absent}
	}
	for i := range l.items {
		l.items[i], l.charges[i] = absent, absent
	}
	return l
}

// Replays since 2023 call the hero ability array m_vecAbilities; older ones
// m_hAbilities. Field names are built once rather than per sample.
var (
	abilityKeys    = slotKeys("m_vecAbilities.%04d", maxAbilitySlots)
	abilityKeysOld = slotKeys("m_hAbilities.%04d", maxAbilitySlots)
	itemKeys       = func() []string {
		keys := make([]string, len(itemSlots))
		for i, slot := range itemSlots {
			keys[i] = fmt.Sprintf("m_hItems.%04d", slot)
		}
		return keys
	}()
)

func slotKeys(format string, n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf(format, i)
	}
	return keys
}

// abilityShown reports whether an ability belongs on the hero card: talents,
// the attribute bonus, engine placeholders and hidden abilities do not.
func abilityShown(name string, hidden bool) bool {
	if hidden || name == "" || name == "attribute_bonus" || name == "generic_hidden" {
		return false
	}
	for _, prefix := range []string{"special_bonus_", "ability_", "plus_", "seasonal_", "twin_gate_"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

// cooldownRemaining turns the networked cooldown field into seconds left.
// Current replays send the remaining time; older ones the game time at which
// the cooldown ends, recognisable because it exceeds any cooldown length.
func cooldownRemaining(cooldown, length, gameTime float64) float64 {
	if cooldown <= 0 {
		return 0
	}
	if gameTime > 0 && cooldown > length+1 {
		cooldown -= gameTime
	}
	return math.Max(0, cooldown)
}

// itemCharges is the count the game would print on the item, or absent for
// items that never show one.
func itemCharges(e *manta.Entity) int16 {
	charges, _ := getNum(e, "m_iCurrentCharges")
	initial, _ := getNum(e, "m_iInitialCharges")
	requires, _ := e.GetBool("m_bRequiresCharges")
	stackable, _ := e.GetBool("m_bStackable")
	if charges > 0 || initial > 0 || requires || stackable {
		return int16(charges)
	}
	return absent
}

// heroItems reads the recorded inventory slots: item ids for the document
// and the entities behind them for cooldowns and charges.
func (x *extractor) heroItems(hero *manta.Entity) ([]int16, []*manta.Entity) {
	items := make([]int16, len(itemSlots))
	ents := make([]*manta.Entity, len(itemSlots))
	for i, key := range itemKeys {
		items[i] = absent
		h, ok := hero.GetUint32(key)
		if !ok || h == invalidHandle {
			continue
		}
		item := x.p.FindEntityByHandle(uint64(h))
		if item == nil {
			continue
		}
		if name := x.entityName(item); name != "" {
			items[i] = x.itemID(name)
			ents[i] = item
		}
	}
	return items, ents
}

// sampleLoadout records what changed in a hero's ability and item slots since
// the previous sample: occupants and levels, cooldowns and charges.
func (x *extractor) sampleLoadout(id int32, hero *manta.Entity, items []int16, ents []*manta.Entity) {
	l := x.loadouts[id]
	if l == nil {
		l = newLoadout()
		x.loadouts[id] = l
	}
	now := float64(x.p.Tick) * x.tickInterval

	keys := abilityKeys
	if _, ok := hero.GetUint32(keys[0]); !ok {
		keys = abilityKeysOld
	}
	for slot, key := range keys {
		h, ok := hero.GetUint32(key)
		if !ok {
			break
		}
		cur := abilitySlot{name: absent}
		var a *manta.Entity
		if h != invalidHandle {
			if a = x.p.FindEntityByHandle(uint64(h)); a != nil {
				hidden, _ := a.GetBool("m_bHidden")
				if name := x.entityName(a); abilityShown(name, hidden) {
					lvl, _ := getNum(a, "m_iLevel")
					cur = abilitySlot{name: x.abilityID(name), level: int8(lvl)}
				}
			}
		}
		prev := &l.abilities[slot]
		fresh := cur.name != prev.name
		if cur != *prev {
			x.data.Abilities = append(x.data.Abilities, abilityEvent{Tick: x.p.Tick, Player: id, Slot: int8(slot), Name: cur.name, Level: cur.level})
			*prev = cur
		}
		if cur.name == absent {
			a = nil
		}
		x.sampleCooldown(id, "ability", slot, a, &l.abilityCD[slot], now, fresh)
	}

	for i, item := range items {
		fresh := item != l.items[i]
		l.items[i] = item
		e := ents[i]
		x.sampleCooldown(id, "item", i, e, &l.itemCD[i], now, fresh)
		charges := int16(absent)
		if e != nil {
			charges = itemCharges(e)
		}
		if charges != l.charges[i] {
			x.data.Charges = append(x.data.Charges, chargeEvent{Tick: x.p.Tick, Player: id, Slot: int8(i), Charges: charges})
			l.charges[i] = charges
		}
	}
}

// sampleCooldown reads a slot's cooldown from the ability or item in it (nil
// when the slot is empty) and records what changed.
func (x *extractor) sampleCooldown(id int32, kind string, slot int, e *manta.Entity, prevEnd *float64, now float64, fresh bool) {
	var remaining, length float64
	if e != nil {
		cd, _ := getNum(e, "m_fCooldown")
		length, _ = getNum(e, "m_flCooldownLength")
		remaining = cooldownRemaining(cd, length, x.gameTime)
	}
	x.recordCooldown(id, kind, slot, remaining, length, prevEnd, now, fresh)
}

// recordCooldown emits a cooldown event when a slot's cooldown differs from
// the one last recorded: a new cast, or an early reset. prevEnd is the
// recorded cooldown's end in server time; fresh means a different ability or
// item now occupies the slot, so nothing recorded for it still applies.
func (x *extractor) recordCooldown(id int32, kind string, slot int, remaining, length float64, prevEnd *float64, now float64, fresh bool) {
	if remaining > 0 {
		end := now + remaining
		if fresh || math.Abs(end-*prevEnd) > cooldownTolerance {
			x.data.Cooldowns = append(x.data.Cooldowns, cooldownEvent{
				Tick: x.p.Tick, Player: id, Kind: kind, Slot: int8(slot),
				Len: float32(math.Round(length*10) / 10), remaining: remaining,
			})
		}
		*prevEnd = end
		return
	}
	if *prevEnd > now+cooldownTolerance {
		// The recorded cooldown would still be running: it was reset early or
		// its owner left the slot.
		x.data.Cooldowns = append(x.data.Cooldowns, cooldownEvent{Tick: x.p.Tick, Player: id, Kind: kind, Slot: int8(slot)})
	}
	*prevEnd = 0
}

func (x *extractor) abilityID(name string) int16 {
	if id, ok := x.abilityIDs[name]; ok {
		return id
	}
	id := int16(len(x.data.AbilityNames))
	x.data.AbilityNames = append(x.data.AbilityNames, name)
	x.abilityIDs[name] = id
	return id
}
