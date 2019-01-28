package main

import (
	"testing"

	"github.com/cockroachdb/cockroach/pkg/util"
)

func TestBasic(t *testing.T) {
	g := New()

	a := g.AddRel("a", 100)

	if g.Card(a) != 100 {
		t.Fatal("expected a to have cardinality 100")
	}

	b := g.AddRel("b", 100)
	g.AddPred(a, b, 0.5)

	if g.Sel(a, b) != 0.5 {
		t.Fatal("expected a and b to have selectivity 0.5")
	}

	c := g.AddRel("c", 1000)

	g.AddPred(a, c, 0.5)
	g.AddPred(a, c, 0.5)

	if g.Sel(a, c) != 0.25 {
		t.Fatalf("expected a and c to have selectivity 0.25, had %v", g.Sel(a, c))
	}

	exp := util.MakeFastIntSet(int(a), int(b), int(c))
	if !g.Rels().Equals(exp) {
		t.Fatalf("expected set to be %s, was %s", exp, g.Rels())
	}

	dot := g.Dot()
	expected := `graph G {
  a -- b;
  a -- c;
}`
	if dot != expected {
		t.Fatalf("expected %q, got %q", expected, dot)
	}

	g2 := g.Restrict(util.MakeFastIntSet(int(a), int(c)))
	exp = util.MakeFastIntSet(int(a), int(c))
	if !g2.Rels().Equals(exp) {
		t.Fatalf("expected set to be %s, was %s", exp, g.Rels())
	}

	dot = g2.Dot()
	expected = `graph G {
  a -- c;
}`
	if dot != expected {
		t.Fatalf("expected %q, got %q", expected, dot)
	}
}

func TestThreeChain(t *testing.T) {
	g := New()

	a := g.AddRel("a", 1)

	b := g.AddRel("b", 10000)
	g.AddPred(a, b, 0.0001)

	c := g.AddRel("c", 10000)
	g.AddPred(a, c, 0.0001)

	left, right := g.Cut()

	_ = left
	_ = right
}
