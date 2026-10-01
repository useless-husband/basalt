package catalog

import (
	"testing"

	"github.com/useless-husband/basalt/internal/types"
)

func sample() *Catalog {
	c := New()
	t := &Table{Oid: 16384, Name: "orders", Columns: []*Column{{Name: "id", Type: types.Int4, NotNull: true}, {Name: "note", Type: types.VarcharN(10)}},
		Indexes: []uint32{16385, 16386}, Checks: []*Check{{Name: "c1", Expr: "id > 0"}},
		Stats: &TableStats{Rows: 10, Pages: 1, Columns: []ColumnStats{{NDistinct: 10, Histogram: []string{"1", "5", "10"}}, {NDistinct: 2, MCV: []string{"a"}, MCVFreq: []float64{0.5}}}}}
	c.Tables[t.Oid] = t
	c.Indexes[16385] = &Index{Oid: 16385, Name: "orders_pkey", Table: t.Oid, Columns: []int{0}, Unique: true, Primary: true, Constraint: true}
	c.Indexes[16386] = &Index{Oid: 16386, Name: "Orders Note", Table: t.Oid, Columns: []int{1}}
	return c
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	c := sample()
	b, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	d, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	tb := d.TableByName("orders")
	if tb == nil || tb.Columns[1].Type != types.VarcharN(10) || len(tb.Checks) != 1 {
		t.Fatalf("table lost: %+v", tb)
	}
	cs := tb.Stats.Columns[0]
	if len(cs.HistVals) != 3 || cs.HistVals[2].I != 10 {
		t.Fatalf("histogram not parsed: %+v", cs)
	}
	if len(tb.Stats.Columns[1].MCVVals) != 1 || tb.Stats.Columns[1].MCVVals[0].S != "a" {
		t.Fatalf("MCV not parsed")
	}
	// Clone is deep.
	cl := d.Clone()
	cl.TableByName("orders").Columns[0].Name = "changed"
	if d.TableByName("orders").Columns[0].Name != "id" {
		t.Fatal("Clone shares columns")
	}
}

func TestLookupAndIndexes(t *testing.T) {
	c := sample()
	if oid, kind, ok := c.Lookup("orders_pkey"); !ok || kind != KindIndex || oid != 16385 {
		t.Fatalf("lookup index: %d %c %v", oid, kind, ok)
	}
	ixs := c.TableIndexes(c.TableByName("orders"))
	if len(ixs) != 2 || !ixs[0].Primary {
		t.Fatal("primary key should come first")
	}
	if got := ixs[1].Definition(c.TableByName("orders")); got != `CREATE INDEX "Orders Note" ON public.orders USING btree (note)` {
		t.Fatalf("definition %q", got)
	}
}

func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{"abc": "abc", "Abc": `"Abc"`, "user": `"user"`, "a b": `"a b"`, `q"t`: `"q""t"`, "_x1": "_x1", "1x": `"1x"`} {
		if got := QuoteIdent(in); got != want {
			t.Errorf("QuoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}
