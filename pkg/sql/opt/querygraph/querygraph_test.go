package querygraph

import (
	"testing"

	"github.com/cockroachdb/cockroach/pkg/util"
)

func TestBasic(t *testing.T) {
	g := New()

	a := g.AddRel(1, "a", 100)

	if g.Card(a) != 100 {
		t.Fatal("expected a to have cardinality 100")
	}

	b := g.AddRel(2, "b", 100)
	g.AddPred(a, b, 0.5)

	if sel, _ := g.Sel(a, b); sel != 0.5 {
		t.Fatal("expected a and b to have selectivity 0.5")
	}

	c := g.AddRel(3, "c", 1000)

	g.AddPred(a, c, 0.5)
	g.AddPred(a, c, 0.5)

	if sel, _ := g.Sel(a, c); sel != 0.25 {
		t.Fatalf("expected a and c to have selectivity 0.25")
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

	n := g.Neighbours(a)
	if !n.Equals(util.MakeFastIntSet(int(b), int(c))) {
		t.Fatalf("expected %v, got %v", util.MakeFastIntSet(int(b), int(c)), n)
	}

	n = g2.Neighbours(a)
	if !n.Equals(util.MakeFastIntSet(int(c))) {
		t.Fatalf("expected %v, got %v", util.MakeFastIntSet(int(c)), n)
	}
}

func TestMerge(t *testing.T) {
	g1 := New()
	g1.AddRel(1, "a", 1000)
	g1.AddRel(2, "b", 1000)
	g1.AddRel(3, "c", 1000)

	g1.AddPred(1, 2, 0.5)
	g1.AddPred(1, 3, 0.5)

	g2 := New()
	g2.AddRel(4, "x", 1000)
	g2.AddRel(5, "y", 1000)
	g2.AddRel(6, "z", 1000)

	g2.AddPred(4, 5, 0.5)
	g2.AddPred(4, 6, 0.5)

	g3 := g1.Union(g2)

	if !g3.Rels().Equals(util.MakeFastIntSet(1, 2, 3, 4, 5, 6)) {
		t.Fatal("rel set should have been 1, 2, 3, 4, 5, 6")
	}

	g3.AddPred(2, 6, 0.5)

	dot := g3.Dot()
	expected := `graph G {
  a -- b;
  a -- c;
  x -- y;
  x -- z;
  b -- z;
}`
	if dot != expected {
		t.Fatalf("expected %q, got %q", expected, dot)
	}
}

func TestThreeChain(t *testing.T) {
	g := New()

	a := g.AddRel(1, "a", 1)

	b := g.AddRel(2, "b", 10000)
	g.AddPred(a, b, 0.0001)

	c := g.AddRel(3, "c", 10000)
	g.AddPred(a, c, 0.0001)

	_, _ = g.Cut()
}
