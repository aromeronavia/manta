package matchviewer

import (
	"math"
	"strings"
	"testing"

	"github.com/dotabuff/manta"
)

func TestAbilityShown(t *testing.T) {
	cases := []struct {
		name   string
		hidden bool
		want   bool
	}{
		{"lion_impale", false, true},
		{"windrunner_focusfire", false, true},
		{"windrunner_tailwind", true, false},
		{"special_bonus_unique_windranger_4", false, false},
		{"special_bonus_attributes", true, false},
		{"attribute_bonus", false, false},
		{"generic_hidden", false, false},
		{"ability_capture", false, false},
		{"plus_high_five", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		if got := abilityShown(c.name, c.hidden); got != c.want {
			t.Errorf("abilityShown(%q, hidden=%v) = %v, want %v", c.name, c.hidden, got, c.want)
		}
	}
}

// Modern replays network the remaining cooldown; older ones the game time at
// which it ends. Either way the result is seconds left, never negative.
func TestCooldownRemaining(t *testing.T) {
	cases := []struct {
		cooldown, length, gameTime, want float64
	}{
		{5.9, 15, 0, 5.9},                // remaining, game time not networked
		{27.1, 40, 1333, 27.1},           // remaining even when game time is known
		{1517.02, 159.97, 1487.62, 29.4}, // absolute end in the future
		{1475.16, 11.97, 1487.62, 0},     // absolute end already passed
		{0, 12, 1487.62, 0},              // off cooldown
		{0, 0, 0, 0},
	}
	for _, c := range cases {
		if got := cooldownRemaining(c.cooldown, c.length, c.gameTime); math.Abs(got-c.want) > 0.01 {
			t.Errorf("cooldownRemaining(%v, %v, %v) = %v, want %v", c.cooldown, c.length, c.gameTime, got, c.want)
		}
	}
}

// sampleClock is the clock of the last sample at or before tick.
func sampleClock(d *MatchData, tick uint32) float32 {
	i := 0
	for i < len(d.Ticks)-1 && d.Ticks[i+1] <= tick {
		i++
	}
	return d.Clock[i]
}

func TestExtractAbilities(t *testing.T) {
	d := loadFixture(t)
	n := len(d.Ticks)
	if d.Format != FormatVersion {
		t.Errorf("format = %d, want %d", d.Format, FormatVersion)
	}
	if len(d.AbilityNames) == 0 {
		t.Fatal("no ability names")
	}

	byPlayer := map[int32][]abilityEvent{}
	lastTick := map[int32]uint32{}
	for _, e := range d.Abilities {
		if findPlayer(d, e.Player) == nil {
			t.Errorf("ability event for unknown player %d", e.Player)
			continue
		}
		if e.Tick < lastTick[e.Player] {
			t.Errorf("ability events for player %d out of tick order at %d", e.Player, e.Tick)
		}
		lastTick[e.Player] = e.Tick
		if e.Name != absent {
			if int(e.Name) >= len(d.AbilityNames) {
				t.Fatalf("ability name index %d out of range", e.Name)
			}
			name := d.AbilityNames[e.Name]
			if name == "" || strings.HasPrefix(name, "special_bonus_") || name == "attribute_bonus" {
				t.Errorf("ability event carries a name the card should not show: %q", name)
			}
		}
		if e.Level < 0 || e.Level > 30 {
			t.Errorf("ability level %d out of range", e.Level)
		}
		byPlayer[e.Player] = append(byPlayer[e.Player], e)
	}
	for _, p := range d.Players {
		evs := byPlayer[p.ID]
		if len(evs) == 0 {
			t.Errorf("player %d (%s) has no ability events", p.ID, p.Hero)
			continue
		}
		names := map[int16]bool{}
		maxLevel := int8(0)
		for _, e := range evs {
			if e.Name != absent {
				names[e.Name] = true
			}
			if e.Level > maxLevel {
				maxLevel = e.Level
			}
		}
		if len(names) < 4 {
			t.Errorf("player %d (%s) shows only %d distinct abilities", p.ID, p.Hero, len(names))
		}
		if maxLevel < 2 {
			t.Errorf("player %d (%s) never levelled an ability past 1", p.ID, p.Hero)
		}
	}

	kinds := map[string]int{}
	for _, c := range d.Cooldowns {
		kinds[c.Kind]++
		if findPlayer(d, c.Player) == nil {
			t.Errorf("cooldown for unknown player %d", c.Player)
		}
		if c.Kind != "ability" && c.Kind != "item" {
			t.Errorf("cooldown kind %q", c.Kind)
		}
		if c.Slot < 0 || (c.Kind == "item" && int(c.Slot) >= len(itemSlots)) {
			t.Errorf("cooldown slot %d out of range for %s", c.Slot, c.Kind)
		}
		clock := sampleClock(d, c.Tick)
		if clock == clockUnknown || c.End == clockUnknown {
			continue
		}
		remaining := float64(c.End - clock)
		if remaining < -0.5 || remaining > float64(c.Len)+1.5 {
			t.Errorf("cooldown at tick %d ends %.1fs after the sample but lasts %.1fs", c.Tick, remaining, c.Len)
		}
		if c.Len <= 0 && remaining > 0.5 {
			t.Errorf("cooldown at tick %d has no length but %.1fs remaining", c.Tick, remaining)
		}
	}
	if kinds["ability"] == 0 || kinds["item"] == 0 {
		t.Errorf("cooldown kinds: %v", kinds)
	}
	// One event per cast, not one per sample while on cooldown.
	if len(d.Cooldowns) > n*len(d.Players)/4 {
		t.Errorf("%d cooldown events for %d samples looks like dense sampling", len(d.Cooldowns), n)
	}

	positive := 0
	for _, c := range d.Charges {
		if findPlayer(d, c.Player) == nil || c.Slot < 0 || int(c.Slot) >= len(itemSlots) {
			t.Errorf("bad charge event %+v", c)
		}
		if c.Charges < absent {
			t.Errorf("charge event with %d charges", c.Charges)
		}
		if c.Charges > 0 {
			positive++
		}
	}
	if positive == 0 {
		t.Error("no item ever showed charges")
	}
}

// Cooldown events are written once per cast, plus a reset whenever a slot's
// recorded cooldown stops applying before it has run out.
func TestRecordCooldown(t *testing.T) {
	x := &extractor{p: &manta.Parser{}, data: &MatchData{}}
	x.p.Tick = 300
	var prev float64
	record := func(remaining, length, now float64, fresh bool) int {
		x.recordCooldown(1, "item", 0, remaining, length, &prev, now, fresh)
		return len(x.data.Cooldowns)
	}
	if n := record(12, 12, 10, false); n != 1 {
		t.Fatalf("a cast should record one event, got %d", n)
	}
	if n := record(11, 12, 11, false); n != 1 {
		t.Errorf("a running cooldown recorded again: %d events", n)
	}
	// The item is swapped for one without a cooldown while the old one was
	// still cooling: the viewer must be told to stop showing it.
	if n := record(0, 0, 12, true); n != 2 {
		t.Fatalf("swapping out a cooling item should record a reset, got %d events", n)
	}
	if last := x.data.Cooldowns[1]; last.Len != 0 || last.remaining != 0 {
		t.Errorf("reset event = %+v, want zero length and remaining", last)
	}
	// A new item arrives carrying a cooldown that matches the one just reset
	// (a rebought scroll on the shared teleport cooldown): recorded again.
	if n := record(9, 0, 13, true); n != 3 {
		t.Errorf("a fresh item's cooldown should be recorded even when it matches the reset one, got %d", n)
	}
	// Running out naturally needs no event.
	if n := record(0, 0, 23, false); n != 3 {
		t.Errorf("an expired cooldown recorded an event: %d", n)
	}
}
