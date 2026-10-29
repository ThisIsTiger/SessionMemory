package session

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"sort"
	"testing"
)

// Compare the graph to an independently constructed parent/child model, with
// randomized delivery order, missing ancestors and multiple forks.
func TestGraphAgainstRandomForests(t *testing.T) {
	for seed := uint64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewPCG(seed, 29))
		var revs []Revision
		parents := map[string]string{}
		for i := 0; i < 75; i++ {
			id, parent := fmt.Sprintf("r%d", i), ""
			if i > 0 && rng.IntN(5) != 0 {
				parent = fmt.Sprintf("r%d", rng.IntN(i))
			}
			parents[id] = parent
			if rng.IntN(5) != 0 {
				revs = append(revs, Revision{RevisionID: id, ParentRevisionID: parent})
			}
		}
		rng.Shuffle(len(revs), func(i, j int) { revs[i], revs[j] = revs[j], revs[i] })
		known, joined := map[string]bool{}, map[string]bool{}
		for _, r := range revs {
			known[r.RevisionID] = true
		}
		for id := range known {
			ok := true
			for p := id; p != ""; p = parents[p] {
				ok = ok && known[p]
			}
			joined[id] = ok
		}
		var heads, pending []string
		for id, ok := range joined {
			if !ok {
				pending = append(pending, id)
				continue
			}
			hasChild := false
			for child, present := range joined {
				hasChild = hasChild || (present && parents[child] == id)
			}
			if !hasChild {
				heads = append(heads, id)
			}
		}
		sort.Strings(heads)
		sort.Strings(pending)
		g := NewGraph(revs)
		if !reflect.DeepEqual(g.Heads(), heads) || !reflect.DeepEqual(g.Pending, pending) {
			t.Fatalf("seed %d: heads=%v want=%v pending=%v want=%v", seed, g.Heads(), heads, g.Pending, pending)
		}
		res := Resolve(g, nil)
		if res.Conflict != (len(heads) > 1) || (len(heads) == 1 && res.Effective != heads[0]) {
			t.Fatalf("seed %d: resolution %+v", seed, res)
		}
		if len(heads) > 1 {
			chosen := heads[rng.IntN(len(heads))]
			res = Resolve(g, []Choice{{Chosen: chosen, SeenHeads: heads}})
			if res.Conflict || res.Effective != chosen {
				t.Fatalf("seed %d: choice did not resolve all known heads: %+v", seed, res)
			}
		}
		for ancestor, aPresent := range joined {
			for child, cPresent := range joined {
				want := false
				if aPresent && cPresent {
					for p := child; p != ""; p = parents[p] {
						want = want || p == ancestor
					}
				}
				if g.IsAncestorOrEqual(ancestor, child) != want {
					t.Fatalf("seed %d: incorrect ancestry %s -> %s", seed, ancestor, child)
				}
			}
		}
	}
}

func BenchmarkGraphLinearHistory(b *testing.B) {
	for _, size := range []int{100, 1000, 4000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			revs := make([]Revision, size)
			for i := range revs {
				revs[i].RevisionID = fmt.Sprintf("r%d", i)
				if i > 0 {
					revs[i].ParentRevisionID = revs[i-1].RevisionID
				}
			}
			b.ResetTimer()
			for b.Loop() {
				g := NewGraph(revs)
				if !g.Has(revs[len(revs)-1].RevisionID) {
					b.Fatal("lost the tip of the history")
				}
			}
		})
	}
}
