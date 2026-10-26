package session

import "sort"

// Choice is a user's continuation choice (design §9.2): which head to keep,
// and which heads they had seen when deciding. It never merges content.
type Choice struct {
	SessionID  string   `json:"session_id"`
	SeenHeads  []string `json:"seen_heads"` // sorted revision IDs, includes Chosen
	Chosen     string   `json:"chosen"`
	DeviceID   string   `json:"device_id"`
	At         string   `json:"at"`
	ManifestID string   `json:"-"` // provenance only, not part of the payload
}

// Graph is the revision DAG (a forest of single-parent chains) of one
// session. Revisions whose parent is unknown are kept aside as Pending until
// the parent arrives; they take no part in heads or ancestry.
type Graph struct {
	parent   map[string]string
	children map[string][]string
	Pending  []string
}

// NewGraph builds a graph from the validated revisions of one session.
func NewGraph(revs []Revision) *Graph {
	g := &Graph{parent: map[string]string{}, children: map[string][]string{}}
	known := map[string]bool{}
	for _, r := range revs {
		known[r.RevisionID] = true
	}
	// a revision joins only if its whole parent chain is known
	var join func(id string, seen map[string]bool) bool
	byID := map[string]Revision{}
	for _, r := range revs {
		byID[r.RevisionID] = r
	}
	join = func(id string, seen map[string]bool) bool {
		if seen[id] {
			return false // cycle: cannot happen with content-addressed IDs, but never loop
		}
		seen[id] = true
		p := byID[id].ParentRevisionID
		return p == "" || (known[p] && join(p, seen))
	}
	for _, r := range revs {
		if !join(r.RevisionID, map[string]bool{}) {
			g.Pending = append(g.Pending, r.RevisionID)
			continue
		}
		g.parent[r.RevisionID] = r.ParentRevisionID
	}
	for id, p := range g.parent {
		if p != "" {
			g.children[p] = append(g.children[p], id)
		}
	}
	sort.Strings(g.Pending)
	return g
}

// Has reports whether id is part of the graph.
func (g *Graph) Has(id string) bool { _, ok := g.parent[id]; return ok }

// Heads are the revisions without children, sorted.
func (g *Graph) Heads() []string {
	var out []string
	for id := range g.parent {
		if len(g.children[id]) == 0 {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// IsAncestorOrEqual reports whether a is b or one of b's ancestors.
func (g *Graph) IsAncestorOrEqual(a, b string) bool {
	if !g.Has(a) || !g.Has(b) {
		return false
	}
	for id := b; id != ""; id = g.parent[id] {
		if id == a {
			return true
		}
	}
	return false
}

// Resolution is the outcome of applying choices to the heads.
type Resolution struct {
	Heads     []string // all current heads
	Conflict  bool     // more than one live head and no covering choice
	Effective string   // the head to continue from; "" while Conflict
}

// Resolve decides whether the session is in conflict (design §9.2, A5/A6).
//
// A choice whose revisions are not all in the graph is ignored for now.
// Choices are consistent when their Chosen revisions lie on one ancestor
// chain; the deepest one is the effective choice C. Heads that some choice
// saw and did not choose are retired. The session is resolved when exactly
// one live head remains and it is C or a descendant of C.
func Resolve(g *Graph, choices []Choice) Resolution {
	res := Resolution{Heads: g.Heads()}
	switch len(res.Heads) {
	case 0:
		return res
	case 1:
		res.Effective = res.Heads[0]
		return res
	}

	var usable []Choice
	for _, c := range choices {
		ok := g.Has(c.Chosen)
		for _, h := range c.SeenHeads {
			ok = ok && g.Has(h)
		}
		if ok {
			usable = append(usable, c)
		}
	}
	if len(usable) == 0 {
		res.Conflict = true
		return res
	}

	deepest := usable[0].Chosen
	for _, c := range usable[1:] {
		switch {
		case g.IsAncestorOrEqual(deepest, c.Chosen):
			deepest = c.Chosen
		case g.IsAncestorOrEqual(c.Chosen, deepest):
		default: // two choices on different branches: inconsistent
			res.Conflict = true
			return res
		}
	}

	retired := map[string]bool{}
	for _, c := range usable {
		for _, h := range c.SeenHeads {
			if h != c.Chosen {
				retired[h] = true
			}
		}
	}
	var live []string
	for _, h := range res.Heads {
		if !retired[h] {
			live = append(live, h)
		}
	}
	if len(live) == 1 && g.IsAncestorOrEqual(deepest, live[0]) {
		res.Effective = live[0]
		return res
	}
	res.Conflict = true
	return res
}

// Landing says what a device holding `installed` should do about the
// effective head (design §9.2 item 6, §9.3).
type Landing int

const (
	// LandUpToDate: installed is the effective head.
	LandUpToDate Landing = iota
	// LandFastForward: installed is an ancestor; update in place.
	LandFastForward
	// LandNeedsAction: installed is not an ancestor (a rejected branch or an
	// unknown revision). Never automatic: needs the user's confirmation.
	LandNeedsAction
)

func (l Landing) String() string {
	return [...]string{"up_to_date", "fast_forward", "needs_action"}[l]
}

// LandingFor computes the landing for a device. installed == "" means nothing
// is installed yet (a first install, not a landing) and returns
// LandFastForward.
func LandingFor(g *Graph, installed, effective string) Landing {
	switch {
	case installed == effective:
		return LandUpToDate
	case installed == "":
		return LandFastForward
	case g.IsAncestorOrEqual(installed, effective):
		return LandFastForward
	}
	return LandNeedsAction
}
