package main

import (
	"bytes"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/util"
)

// TODO(justin): handle n-ary predicates.

type relation struct {
	name string
	card int
}

type RelID int

type RelSet = util.FastIntSet

type predicate struct {
	first       RelID
	second      RelID
	selectivity float64
}

type underGraph struct {
	numRels int
	rels    []relation

	// TODO(justin): represent this with a matrix.
	predicates []predicate
}

type Graph struct {
	under *underGraph

	rels RelSet
}

func New() *Graph {
	return &Graph{
		under: &underGraph{},
	}
}

func (g *Graph) Name(r RelID) string {
	return g.under.rels[int(r-1)].name
}

func (g *Graph) AddRel(name string, card int) RelID {
	id := g.under.numRels + 1
	g.under.numRels++

	g.under.rels = append(g.under.rels, relation{
		name: name,
		card: card,
	})

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

func (g *Graph) Sel(r1, r2 RelID) float64 {
	var res float64 = 1
	if r1 > r2 {
		r1, r2 = r2, r1
	}

	for _, p := range g.under.predicates {
		if p.first == r1 && p.second == r2 {
			res *= p.selectivity
		}
	}

	return res
}

func (g *Graph) Card(r RelID) int {
	return g.under.rels[r-1].card
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
	for _, p := range g.under.predicates {
		if !g.rels.Contains(int(p.first)) || !g.rels.Contains(int(p.second)) {
			continue
		}
		fmt.Fprintf(&buf, "  %s -- %s;\n", g.Name(p.first), g.Name(p.second))
	}
	buf.WriteString("}")

	return buf.String()
}

func (g *Graph) Cut() (g1, g2 *Graph) {
	return nil, nil
}
