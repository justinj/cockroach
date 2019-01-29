package querygraph

import (
	"bytes"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/util"
)

// TODO(justin): handle n-ary predicates.

type RelID int

type relation struct {
	id   RelID
	name string
	card int
}

type RelSet = util.FastIntSet

type predicate struct {
	first       RelID
	second      RelID
	selectivity float64
}

type underGraph struct {
	numRels int
	rels    map[RelID]relation

	// TODO(justin): represent this with a matrix.
	predicates []predicate

	meta map[RelID]interface{}
}

type Graph struct {
	under *underGraph

	rels RelSet
}

func (g *Graph) AddMeta(r int, d interface{}) {
	g.under.meta[RelID(r)] = d
}

func (g *Graph) GetMeta(r int) interface{} {
	return g.under.meta[RelID(r)]
}

func New() *Graph {
	return &Graph{
		under: &underGraph{
			rels: make(map[RelID]relation),
			meta: make(map[RelID]interface{}),
		},
	}
}

func (g *Graph) Name(r RelID) string {
	return g.under.rels[r].name
}

func (g *Graph) Card(r RelID) int {
	return g.under.rels[r].card
}

func (g *Graph) AddRel(id int, name string, card int) RelID {
	for _, r := range g.under.rels {
		if int(r.id) == id {
			return r.id
		}
	}
	g.under.numRels++

	g.under.rels[RelID(id)] = relation{
		name: name,
		card: card,
	}

	g.rels.Add(id)

	return RelID(id)
}

func (g *Graph) AddPred(r1, r2 RelID, selectivity float64) {
	if r1 == r2 {
		panic("relation can't have predicate with itself")
	}

	if r1 > r2 {
		r1, r2 = r2, r1
	}

	for i := range g.under.predicates {
		p := &g.under.predicates[i]
		if p.first == r1 && p.second == r2 {
			p.selectivity *= selectivity
			return
		}
	}

	g.under.predicates = append(g.under.predicates, predicate{r1, r2, selectivity})
}

func (g *Graph) Union(g2 *Graph) *Graph {
	newG := *g
	newG.rels = newG.rels.Copy()
	for i, ok := g2.rels.Next(0); ok; i, ok = g2.rels.Next(i + 1) {
		r := RelID(i)
		newG.AddRel(i, g2.Name(r), g2.Card(r))
	}

	newG.under.predicates = append(newG.under.predicates, g2.under.predicates...)
	for k, v := range g2.under.meta {
		newG.AddMeta(int(k), v)
	}

	return &newG
}

func (g *Graph) Sel(r1, r2 RelID) (float64, bool) {
	var res float64 = 1
	if r1 > r2 {
		r1, r2 = r2, r1
	}

	seen := false
	for _, p := range g.under.predicates {
		if p.first == r1 && p.second == r2 {
			seen = true
			res *= p.selectivity
		}
	}

	return res, seen
}

func (g *Graph) Rels() RelSet {
	return g.rels
}

func (g *Graph) Restrict(r RelSet) *Graph {
	return &Graph{
		under: g.under,
		rels:  r,
	}
}

func (g *Graph) Dot() string {
	var buf bytes.Buffer
	buf.WriteString("graph G {\n")
	for i, ok := g.rels.Next(0); ok; i, ok = g.rels.Next(i + 1) {
		fmt.Fprintf(&buf, "  %s%d;\n", g.Name(RelID(i)), i)
	}
	for _, p := range g.under.predicates {
		if !g.rels.Contains(int(p.first)) || !g.rels.Contains(int(p.second)) {
			continue
		}
		fmt.Fprintf(&buf, "  %s%d -- %s%d;\n", g.Name(p.first), p.first, g.Name(p.second), p.second)
	}
	buf.WriteString("}")

	return buf.String()
}

// Neighbours returns the relations which share a predicate with rel.
func (g *Graph) Neighbours(rel RelID) RelSet {
	var result RelSet
	for i, ok := g.rels.Next(0); ok; i, ok = g.rels.Next(i + 1) {
		r := RelID(i)
		if r == rel {
			continue
		}
		if _, ok := g.Sel(rel, r); ok {
			result.Add(i)
		}
	}
	return result
}

func (g *Graph) dfs(from RelID, visited RelSet) RelID {
	n := g.Neighbours(from)
	n.DifferenceWith(visited)
	next, ok := n.Next(0)
	if !ok {
		return from
	}
	visited.Add(int(from))
	return g.dfs(RelID(next), visited)
}

// pickRel returns a vertex whose deletion does not disconnect G.
func (g *Graph) pickRel() RelID {
	start, ok := g.rels.Next(0)
	if !ok {
		panic("can't pickRel an empty graph")
	}
	return g.dfs(RelID(start), RelSet{})
}

func (g *Graph) Cut() (g1, g2 *Graph) {
	// A simple implementation of finding a minimal cut:
	// pick a vertex whose deletion does not disconnect the graph.
	r := g.pickRel()
	s := util.MakeFastIntSet(int(r))
	return g.Restrict(s), g.Restrict(g.rels.Difference(s))
}
