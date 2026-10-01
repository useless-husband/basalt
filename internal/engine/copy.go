package engine

import (
	"fmt"
	"strings"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/sql"
	"github.com/useless-husband/basalt/internal/types"
)

// CopyTarget resolves the table and columns of COPY ... FROM STDIN.
func (s *Session) CopyTarget(cp *sql.CopyStmt) (*catalog.Table, []int, error) {
	if s.failed {
		return nil, nil, pgerr.New(pgerr.InFailedSQLTransaction, "current transaction is aborted, commands ignored until end of transaction block")
	}
	t := s.catalog().TableByName(cp.Table.Name)
	if t == nil {
		return nil, nil, pgerr.New(pgerr.UndefinedTable, "relation \"%s\" does not exist", cp.Table.Name)
	}
	var cols []int
	if len(cp.Columns) == 0 {
		for i := range t.Columns {
			cols = append(cols, i)
		}
	} else {
		for _, n := range cp.Columns {
			i := t.ColumnIndex(n)
			if i < 0 {
				return nil, nil, pgerr.New(pgerr.UndefinedColumn, "column \"%s\" of relation \"%s\" does not exist", n, t.Name)
			}
			cols = append(cols, i)
		}
	}
	return t, cols, nil
}

const copyBatch = 200

// CopyFrom inserts rows given as text fields (nil = NULL), in batches of
// multi-row INSERTs, inside the current or an implicit transaction.
func (s *Session) CopyFrom(t *catalog.Table, cols []int, rows [][]*string) (int64, error) {
	var names []string
	var ptypes []uint32
	for _, c := range cols {
		names = append(names, catalog.QuoteIdent(t.Columns[c].Name))
	}
	prefix := fmt.Sprintf("INSERT INTO %s (%s) VALUES ", catalog.QuoteIdent(t.Name), strings.Join(names, ", "))
	var total int64
	prepared := map[int]*Prepared{}
	for start := 0; start < len(rows); start += copyBatch {
		batch := rows[start:min(start+copyBatch, len(rows))]
		p := prepared[len(batch)]
		if p == nil {
			var b strings.Builder
			b.WriteString(prefix)
			n := 1
			ptypes = ptypes[:0]
			for r := range batch {
				if r > 0 {
					b.WriteString(", ")
				}
				b.WriteByte('(')
				for k, c := range cols {
					if k > 0 {
						b.WriteString(", ")
					}
					fmt.Fprintf(&b, "$%d", n)
					n++
					ptypes = append(ptypes, t.Columns[c].Type.Oid)
				}
				b.WriteByte(')')
			}
			var err error
			if p, err = s.Prepare("", b.String(), ptypes); err != nil {
				return total, err
			}
			prepared[len(batch)] = p
		}
		params := make([]types.Value, 0, len(batch)*len(cols))
		for ri, r := range batch {
			if len(r) != len(cols) {
				return total, pgerr.New("22P04", "line %d: expected %d columns, got %d", start+ri+1, len(cols), len(r))
			}
			for k, f := range r {
				if f == nil {
					params = append(params, types.Null)
					continue
				}
				v, err := types.Parse(*f, t.Columns[cols[k]].Type)
				if err != nil {
					return total, err
				}
				params = append(params, v)
			}
		}
		res, err := s.ExecPrepared(p, params)
		if err != nil {
			return total, err
		}
		var n int64
		fmt.Sscanf(res.Tag, "INSERT 0 %d", &n)
		total += n
	}
	if !s.explicit && s.txn != nil {
		if err := s.endTxn(true); err != nil {
			return total, err
		}
	}
	return total, nil
}
