package main

import (
	"errors"
	"fmt"
	"sort"

	"github.com/dotabuff/manta"
	"github.com/dotabuff/manta/dota"
)

type fieldValue struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// entityState is an entity's full state as of a tick.
type entityState struct {
	RequestedTick uint32       `json:"requestedTick"`
	StateTick     uint32       `json:"stateTick"`
	Index         int32        `json:"index"`
	Serial        int32        `json:"serial"`
	Class         string       `json:"class"`
	Missing       bool         `json:"missing"`
	Fields        []fieldValue `json:"fields"`
}

var errReachedTick = errors.New("reached target tick")

// entityStateAt re-parses the replay, applying every packet with a tick at or
// below the target, and returns the entity at index as it stands then.
//
// Manta sorts each packet's inner messages so net_Tick is dispatched before
// PacketEntities. Returning an error from the net_Tick callback of the first
// packet past the target therefore aborts that packet before any of its entity
// updates apply, which makes the stop exact. StateTick reports the tick of the
// last packet actually applied.
func entityStateAt(buf []byte, tick uint32, index int32) (*entityState, error) {
	p, err := manta.NewParser(buf)
	if err != nil {
		return nil, err
	}

	var lastApplied uint32
	p.Callbacks.OnCNETMsg_Tick(func(m *dota.CNETMsg_Tick) error {
		if p.Tick > tick {
			return errReachedTick
		}
		return nil
	})
	p.Callbacks.OnCDemoPacket(func(m *dota.CDemoPacket) error {
		lastApplied = p.Tick
		return nil
	})
	p.Callbacks.OnCDemoFullPacket(func(m *dota.CDemoFullPacket) error {
		lastApplied = p.Tick
		return nil
	})

	if err := p.Start(); err != nil && !errors.Is(err, errReachedTick) {
		return nil, err
	}

	st := &entityState{RequestedTick: tick, StateTick: lastApplied, Index: index}
	e := p.FindEntity(index)
	if e == nil {
		st.Missing = true
		return st, nil
	}

	st.Serial = e.GetSerial()
	st.Class = e.GetClassName()
	for name, v := range e.Map() {
		st.Fields = append(st.Fields, fieldValue{Name: name, Type: fmt.Sprintf("%T", v), Value: fmt.Sprint(v)})
	}
	sort.Slice(st.Fields, func(i, j int) bool { return st.Fields[i].Name < st.Fields[j].Name })
	return st, nil
}
