package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/executor"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/planner"
	"github.com/useless-husband/basalt/internal/sql"
	"github.com/useless-husband/basalt/internal/storage"
	"github.com/useless-husband/basalt/internal/txn"
	"github.com/useless-husband/basalt/internal/types"
)

// ddl runs a schema change. DDL is not transactional in basalt: each
// statement commits on its own (it is rejected inside BEGIN ... COMMIT).
func (s *Session) ddl(ctx context.Context, st sql.Stmt) (*Result, error) {
	// A transaction to hold locks and read existing rows.
	t := s.db.txns.Begin()
	t.Ctx = ctx
	defer func() {
		if !t.Done() {
			t.Abort()
		}
	}()
	var res *Result
	var err error
	switch x := st.(type) {
	case *sql.CreateTableStmt:
		res, err = s.createTable(t, x)
	case *sql.CreateIndexStmt:
		res, err = s.createIndex(t, x)
	case *sql.DropStmt:
		res, err = s.drop(t, x)
	case *sql.AlterTableStmt:
		res, err = s.alterTable(t, x)
	case *sql.TruncateStmt:
		res, err = s.truncate(t, x)
	case *sql.CreateSequenceStmt:
		res, err = s.createSequence(t, x)
	default:
		err = pgerr.Internal("unknown DDL %T", st)
	}
	if err != nil {
		return nil, err
	}
	if err := t.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

func relationExists(cat *catalog.Catalog, name string) bool {
	_, _, ok := cat.Lookup(name)
	return ok
}

// uniqueName returns base, or base1, base2, ... if taken.
func uniqueName(cat *catalog.Catalog, base string, taken map[string]bool) string {
	name := base
	for i := 1; relationExists(cat, name) || taken[name]; i++ {
		name = fmt.Sprintf("%s%d", base, i)
	}
	taken[name] = true
	return name
}

func truncName(s string) string {
	if len(s) > 63 {
		return s[:63]
	}
	return s
}

func lockTable(t *txn.Txn, oid uint32, mode txn.Mode) error {
	return t.Lock(txn.Tag{Kind: txn.TagRelation, ID: uint64(oid)}, mode)
}

type pendingIndex struct {
	name    string
	cols    []int
	unique  bool
	primary bool
	conName string
}

func (s *Session) createTable(t *txn.Txn, x *sql.CreateTableStmt) (*Result, error) {
	db := s.db
	if x.Table.Schema != "" && x.Table.Schema != "public" {
		return nil, pgerr.New(pgerr.UndefinedObject, "schema \"%s\" does not exist", x.Table.Schema)
	}
	if x.AsSelect != nil {
		return s.createTableAs(t, x)
	}
	db.ddlMu.Lock()
	defer db.ddlMu.Unlock()
	cur := db.Catalog()
	name := x.Table.Name
	if relationExists(cur, name) {
		if x.IfNotExists {
			s.Notices = append(s.Notices, fmt.Sprintf("relation \"%s\" already exists, skipping", name))
			return &Result{Tag: "CREATE TABLE"}, nil
		}
		return nil, pgerr.New(pgerr.DuplicateTable, "relation \"%s\" already exists", name)
	}
	if len(x.Columns) == 0 {
		return nil, pgerr.New(pgerr.InvalidTableDefinition, "tables must have at least one column")
	}
	if len(x.Columns) > 1600 {
		return nil, pgerr.New(pgerr.ProgramLimitExceeded, "tables can have at most 1600 columns")
	}
	cat := cur.Clone()
	tbl := &catalog.Table{Name: name}
	taken := map[string]bool{name: true}
	var seqs []*catalog.Sequence
	var indexes []pendingIndex
	var checks []*sql.Constraint
	var fks []*sql.Constraint
	for _, cd := range x.Columns {
		if tbl.ColumnIndex(cd.Name) >= 0 {
			return nil, pgerr.New(pgerr.DuplicateColumn, "column \"%s\" specified more than once", cd.Name)
		}
		typ, err := planner.ResolveTypeName(cd.Type)
		if err != nil {
			return nil, err
		}
		col := &catalog.Column{Name: cd.Name, Type: typ, NotNull: cd.NotNull}
		if types.IsSerial(cd.Type.Name) || cd.Identity {
			if cd.Type.Array {
				return nil, pgerr.Unsupported("array of serial is not implemented")
			}
			seqName := uniqueName(cat, truncName(name+"_"+cd.Name+"_seq"), taken)
			seqs = append(seqs, &catalog.Sequence{Name: seqName, Increment: 1, Start: 1, OwnerCol: len(tbl.Columns)})
			col.Default = fmt.Sprintf("nextval('%s'::regclass)", seqName)
			col.NotNull = true
			col.Identity = cd.Identity
		} else if cd.Default != nil {
			col.Default = cd.DefaultSQL
		}
		tbl.Columns = append(tbl.Columns, col)
		for _, c := range cd.Constraints {
			c.Columns = []string{cd.Name}
			switch c.Kind {
			case sql.ConCheck:
				checks = append(checks, c)
			case sql.ConForeignKey:
				fks = append(fks, c)
			default:
				if err := addKeyConstraint(cat, tbl, c, &indexes, taken); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, c := range x.Constraints {
		switch c.Kind {
		case sql.ConCheck:
			checks = append(checks, c)
		case sql.ConForeignKey:
			fks = append(fks, c)
		default:
			if err := addKeyConstraint(cat, tbl, c, &indexes, taken); err != nil {
				return nil, err
			}
		}
	}
	// Validate defaults.
	for _, col := range tbl.Columns {
		if col.Default == "" || strings.HasPrefix(col.Default, "nextval(") {
			continue
		}
		ast, err := sql.ParseExpr(col.Default)
		if err != nil {
			return nil, err
		}
		b := planner.NewBinder(cat, nil, nil)
		if _, err := b.BindExprNoColumns(ast, col.Type); err != nil {
			return nil, err
		}
	}
	for _, c := range checks {
		if err := addCheck(cat, tbl, c); err != nil {
			return nil, err
		}
	}
	for _, c := range fks {
		if err := addForeignKey(cat, tbl, c, indexes); err != nil {
			return nil, err
		}
	}
	// Allocate storage and object ids in the same mini-transaction that
	// writes the catalog, so the table appears atomically.
	m := db.store.Begin()
	var err error
	if tbl.Oid, err = m.NextOid(); err != nil {
		m.Abort()
		return nil, err
	}
	if tbl.Heap, err = m.CreateHeap(); err != nil {
		m.Abort()
		return nil, err
	}
	for _, sq := range seqs {
		if sq.Oid, err = m.NextOid(); err != nil {
			m.Abort()
			return nil, err
		}
		if sq.Page, err = m.AllocSeq(sq.Start); err != nil {
			m.Abort()
			return nil, err
		}
		sq.OwnerTable = tbl.Oid
		cat.Sequences[sq.Oid] = sq
	}
	for _, pi := range indexes {
		ix := &catalog.Index{Name: pi.name, Table: tbl.Oid, Columns: pi.cols, Unique: pi.unique, Primary: pi.primary, Constraint: true}
		if ix.Oid, err = m.NextOid(); err != nil {
			m.Abort()
			return nil, err
		}
		if ix.Root, err = m.CreateBTree(); err != nil {
			m.Abort()
			return nil, err
		}
		cat.Indexes[ix.Oid] = ix
		tbl.Indexes = append(tbl.Indexes, ix.Oid)
	}
	// A self-referencing foreign key points at the new table's oid.
	for _, fk := range tbl.FKs {
		if fk.RefTable == 0 {
			fk.RefTable = tbl.Oid
		}
	}
	cat.Tables[tbl.Oid] = tbl
	if err := db.saveCatalog(m, cat); err != nil {
		return nil, err
	}
	return &Result{Tag: "CREATE TABLE"}, nil
}

// addKeyConstraint records a PRIMARY KEY or UNIQUE constraint as an index.
func addKeyConstraint(cat *catalog.Catalog, tbl *catalog.Table, c *sql.Constraint, indexes *[]pendingIndex, taken map[string]bool) error {
	var cols []int
	for _, n := range c.Columns {
		i := tbl.ColumnIndex(n)
		if i < 0 {
			return pgerr.New(pgerr.UndefinedColumn, "column \"%s\" named in key does not exist", n)
		}
		for _, x := range cols {
			if x == i {
				return pgerr.New(pgerr.DuplicateColumn, "column \"%s\" appears twice in %s constraint", n, map[bool]string{true: "primary key", false: "unique"}[c.Kind == sql.ConPrimaryKey])
			}
		}
		cols = append(cols, i)
	}
	primary := c.Kind == sql.ConPrimaryKey
	if primary {
		for _, pi := range *indexes {
			if pi.primary {
				return pgerr.New(pgerr.InvalidTableDefinition, "multiple primary keys for table \"%s\" are not allowed", tbl.Name)
			}
		}
		for _, i := range cols {
			tbl.Columns[i].NotNull = true
		}
	}
	for _, i := range cols {
		k := tbl.Columns[i].Type.Kind()
		if k == types.KArray {
			return pgerr.Unsupported("indexes on array columns are not supported")
		}
	}
	name := c.Name
	if name == "" {
		if primary {
			name = uniqueName(cat, truncName(tbl.Name+"_pkey"), taken)
		} else {
			parts := []string{tbl.Name}
			for _, i := range cols {
				parts = append(parts, tbl.Columns[i].Name)
			}
			name = uniqueName(cat, truncName(strings.Join(parts, "_")+"_key"), taken)
		}
	} else if relationExists(cat, name) || taken[name] {
		return pgerr.New(pgerr.DuplicateTable, "relation \"%s\" already exists", name)
	} else {
		taken[name] = true
	}
	*indexes = append(*indexes, pendingIndex{name: name, cols: cols, unique: true, primary: primary, conName: name})
	return nil
}

func addCheck(cat *catalog.Catalog, tbl *catalog.Table, c *sql.Constraint) error {
	b := planner.NewBinder(cat, nil, nil)
	e, _, err := b.BindTableExpr(tbl, c.Check)
	if err != nil {
		return err
	}
	if e.Type().Oid != types.OidBool {
		return pgerr.New(pgerr.DatatypeMismatch, "argument of CHECK must be type boolean, not type %s", e.Type())
	}
	if expr.HasSubquery(e) {
		return pgerr.Unsupported("cannot use subquery in check constraint")
	}
	name := c.Name
	if name == "" {
		base := tbl.Name
		if len(c.Columns) == 1 {
			base += "_" + c.Columns[0]
		}
		name = truncName(base + "_check")
		for i := 1; ; i++ {
			dup := false
			for _, ch := range tbl.Checks {
				if ch.Name == name {
					dup = true
				}
			}
			if !dup {
				break
			}
			name = truncName(fmt.Sprintf("%s_check%d", base, i))
		}
	}
	tbl.Checks = append(tbl.Checks, &catalog.Check{Name: name, Expr: c.CheckSQL})
	return nil
}

func addForeignKey(cat *catalog.Catalog, tbl *catalog.Table, c *sql.Constraint, pending []pendingIndex) error {
	var cols []int
	for _, n := range c.Columns {
		i := tbl.ColumnIndex(n)
		if i < 0 {
			return pgerr.New(pgerr.UndefinedColumn, "column \"%s\" referenced in foreign key constraint does not exist", n)
		}
		cols = append(cols, i)
	}
	self := c.RefTable.Name == tbl.Name
	parent := tbl
	if !self {
		parent = cat.TableByName(c.RefTable.Name)
		if parent == nil {
			return pgerr.New(pgerr.UndefinedTable, "relation \"%s\" does not exist", c.RefTable.Name)
		}
	}
	var refCols []int
	if len(c.RefCols) == 0 {
		var pk []int
		if self {
			for _, pi := range pending {
				if pi.primary {
					pk = pi.cols
				}
			}
		} else if ix := cat.PrimaryKey(parent); ix != nil {
			pk = ix.Columns
		}
		if pk == nil {
			return pgerr.New("42830", "there is no primary key for referenced table \"%s\"", parent.Name)
		}
		refCols = pk
	} else {
		for _, n := range c.RefCols {
			i := parent.ColumnIndex(n)
			if i < 0 {
				return pgerr.New(pgerr.UndefinedColumn, "column \"%s\" referenced in foreign key constraint does not exist", n)
			}
			refCols = append(refCols, i)
		}
	}
	if len(refCols) != len(cols) {
		return pgerr.New("42830", "number of referencing and referenced columns for foreign key disagree")
	}
	// The referenced columns need a unique index, in this order.
	found := false
	if self {
		for _, pi := range pending {
			if sameInts(pi.cols, refCols) {
				found = true
			}
		}
	}
	for _, ix := range cat.TableIndexes(parent) {
		if ix.Unique && sameInts(ix.Columns, refCols) {
			found = true
		}
	}
	if !found {
		return pgerr.New("42830", "there is no unique constraint matching given keys for referenced table \"%s\"", parent.Name)
	}
	for i := range cols {
		a, b := tbl.Columns[cols[i]].Type, parent.Columns[refCols[i]].Type
		if _, ok := expr.CommonType(a, b); !ok {
			return pgerr.New(pgerr.DatatypeMismatch, "foreign key constraint cannot be implemented").
				WithDetail("Key columns \"%s\" and \"%s\" are of incompatible types: %s and %s.", tbl.Columns[cols[i]].Name, parent.Columns[refCols[i]].Name, a, b)
		}
	}
	name := c.Name
	if name == "" {
		parts := []string{tbl.Name}
		for _, i := range cols {
			parts = append(parts, tbl.Columns[i].Name)
		}
		name = truncName(strings.Join(parts, "_") + "_fkey")
	}
	onDel, onUpd := c.OnDelete, c.OnUpdate
	if onDel == "" {
		onDel = "NO ACTION"
	}
	if onUpd == "" {
		onUpd = "NO ACTION"
	}
	fk := &catalog.ForeignKey{Name: name, Columns: cols, RefColumns: refCols, OnDelete: onDel, OnUpdate: onUpd}
	if !self {
		fk.RefTable = parent.Oid
	}
	tbl.FKs = append(tbl.FKs, fk)
	return nil
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Session) createTableAs(t *txn.Txn, x *sql.CreateTableStmt) (*Result, error) {
	// Plan the query to learn the columns, create the table, then insert.
	ps, _, err := s.plan(x.AsSelect, nil)
	if err != nil {
		return nil, err
	}
	ct := &sql.CreateTableStmt{Table: x.Table, IfNotExists: x.IfNotExists}
	for _, o := range ps.output {
		typ := o.T
		if typ.Oid == types.OidUnknown {
			typ = types.Text
		}
		ct.Columns = append(ct.Columns, &sql.ColumnDef{Name: o.Name, Type: &sql.TypeName{Name: typ.TypName(), Mods: modsOf(typ), Array: false}})
		if typ.IsArray() {
			ct.Columns[len(ct.Columns)-1].Type = &sql.TypeName{Name: typ.Elem().TypName(), Array: true}
		}
	}
	if _, err := s.createTable(t, ct); err != nil {
		return nil, err
	}
	ins := &sql.InsertStmt{Table: &sql.TableName{Name: x.Table.Name}, Source: x.AsSelect}
	s.beginTxn()
	s.txn.Ctx = t.Ctx
	res, err := s.execPlanned(t.Ctx, ins, nil, nil)
	if err != nil {
		_ = s.endTxn(false)
		_, _ = s.drop(t, &sql.DropStmt{Kind: sql.DropTable, Names: []*sql.TableName{{Name: x.Table.Name}}})
		return nil, err
	}
	if err := s.endTxn(true); err != nil {
		return nil, err
	}
	n := strings.TrimPrefix(res.Tag, "INSERT 0 ")
	return &Result{Tag: "SELECT " + n}, nil
}

func modsOf(t types.T) []int {
	if n := t.CharLen(); n > 0 {
		return []int{n}
	}
	if p, sc, ok := t.NumericPrecScale(); ok {
		return []int{p, sc}
	}
	return nil
}

func (s *Session) createSequence(t *txn.Txn, x *sql.CreateSequenceStmt) (*Result, error) {
	db := s.db
	db.ddlMu.Lock()
	defer db.ddlMu.Unlock()
	cat := db.Catalog().Clone()
	if relationExists(cat, x.Name.Name) {
		if x.IfNotExists {
			return &Result{Tag: "CREATE SEQUENCE"}, nil
		}
		return nil, pgerr.New(pgerr.DuplicateTable, "relation \"%s\" already exists", x.Name.Name)
	}
	if x.Increment == 0 {
		return nil, pgerr.New(pgerr.InvalidParameterValue, "INCREMENT must not be zero")
	}
	sq := &catalog.Sequence{Name: x.Name.Name, Increment: x.Increment, Start: x.Start}
	m := db.store.Begin()
	var err error
	if sq.Oid, err = m.NextOid(); err != nil {
		m.Abort()
		return nil, err
	}
	if sq.Page, err = m.AllocSeq(x.Start); err != nil {
		m.Abort()
		return nil, err
	}
	cat.Sequences[sq.Oid] = sq
	if err := db.saveCatalog(m, cat); err != nil {
		return nil, err
	}
	return &Result{Tag: "CREATE SEQUENCE"}, nil
}

// buildIndex fills a new index from the table's tuple versions. Every
// version that might still be visible to some snapshot gets an entry;
// uniqueness is enforced among the live versions only.
func (s *Session) buildIndex(t *txn.Txn, tbl *catalog.Table, ix *catalog.Index) error {
	cat := s.db.Catalog()
	xc := executor.New(t, s.db.store, cat, &expr.Ctx{Env: s})
	return xc.BuildIndex(tbl, ix)
}

func (s *Session) createIndex(t *txn.Txn, x *sql.CreateIndexStmt) (*Result, error) {
	db := s.db
	cur := db.Catalog()
	tbl := cur.TableByName(x.Table.Name)
	if tbl == nil {
		return nil, pgerr.New(pgerr.UndefinedTable, "relation \"%s\" does not exist", x.Table.Name)
	}
	if x.Using != "" && x.Using != "btree" {
		return nil, pgerr.Unsupported("access method \"%s\" is not supported (only btree)", x.Using)
	}
	// Block writers (not readers) while the index is built.
	if err := lockTable(t, tbl.Oid, txn.Share); err != nil {
		return nil, err
	}
	db.ddlMu.Lock()
	defer db.ddlMu.Unlock()
	cat := db.Catalog().Clone()
	tbl = cat.TableByName(x.Table.Name)
	if tbl == nil {
		return nil, pgerr.New(pgerr.UndefinedTable, "relation \"%s\" does not exist", x.Table.Name)
	}
	var cols []int
	for _, el := range x.Columns {
		i := tbl.ColumnIndex(el.Column)
		if i < 0 {
			return nil, pgerr.New(pgerr.UndefinedColumn, "column \"%s\" does not exist", el.Column)
		}
		if el.Desc {
			s.Notices = append(s.Notices, "DESC index columns are stored in ascending order")
		}
		if tbl.Columns[i].Type.Kind() == types.KArray {
			return nil, pgerr.Unsupported("indexes on array columns are not supported")
		}
		cols = append(cols, i)
	}
	name := x.Name
	if name == "" {
		parts := []string{tbl.Name}
		for _, i := range cols {
			parts = append(parts, tbl.Columns[i].Name)
		}
		name = uniqueName(cat, truncName(strings.Join(parts, "_")+"_idx"), map[string]bool{})
	} else if relationExists(cat, name) {
		if x.IfNotExists {
			s.Notices = append(s.Notices, fmt.Sprintf("relation \"%s\" already exists, skipping", name))
			return &Result{Tag: "CREATE INDEX"}, nil
		}
		return nil, pgerr.New(pgerr.DuplicateTable, "relation \"%s\" already exists", name)
	}
	ix := &catalog.Index{Name: name, Table: tbl.Oid, Columns: cols, Unique: x.Unique}
	m := db.store.Begin()
	var err error
	if ix.Oid, err = m.NextOid(); err != nil {
		m.Abort()
		return nil, err
	}
	if ix.Root, err = m.CreateBTree(); err != nil {
		m.Abort()
		return nil, err
	}
	lsn, err := m.Commit()
	if err != nil {
		return nil, err
	}
	if err := db.store.Flush(lsn); err != nil {
		return nil, err
	}
	if err := s.buildIndex(t, tbl, ix); err != nil {
		_ = db.store.FreeBTree(ix.Root)
		return nil, err
	}
	cat.Indexes[ix.Oid] = ix
	tbl.Indexes = append(tbl.Indexes, ix.Oid)
	if err := db.saveCatalog(db.store.Begin(), cat); err != nil {
		return nil, err
	}
	return &Result{Tag: "CREATE INDEX"}, nil
}

func (s *Session) drop(t *txn.Txn, x *sql.DropStmt) (*Result, error) {
	db := s.db
	tag := [...]string{"DROP TABLE", "DROP INDEX", "DROP SEQUENCE", "DROP VIEW"}[x.Kind]
	if x.Kind == sql.DropView {
		return nil, pgerr.Unsupported("views are not supported")
	}
	// Lock the tables first (waiting for their users to finish).
	cur := db.Catalog()
	for _, n := range x.Names {
		switch x.Kind {
		case sql.DropTable:
			if tbl := cur.TableByName(n.Name); tbl != nil {
				if err := lockTable(t, tbl.Oid, txn.AccessExclusive); err != nil {
					return nil, err
				}
			}
		case sql.DropIndex:
			if ix := cur.IndexByName(n.Name); ix != nil {
				if err := lockTable(t, ix.Table, txn.AccessExclusive); err != nil {
					return nil, err
				}
			}
		}
	}
	db.ddlMu.Lock()
	defer db.ddlMu.Unlock()
	cat := db.Catalog().Clone()
	var freeHeaps, freeTrees, freeSeqs []storage.PageID
	for _, n := range x.Names {
		switch x.Kind {
		case sql.DropTable:
			tbl := cat.TableByName(n.Name)
			if tbl == nil {
				if x.IfExists {
					s.Notices = append(s.Notices, fmt.Sprintf("table \"%s\" does not exist, skipping", n.Name))
					continue
				}
				return nil, pgerr.New(pgerr.UndefinedTable, "table \"%s\" does not exist", n.Name)
			}
			for _, ref := range cat.ReferencedBy(tbl.Oid) {
				if ref.Table.Oid == tbl.Oid {
					continue
				}
				dropping := false
				for _, o := range x.Names {
					if o.Name == ref.Table.Name {
						dropping = true
					}
				}
				if dropping {
					continue
				}
				if !x.Cascade {
					return nil, pgerr.New("2BP01", "cannot drop table %s because other objects depend on it", tbl.Name).
						WithDetail("constraint %s on table %s depends on table %s", ref.FK.Name, ref.Table.Name, tbl.Name).
						WithHint("Use DROP ... CASCADE to drop the dependent objects too.")
				}
				s.Notices = append(s.Notices, fmt.Sprintf("drop cascades to constraint %s on table %s", ref.FK.Name, ref.Table.Name))
				var keep []*catalog.ForeignKey
				for _, fk := range ref.Table.FKs {
					if fk != ref.FK {
						keep = append(keep, fk)
					}
				}
				ref.Table.FKs = keep
			}
			for _, oid := range tbl.Indexes {
				if ix := cat.Indexes[oid]; ix != nil {
					freeTrees = append(freeTrees, ix.Root)
					delete(cat.Indexes, oid)
				}
			}
			for oid, sq := range cat.Sequences {
				if sq.OwnerTable == tbl.Oid {
					freeSeqs = append(freeSeqs, sq.Page)
					delete(cat.Sequences, oid)
				}
			}
			freeHeaps = append(freeHeaps, tbl.Heap)
			delete(cat.Tables, tbl.Oid)
		case sql.DropIndex:
			ix := cat.IndexByName(n.Name)
			if ix == nil {
				if x.IfExists {
					s.Notices = append(s.Notices, fmt.Sprintf("index \"%s\" does not exist, skipping", n.Name))
					continue
				}
				return nil, pgerr.New(pgerr.UndefinedObject, "index \"%s\" does not exist", n.Name)
			}
			if ix.Constraint {
				tbl := cat.Tables[ix.Table]
				return nil, pgerr.New("2BP01", "cannot drop index %s because constraint %s on table %s requires it", ix.Name, ix.Name, tbl.Name).
					WithHint("You can drop constraint %s on table %s instead.", ix.Name, tbl.Name)
			}
			tbl := cat.Tables[ix.Table]
			var keep []uint32
			for _, o := range tbl.Indexes {
				if o != ix.Oid {
					keep = append(keep, o)
				}
			}
			tbl.Indexes = keep
			freeTrees = append(freeTrees, ix.Root)
			delete(cat.Indexes, ix.Oid)
		case sql.DropSequence:
			sq := cat.SequenceByName(n.Name)
			if sq == nil {
				if x.IfExists {
					continue
				}
				return nil, pgerr.New(pgerr.UndefinedTable, "sequence \"%s\" does not exist", n.Name)
			}
			freeSeqs = append(freeSeqs, sq.Page)
			delete(cat.Sequences, sq.Oid)
		}
	}
	if err := db.saveCatalog(db.store.Begin(), cat); err != nil {
		return nil, err
	}
	// Free the storage after the catalog no longer references it.
	for _, h := range freeHeaps {
		if err := db.store.FreeHeap(h); err != nil {
			return nil, err
		}
	}
	for _, r := range freeTrees {
		if err := db.store.FreeBTree(r); err != nil {
			return nil, err
		}
	}
	if len(freeSeqs) > 0 {
		m := db.store.Begin()
		for _, p := range freeSeqs {
			if err := m.Free(p); err != nil {
				m.Abort()
				return nil, err
			}
		}
		if _, err := m.Commit(); err != nil {
			return nil, err
		}
	}
	return &Result{Tag: tag}, nil
}

func (s *Session) truncate(t *txn.Txn, x *sql.TruncateStmt) (*Result, error) {
	db := s.db
	cur := db.Catalog()
	for _, n := range x.Tables {
		tbl := cur.TableByName(n.Name)
		if tbl == nil {
			return nil, pgerr.New(pgerr.UndefinedTable, "relation \"%s\" does not exist", n.Name)
		}
		for _, ref := range cur.ReferencedBy(tbl.Oid) {
			if ref.Table.Oid != tbl.Oid {
				return nil, pgerr.New(pgerr.FeatureNotSupported, "cannot truncate a table referenced in a foreign key constraint").
					WithDetail("Table \"%s\" references \"%s\".", ref.Table.Name, tbl.Name)
			}
		}
		if err := lockTable(t, tbl.Oid, txn.AccessExclusive); err != nil {
			return nil, err
		}
	}
	db.ddlMu.Lock()
	defer db.ddlMu.Unlock()
	cat := db.Catalog().Clone()
	var oldHeaps, oldTrees []storage.PageID
	m := db.store.Begin()
	for _, n := range x.Tables {
		tbl := cat.TableByName(n.Name)
		oldHeaps = append(oldHeaps, tbl.Heap)
		var err error
		if tbl.Heap, err = m.CreateHeap(); err != nil {
			m.Abort()
			return nil, err
		}
		for _, oid := range tbl.Indexes {
			ix := cat.Indexes[oid]
			oldTrees = append(oldTrees, ix.Root)
			if ix.Root, err = m.CreateBTree(); err != nil {
				m.Abort()
				return nil, err
			}
		}
		tbl.Stats = nil
	}
	if err := db.saveCatalog(m, cat); err != nil {
		return nil, err
	}
	for _, h := range oldHeaps {
		if err := db.store.FreeHeap(h); err != nil {
			return nil, err
		}
	}
	for _, r := range oldTrees {
		if err := db.store.FreeBTree(r); err != nil {
			return nil, err
		}
	}
	return &Result{Tag: "TRUNCATE TABLE"}, nil
}

func (s *Session) alterTable(t *txn.Txn, x *sql.AlterTableStmt) (*Result, error) {
	db := s.db
	cur := db.Catalog()
	tbl := cur.TableByName(x.Table.Name)
	if tbl == nil {
		if x.IfExists {
			return &Result{Tag: "ALTER TABLE"}, nil
		}
		return nil, pgerr.New(pgerr.UndefinedTable, "relation \"%s\" does not exist", x.Table.Name)
	}
	if err := lockTable(t, tbl.Oid, txn.AccessExclusive); err != nil {
		return nil, err
	}
	db.ddlMu.Lock()
	defer db.ddlMu.Unlock()
	cat := db.Catalog().Clone()
	tbl = cat.Tables[tbl.Oid]
	switch {
	case x.RenameTo != "":
		if relationExists(cat, x.RenameTo) {
			return nil, pgerr.New(pgerr.DuplicateTable, "relation \"%s\" already exists", x.RenameTo)
		}
		tbl.Name = x.RenameTo
	case x.RenameCol[0] != "":
		i := tbl.ColumnIndex(x.RenameCol[0])
		if i < 0 {
			return nil, pgerr.New(pgerr.UndefinedColumn, "column \"%s\" does not exist", x.RenameCol[0])
		}
		if tbl.ColumnIndex(x.RenameCol[1]) >= 0 {
			return nil, pgerr.New(pgerr.DuplicateColumn, "column \"%s\" of relation \"%s\" already exists", x.RenameCol[1], tbl.Name)
		}
		if len(tbl.Checks) > 0 {
			return nil, pgerr.Unsupported("renaming a column of a table with CHECK constraints is not supported")
		}
		tbl.Columns[i].Name = x.RenameCol[1]
	case x.AddColumn != nil:
		cd := x.AddColumn
		if tbl.ColumnIndex(cd.Name) >= 0 {
			return nil, pgerr.New(pgerr.DuplicateColumn, "column \"%s\" of relation \"%s\" already exists", cd.Name, tbl.Name)
		}
		typ, err := planner.ResolveTypeName(cd.Type)
		if err != nil {
			return nil, err
		}
		if types.IsSerial(cd.Type.Name) || cd.Identity || len(cd.Constraints) > 0 {
			return nil, pgerr.Unsupported("ADD COLUMN with constraints or serial types is not supported")
		}
		col := &catalog.Column{Name: cd.Name, Type: typ, NotNull: cd.NotNull}
		if cd.Default != nil {
			col.Default = cd.DefaultSQL
			// Existing rows take the default's value, which must be a
			// constant.
			b := planner.NewBinder(cat, nil, nil)
			e, err := b.BindExprNoColumns(cd.Default, typ)
			if err != nil {
				return nil, err
			}
			rw := &planner.Rewriter{Ctx: &expr.Ctx{Env: s}}
			c, ok := rw.Fold(e).(*expr.Const)
			if !ok || expr.IsVolatile(e) {
				return nil, pgerr.Unsupported("ADD COLUMN with a non-constant default is not supported")
			}
			if !c.V.IsNull() {
				col.Missing, col.HasMissing = types.ToText(c.V, typ), true
			}
		}
		if col.NotNull && !col.HasMissing {
			// Fine only if the table is empty.
			n, err := s.countVisible(t, tbl)
			if err != nil {
				return nil, err
			}
			if n > 0 {
				return nil, pgerr.New(pgerr.NotNullViolation, "column \"%s\" of relation \"%s\" contains null values", cd.Name, tbl.Name)
			}
		}
		tbl.Columns = append(tbl.Columns, col)
		tbl.Stats = nil
	case x.DropColumn != "":
		return nil, pgerr.Unsupported("ALTER TABLE DROP COLUMN is not supported")
	case x.AddConstraint != nil:
		c := x.AddConstraint
		switch c.Kind {
		case sql.ConCheck:
			if err := addCheck(cat, tbl, c); err != nil {
				return nil, err
			}
			if err := s.validateExisting(t, cat, tbl); err != nil {
				return nil, err
			}
		case sql.ConForeignKey:
			if err := addForeignKey(cat, tbl, c, nil); err != nil {
				return nil, err
			}
			if err := s.validateExisting(t, cat, tbl); err != nil {
				return nil, err
			}
		default:
			var pend []pendingIndex
			if c.Kind == sql.ConPrimaryKey && cat.PrimaryKey(tbl) != nil {
				return nil, pgerr.New(pgerr.InvalidTableDefinition, "multiple primary keys for table \"%s\" are not allowed", tbl.Name)
			}
			if err := addKeyConstraint(cat, tbl, c, &pend, map[string]bool{}); err != nil {
				return nil, err
			}
			pi := pend[0]
			ix := &catalog.Index{Name: pi.name, Table: tbl.Oid, Columns: pi.cols, Unique: true, Primary: pi.primary, Constraint: true}
			m := db.store.Begin()
			var err error
			if ix.Oid, err = m.NextOid(); err != nil {
				m.Abort()
				return nil, err
			}
			if ix.Root, err = m.CreateBTree(); err != nil {
				m.Abort()
				return nil, err
			}
			if _, err := m.Commit(); err != nil {
				return nil, err
			}
			if pi.primary {
				if err := s.validateExisting(t, cat, tbl); err != nil {
					_ = db.store.FreeBTree(ix.Root)
					return nil, err
				}
			}
			if err := s.buildIndex(t, tbl, ix); err != nil {
				_ = db.store.FreeBTree(ix.Root)
				return nil, err
			}
			cat.Indexes[ix.Oid] = ix
			tbl.Indexes = append(tbl.Indexes, ix.Oid)
		}
	}
	if err := db.saveCatalog(db.store.Begin(), cat); err != nil {
		return nil, err
	}
	return &Result{Tag: "ALTER TABLE"}, nil
}

// validateExisting checks the existing rows of tbl against the (new)
// constraints in cat.
func (s *Session) validateExisting(t *txn.Txn, cat *catalog.Catalog, tbl *catalog.Table) error {
	xc := executor.New(t, s.db.store, cat, &expr.Ctx{Env: s})
	return xc.ValidateTable(tbl)
}

func (s *Session) countVisible(t *txn.Txn, tbl *catalog.Table) (int, error) {
	n := 0
	cur := s.db.store.Heap(tbl.Heap).Scan()
	for {
		_, tuple, ok, err := cur.Next()
		if err != nil {
			return 0, err
		}
		if !ok {
			return n, nil
		}
		if t.Visible(storage.DecodeHeader(tuple)) {
			n++
		}
	}
}
