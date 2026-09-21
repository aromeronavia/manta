package matchviewer

import "time"

// Dota 2 world coordinates: an entity's position is cell*cellSize + offset,
// and the playable map is centred on mapCenter in both axes. A minimap image
// spans mapDef.Size world units, centred on mapCenter; the viewer projects
// positions with (x - mapCenter) / size.
const (
	cellSize  = 128
	mapCenter = 16384
)

// mapDef describes a minimap image edition. Sizes and dates follow the
// community definitions used by ReDota (timkurvers/redota).
type mapDef struct {
	ID    string  `json:"id"`
	Size  float64 `json:"size"`
	Image string  `json:"image,omitempty"`
	since time.Time
}

// mapDefs lists known map editions, newest first.
var mapDefs = []mapDef{
	{ID: "7.40", Size: 19134, since: time.Date(2025, 12, 16, 0, 0, 0, 0, time.UTC)},
	{ID: "7.38", Size: 19134, since: time.Date(2025, 2, 19, 0, 0, 0, 0, time.UTC)},
	{ID: "7.33", Size: 19134, since: time.Date(2023, 4, 21, 0, 0, 0, 0, time.UTC)},
	{ID: "7.29", Size: 16384, since: time.Date(2021, 4, 10, 0, 0, 0, 0, time.UTC)},
	{ID: "7.23", Size: 16384, since: time.Date(2019, 11, 26, 0, 0, 0, 0, time.UTC)},
}

// mapForTime returns the newest map edition released at or before t, falling
// back to the oldest known edition for matches that predate them all.
func mapForTime(t time.Time) mapDef {
	for _, m := range mapDefs {
		if !t.Before(m.since) {
			return m
		}
	}
	return mapDefs[len(mapDefs)-1]
}

// mapByID returns the map edition with the given id, if known.
func mapByID(id string) (mapDef, bool) {
	for _, m := range mapDefs {
		if m.ID == id {
			return m, true
		}
	}
	return mapDef{}, false
}
