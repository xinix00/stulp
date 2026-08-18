package main

import (
	"fmt"

	mattercontroller "github.com/xinix00/stulp/plugins/matter/internal/controller"
)

// What a link means is decided here rather than in the browser: which two
// reports are the same link, whether both ends confirmed it, and how good it
// is. Those are judgements about data and belong under ordinary Go tests.
//
// Where a node ends up on screen is not decided here at all. The graph library
// in the browser lays out, rescales and lets the user drag; that is a drawing
// problem and it is better at it.

// DrawnLink is one link with its presentation already decided: a stable
// identity so the page can replace rather than merge, and a grade and weight
// so it does not have to know what an LQI is.
type DrawnLink struct {
	ID string `json:"id"`
	mattercontroller.MeshLink
	Grade  string  `json:"grade"`
	Weight float64 `json:"weight"`
}

// linkGrade classifies a radio link by its LQI, which runs 0 to 255.
func linkGrade(link mattercontroller.MeshLink) string {
	if link.Kind == "border" {
		return "border"
	}
	if link.LQI == nil {
		return "unknown"
	}
	switch {
	case *link.LQI >= 150:
		return "strong"
	case *link.LQI >= 80:
		return "fair"
	default:
		return "weak"
	}
}

func linkWeight(link mattercontroller.MeshLink) float64 {
	if link.Kind == "border" {
		return 1.4
	}
	if link.LQI == nil {
		return 1.5
	}
	return 1.5 + (float64(*link.LQI)/255.0)*3.0
}

// linkID identifies a link by the pair it connects, not by who reported it.
// Both ends report the same radio link, so this is what lets the second report
// confirm the first instead of drawing a duplicate.
func linkID(link mattercontroller.MeshLink) string {
	if link.To == "" {
		return fmt.Sprintf("%s|%s|%s", link.Kind, link.From, link.ToExtAddress)
	}
	first, second := link.From, link.To
	if first > second {
		first, second = second, first
	}
	return fmt.Sprintf("%s|%s|%s", link.Kind, first, second)
}

// drawLinks decides presentation for every link and drops the duplicate report
// of a pair, marking the survivor as confirmed by both ends.
func drawLinks(links []mattercontroller.MeshLink) []DrawnLink {
	byID := make(map[string]*DrawnLink, len(links))
	order := make([]string, 0, len(links))
	for _, link := range links {
		id := linkID(link)
		existing := byID[id]
		if existing == nil {
			drawn := DrawnLink{ID: id, MeshLink: link, Grade: linkGrade(link), Weight: linkWeight(link)}
			byID[id] = &drawn
			order = append(order, id)
			continue
		}
		// A second report from the other end is the confirmation.
		if existing.From != link.From {
			existing.Mutual = true
		}
		// Keep a measurement over a missing one.
		if existing.LQI == nil && link.LQI != nil {
			existing.LQI = link.LQI
			existing.Grade, existing.Weight = linkGrade(existing.MeshLink), linkWeight(existing.MeshLink)
		}
		if existing.RSSI == nil && link.RSSI != nil {
			existing.RSSI = link.RSSI
		}
	}
	result := make([]DrawnLink, 0, len(order))
	for _, id := range order {
		result = append(result, *byID[id])
	}
	return result
}
