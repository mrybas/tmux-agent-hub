package state

import "sort"

// nextPriority: which statuses need the user, most urgent first.
var nextPriority = map[Status]int{
	StatusWaitingPermission: 0,
	StatusStuck:             1,
	StatusDone:              2,
	StatusWaitingInput:      3,
}

// ChooseNext picks the agent to jump to: blocked-on-permission first, then
// unseen results, then idle ones; within a status — waiting the longest
// first. Returns nil when nobody needs the user.
func ChooseNext(panes []*Pane) *Pane {
	var candidates []*Pane
	for _, p := range panes {
		if _, ok := nextPriority[p.Display()]; ok {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		pi, pj := nextPriority[candidates[i].Display()], nextPriority[candidates[j].Display()]
		if pi != pj {
			return pi < pj
		}
		return candidates[i].StatusSince.Before(candidates[j].StatusSince)
	})
	return candidates[0]
}
