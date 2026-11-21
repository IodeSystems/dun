package procps

import "sort"

// Order is a frozen order of groups and of the rows in them: a list a
// person is moving through must not re-sort under the cursor. Values keep
// updating; Extend puts new processes at the end and drops exited ones.
type Order struct {
	grp  map[string]int
	row  map[int]int
	next int
}

// FreezeOrder takes gs's order as it is.
func FreezeOrder(gs []Group) *Order {
	o := &Order{grp: map[string]int{}, row: map[int]int{}}
	o.Extend(gs)
	return o
}

// Extend ranks what gs has that o does not (after everything ranked, in gs's
// order) and forgets what gs no longer has.
func (o *Order) Extend(gs []Group) {
	seenG, seenP := map[string]bool{}, map[int]bool{}
	for _, g := range gs {
		seenG[g.Group] = true
		if _, ok := o.grp[g.Group]; !ok {
			o.grp[g.Group] = o.next
			o.next++
		}
		for _, r := range g.Rows {
			seenP[r.PID] = true
			if _, ok := o.row[r.PID]; !ok {
				o.row[r.PID] = o.next
				o.next++
			}
		}
	}
	for k := range o.grp {
		if !seenG[k] {
			delete(o.grp, k)
		}
	}
	for k := range o.row {
		if !seenP[k] {
			delete(o.row, k)
		}
	}
}

// Apply is a copy of gs in o's order. gs should have been Extended.
func (o *Order) Apply(gs []Group) []Group {
	out := make([]Group, len(gs))
	copy(out, gs)
	for i := range out {
		rows := append([]Row(nil), out[i].Rows...)
		sort.SliceStable(rows, func(a, b int) bool { return o.row[rows[a].PID] < o.row[rows[b].PID] })
		out[i].Rows = rows
	}
	sort.SliceStable(out, func(a, b int) bool { return o.grp[out[a].Group] < o.grp[out[b].Group] })
	return out
}
