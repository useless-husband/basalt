// Package catalog holds the schema: tables, columns, indexes, constraints,
// sequences and planner statistics. A Catalog value is immutable once
// published; DDL builds a modified copy and swaps it in, so queries can
// keep using the version they started with.
package catalog

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/useless-husband/basalt/internal/storage"
	"github.com/useless-husband/basalt/internal/types"
)

// Catalog is one version of the schema.
type Catalog struct {
	Version   uint64
	Tables    map[uint32]*Table
	Indexes   map[uint32]*Index
	Sequences map[uint32]*Sequence
}

// Table is a user table.
type Table struct {
	Oid     uint32
	Name    string
	Columns []*Column
	Heap    storage.PageID
	Indexes []uint32 // index OIDs
	Checks  []*Check
	FKs     []*ForeignKey
	Stats   *TableStats `json:",omitempty"`
}

// Column is a table column.
type Column struct {
	Name    string
	Type    types.T
	NotNull bool
	Default string `json:",omitempty"` // SQL expression text
	// Missing is the value of rows written before the column was added
	// (text form), when HasMissing is set.
	Missing    string `json:",omitempty"`
	HasMissing bool   `json:",omitempty"`
	Identity   bool   `json:",omitempty"`
}

// Index is a B+tree index on table columns.
type Index struct {
	Oid        uint32
	Name       string
	Table      uint32
	Columns    []int // positions in Table.Columns
	Unique     bool
	Primary    bool
	Constraint bool // created by a PRIMARY KEY or UNIQUE constraint
	Root       storage.PageID
}

// Check is a CHECK constraint.
type Check struct {
	Name string
	Expr string
}

// ForeignKey is a FOREIGN KEY constraint of the referencing table.
type ForeignKey struct {
	Name       string
	Columns    []int
	RefTable   uint32
	RefColumns []int
	OnDelete   string
	OnUpdate   string
}

// Sequence is a sequence (used by serial columns).
type Sequence struct {
	Oid        uint32
	Name       string
	Page       storage.PageID
	Increment  int64
	Start      int64
	OwnerTable uint32 `json:",omitempty"`
	OwnerCol   int    `json:",omitempty"`
}

// TableStats are gathered by ANALYZE.
type TableStats struct {
	Rows    float64
	Pages   int
	Columns []ColumnStats
}

// ColumnStats describe one column's value distribution. Values are kept in
// text form so the catalog serializes as plain JSON.
type ColumnStats struct {
	NullFrac  float64
	NDistinct float64
	AvgWidth  int
	MCV       []string  `json:",omitempty"`
	MCVFreq   []float64 `json:",omitempty"`
	Histogram []string  `json:",omitempty"`
	// Parsed forms, filled in by Decode.
	MCVVals  []types.Value `json:"-"`
	HistVals []types.Value `json:"-"`
}

// New returns an empty catalog.
func New() *Catalog {
	return &Catalog{Tables: map[uint32]*Table{}, Indexes: map[uint32]*Index{}, Sequences: map[uint32]*Sequence{}}
}

// Encode serializes the catalog.
func (c *Catalog) Encode() ([]byte, error) { return json.Marshal(c) }

// Decode parses a serialized catalog.
func Decode(b []byte) (*Catalog, error) {
	c := New()
	if len(b) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if c.Tables == nil {
		c.Tables = map[uint32]*Table{}
	}
	if c.Indexes == nil {
		c.Indexes = map[uint32]*Index{}
	}
	if c.Sequences == nil {
		c.Sequences = map[uint32]*Sequence{}
	}
	for _, t := range c.Tables {
		t.Stats.parse(t)
	}
	return c, nil
}

func (s *TableStats) parse(t *Table) {
	if s == nil {
		return
	}
	for i := range s.Columns {
		if i >= len(t.Columns) {
			break
		}
		cs := &s.Columns[i]
		typ := t.Columns[i].Type
		cs.MCVVals = cs.MCVVals[:0]
		for _, v := range cs.MCV {
			if pv, err := types.Parse(v, typ); err == nil {
				cs.MCVVals = append(cs.MCVVals, pv)
			}
		}
		if len(cs.MCVVals) != len(cs.MCV) {
			cs.MCVVals, cs.MCV, cs.MCVFreq = nil, nil, nil
		}
		cs.HistVals = cs.HistVals[:0]
		for _, v := range cs.Histogram {
			if pv, err := types.Parse(v, typ); err == nil {
				cs.HistVals = append(cs.HistVals, pv)
			}
		}
	}
}

// Clone returns a deep copy for modification.
func (c *Catalog) Clone() *Catalog {
	b, err := c.Encode()
	if err != nil {
		panic(err)
	}
	n, err := Decode(b)
	if err != nil {
		panic(err)
	}
	n.Version = c.Version
	return n
}

// Relation kinds for name lookup.
const (
	KindTable    = 'r'
	KindIndex    = 'i'
	KindSequence = 'S'
)

// Lookup finds a relation of any kind by name.
func (c *Catalog) Lookup(name string) (oid uint32, kind byte, ok bool) {
	for _, t := range c.Tables {
		if t.Name == name {
			return t.Oid, KindTable, true
		}
	}
	for _, i := range c.Indexes {
		if i.Name == name {
			return i.Oid, KindIndex, true
		}
	}
	for _, s := range c.Sequences {
		if s.Name == name {
			return s.Oid, KindSequence, true
		}
	}
	return 0, 0, false
}

// TableByName returns a table or nil.
func (c *Catalog) TableByName(name string) *Table {
	for _, t := range c.Tables {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// IndexByName returns an index or nil.
func (c *Catalog) IndexByName(name string) *Index {
	for _, i := range c.Indexes {
		if i.Name == name {
			return i
		}
	}
	return nil
}

// SequenceByName returns a sequence or nil.
func (c *Catalog) SequenceByName(name string) *Sequence {
	for _, s := range c.Sequences {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// TableIndexes returns the indexes of a table, primary key first.
func (c *Catalog) TableIndexes(t *Table) []*Index {
	out := make([]*Index, 0, len(t.Indexes))
	for _, oid := range t.Indexes {
		if ix := c.Indexes[oid]; ix != nil {
			out = append(out, ix)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Primary != out[j].Primary {
			return out[i].Primary
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// SortedTables returns tables ordered by name.
func (c *Catalog) SortedTables() []*Table {
	out := make([]*Table, 0, len(c.Tables))
	for _, t := range c.Tables {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Referencing describes a foreign key of another table that points at a
// table.
type Referencing struct {
	Table *Table
	FK    *ForeignKey
}

// ReferencedBy lists the foreign keys that reference table oid.
func (c *Catalog) ReferencedBy(oid uint32) []Referencing {
	var out []Referencing
	for _, t := range c.SortedTables() {
		for _, fk := range t.FKs {
			if fk.RefTable == oid {
				out = append(out, Referencing{t, fk})
			}
		}
	}
	return out
}

// ColumnIndex returns the position of a column by name, or -1.
func (t *Table) ColumnIndex(name string) int {
	for i, c := range t.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// ColumnTypes returns the types of all columns.
func (t *Table) ColumnTypes() []types.T {
	out := make([]types.T, len(t.Columns))
	for i, c := range t.Columns {
		out[i] = c.Type
	}
	return out
}

// PrimaryKey returns the primary key index or nil.
func (c *Catalog) PrimaryKey(t *Table) *Index {
	for _, oid := range t.Indexes {
		if ix := c.Indexes[oid]; ix != nil && ix.Primary {
			return ix
		}
	}
	return nil
}

// ColumnNames returns the column names of an index.
func (ix *Index) ColumnNames(t *Table) []string {
	out := make([]string, len(ix.Columns))
	for i, c := range ix.Columns {
		out[i] = t.Columns[c].Name
	}
	return out
}

// Definition renders CREATE INDEX for pg_get_indexdef.
func (ix *Index) Definition(t *Table) string {
	var b strings.Builder
	b.WriteString("CREATE ")
	if ix.Unique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX ")
	b.WriteString(QuoteIdent(ix.Name))
	b.WriteString(" ON public.")
	b.WriteString(QuoteIdent(t.Name))
	b.WriteString(" USING btree (")
	for i, n := range ix.ColumnNames(t) {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(QuoteIdent(n))
	}
	b.WriteString(")")
	return b.String()
}

// QuoteIdent quotes an identifier if needed.
func QuoteIdent(s string) string {
	plain := s != ""
	for i, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9')) {
			plain = false
			break
		}
	}
	if plain && !reservedWords[s] {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

var reservedWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`all analyse analyze and any array as asc asymmetric both case cast check
		collate column constraint create current_catalog current_date current_role current_time
		current_timestamp current_user default deferrable desc distinct do else end except false fetch
		for foreign from grant group having in initially intersect into lateral leading limit localtime
		localtimestamp not null offset on only or order placing primary references returning select
		session_user some symmetric table then to trailing true union unique user using variadic when
		where window with`) {
		reservedWords[w] = true
	}
}
