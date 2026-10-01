package sql

import (
	"math"
	"strconv"
	"strings"

	"github.com/useless-husband/basalt/internal/pgerr"
)

// RawStmt is a parsed statement with its source text.
type RawStmt struct {
	Stmt Stmt
	SQL  string
}

// Parser is a recursive-descent parser over a token slice.
type Parser struct {
	src  string
	toks []Token
	pos  int
}

// Parse parses a string that may contain several statements separated by
// semicolons. Empty statements are dropped.
func Parse(src string) ([]RawStmt, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := &Parser{src: src, toks: toks}
	var out []RawStmt
	for {
		for p.isOp(";") {
			p.pos++
		}
		if p.peek().Kind == TEOF {
			break
		}
		start := p.peek().Pos
		st, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		end := p.peek().Pos
		if !p.isOp(";") && p.peek().Kind != TEOF {
			return nil, p.errorf("syntax error at or near \"%s\"", p.peek())
		}
		out = append(out, RawStmt{Stmt: st, SQL: strings.TrimSpace(src[start:end])})
	}
	return out, nil
}

// ParseOne parses exactly one statement.
func ParseOne(src string) (Stmt, error) {
	stmts, err := Parse(src)
	if err != nil {
		return nil, err
	}
	if len(stmts) != 1 {
		return nil, pgerr.New(pgerr.SyntaxError, "expected exactly one statement, got %d", len(stmts))
	}
	return stmts[0].Stmt, nil
}

// ParseExpr parses a standalone expression (used for stored defaults and
// CHECK constraints).
func ParseExpr(src string) (Expr, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := &Parser{src: src, toks: toks}
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().Kind != TEOF {
		return nil, p.errorf("syntax error at or near \"%s\"", p.peek())
	}
	return e, nil
}

// ---- token helpers ----

func (p *Parser) peek() Token { return p.toks[p.pos] }

func (p *Parser) peekN(n int) Token {
	if p.pos+n < len(p.toks) {
		return p.toks[p.pos+n]
	}
	return p.toks[len(p.toks)-1]
}

func (p *Parser) next() Token {
	t := p.toks[p.pos]
	if t.Kind != TEOF {
		p.pos++
	}
	return t
}

func (p *Parser) errorf(format string, args ...any) error {
	return syntaxErr(p.peek().Pos, format, args...)
}

func (p *Parser) unexpected() error {
	t := p.peek()
	if t.Kind == TEOF {
		return syntaxErr(t.Pos, "syntax error at end of input")
	}
	return syntaxErr(t.Pos, "syntax error at or near \"%s\"", t)
}

// isKw reports whether the current token is the given (unquoted) keyword.
func (p *Parser) isKw(kw string) bool {
	t := p.peek()
	return t.Kind == TIdent && t.Val == kw
}

func (p *Parser) isKwN(n int, kw string) bool {
	t := p.peekN(n)
	return t.Kind == TIdent && t.Val == kw
}

func (p *Parser) acceptKw(kws ...string) bool {
	for i, kw := range kws {
		if !p.isKwN(i, kw) {
			return false
		}
	}
	p.pos += len(kws)
	return true
}

func (p *Parser) expectKw(kws ...string) error {
	for _, kw := range kws {
		if !p.isKw(kw) {
			return p.unexpected()
		}
		p.pos++
	}
	return nil
}

func (p *Parser) isOp(op string) bool {
	t := p.peek()
	return t.Kind == TOp && t.Val == op
}

func (p *Parser) acceptOp(op string) bool {
	if p.isOp(op) {
		p.pos++
		return true
	}
	return false
}

func (p *Parser) expectOp(op string) error {
	if !p.acceptOp(op) {
		return p.unexpected()
	}
	return nil
}

var reserved = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`all analyse analyze and any array as asc asymmetric both case cast check
		collate column constraint create current_catalog current_date current_role current_time
		current_timestamp current_user default deferrable desc distinct do else end except false fetch
		for foreign from grant group having in initially intersect into lateral leading limit localtime
		localtimestamp not null offset on only or order placing primary references returning select
		session_user some symmetric table then to trailing true union unique user using variadic when
		where window with`) {
		reserved[w] = true
	}
}

// aliasStop are non-reserved words that still cannot start an implicit alias.
var aliasStop = map[string]bool{
	"join": true, "inner": true, "left": true, "right": true, "full": true, "cross": true,
	"natural": true, "outer": true, "set": true, "like": true, "ilike": true, "is": true,
	"isnull": true, "notnull": true, "between": true, "similar": true, "overlaps": true,
	"on": true, "using": true, "values": true,
}

// ident parses an identifier (quoted or an unreserved keyword).
func (p *Parser) ident() (string, error) {
	t := p.peek()
	switch t.Kind {
	case TQuotedIdent:
		p.pos++
		return t.Val, nil
	case TIdent:
		if reserved[t.Val] {
			return "", p.unexpected()
		}
		p.pos++
		return t.Val, nil
	}
	return "", p.unexpected()
}

// colLabel parses any identifier, including reserved keywords (used after AS).
func (p *Parser) colLabel() (string, error) {
	t := p.peek()
	if t.Kind == TIdent || t.Kind == TQuotedIdent {
		p.pos++
		return t.Val, nil
	}
	return "", p.unexpected()
}

func (p *Parser) canBeImplicitAlias() bool {
	t := p.peek()
	if t.Kind == TQuotedIdent {
		return true
	}
	return t.Kind == TIdent && !reserved[t.Val] && !aliasStop[t.Val]
}

func (p *Parser) identList() ([]string, error) {
	var out []string
	for {
		id, err := p.colLabel()
		if err != nil {
			return nil, err
		}
		out = append(out, id)
		if !p.acceptOp(",") {
			return out, nil
		}
	}
}

func (p *Parser) parenIdentList() ([]string, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	l, err := p.identList()
	if err != nil {
		return nil, err
	}
	return l, p.expectOp(")")
}

// qualifiedName parses [schema.]name.
func (p *Parser) qualifiedName() (*TableName, error) {
	pos := p.peek().Pos
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	tn := &TableName{Name: name, Pos: pos}
	if p.acceptOp(".") {
		n2, err := p.colLabel()
		if err != nil {
			return nil, err
		}
		tn.Schema, tn.Name = name, n2
		if p.acceptOp(".") { // catalog.schema.name
			n3, err := p.colLabel()
			if err != nil {
				return nil, err
			}
			tn.Schema, tn.Name = n2, n3
		}
	}
	return tn, nil
}

// ---- statements ----

func (p *Parser) parseStatement() (Stmt, error) {
	t := p.peek()
	if t.Kind == TOp && t.Val == "(" {
		return p.parseQuery()
	}
	if t.Kind != TIdent {
		return nil, p.unexpected()
	}
	switch t.Val {
	case "select", "values", "table":
		return p.parseQuery()
	case "with":
		return p.parseWithStatement()
	case "insert":
		return p.parseInsert(nil)
	case "update":
		return p.parseUpdate(nil)
	case "delete":
		return p.parseDelete(nil)
	case "create":
		return p.parseCreate()
	case "drop":
		return p.parseDrop()
	case "alter":
		return p.parseAlter()
	case "truncate":
		p.next()
		p.acceptKw("table")
		var tables []*TableName
		for {
			tn, err := p.qualifiedName()
			if err != nil {
				return nil, err
			}
			tables = append(tables, tn)
			if !p.acceptOp(",") {
				break
			}
		}
		p.acceptKw("restart", "identity")
		p.acceptKw("continue", "identity")
		p.acceptKw("cascade")
		p.acceptKw("restrict")
		return &TruncateStmt{Tables: tables}, nil
	case "begin", "start":
		p.next()
		if t.Val == "start" {
			if err := p.expectKw("transaction"); err != nil {
				return nil, err
			}
		} else if !p.acceptKw("transaction") {
			p.acceptKw("work")
		}
		st := &TransactionStmt{Kind: TxnBegin}
		return st, p.parseTxnModes(st)
	case "commit", "end":
		p.next()
		if !p.acceptKw("transaction") {
			p.acceptKw("work")
		}
		p.acceptKw("and", "no", "chain")
		return &TransactionStmt{Kind: TxnCommit}, nil
	case "rollback", "abort":
		p.next()
		if !p.acceptKw("transaction") {
			p.acceptKw("work")
		}
		if p.acceptKw("to") {
			p.acceptKw("savepoint")
			name, err := p.ident()
			if err != nil {
				return nil, err
			}
			return &TransactionStmt{Kind: TxnRollbackTo, Savepoint: name}, nil
		}
		p.acceptKw("and", "no", "chain")
		return &TransactionStmt{Kind: TxnRollback}, nil
	case "savepoint":
		p.next()
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		return &TransactionStmt{Kind: TxnSavepoint, Savepoint: name}, nil
	case "release":
		p.next()
		p.acceptKw("savepoint")
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		return &TransactionStmt{Kind: TxnRelease, Savepoint: name}, nil
	case "explain":
		return p.parseExplain()
	case "analyze", "analyse":
		p.next()
		p.acceptKw("verbose")
		st := &AnalyzeStmt{}
		if p.peek().Kind == TIdent || p.peek().Kind == TQuotedIdent {
			tn, err := p.qualifiedName()
			if err != nil {
				return nil, err
			}
			st.Table = tn
		}
		return st, nil
	case "vacuum":
		p.next()
		st := &VacuumStmt{}
		for {
			switch {
			case p.acceptKw("full"):
				st.Full = true
			case p.acceptKw("analyze"), p.acceptKw("analyse"):
				st.Analyze = true
			case p.acceptKw("verbose"), p.acceptKw("freeze"):
			default:
				goto done
			}
		}
	done:
		if p.peek().Kind == TIdent || p.peek().Kind == TQuotedIdent {
			tn, err := p.qualifiedName()
			if err != nil {
				return nil, err
			}
			st.Table = tn
		}
		return st, nil
	case "set":
		return p.parseSet()
	case "reset":
		p.next()
		if p.acceptKw("all") {
			return &SetStmt{Name: "all", Reset: true}, nil
		}
		name, err := p.settingName()
		if err != nil {
			return nil, err
		}
		return &SetStmt{Name: name, Reset: true}, nil
	case "show":
		p.next()
		if p.acceptKw("transaction", "isolation", "level") {
			return &ShowStmt{Name: "transaction_isolation"}, nil
		}
		if p.acceptKw("time", "zone") {
			return &ShowStmt{Name: "timezone"}, nil
		}
		name, err := p.settingName()
		if err != nil {
			return nil, err
		}
		return &ShowStmt{Name: name}, nil
	case "checkpoint":
		p.next()
		return &CheckpointStmt{}, nil
	case "deallocate":
		p.next()
		p.acceptKw("prepare")
		if p.acceptKw("all") {
			return &DeallocateStmt{All: true}, nil
		}
		name, err := p.colLabel()
		if err != nil {
			return nil, err
		}
		return &DeallocateStmt{Name: name}, nil
	case "discard":
		p.next()
		if !p.acceptKw("all") && !p.acceptKw("plans") && !p.acceptKw("sequences") && !p.acceptKw("temp") {
			return nil, p.unexpected()
		}
		return &DiscardStmt{}, nil
	case "copy":
		return p.parseCopy()
	case "listen", "unlisten", "notify":
		p.next()
		for p.peek().Kind != TEOF && !p.isOp(";") {
			p.next()
		}
		return &NoopStmt{Tag: strings.ToUpper(t.Val)}, nil
	}
	return nil, p.unexpected()
}

func (p *Parser) parseTxnModes(st *TransactionStmt) error {
	for {
		switch {
		case p.acceptKw("isolation", "level"):
			lvl, err := p.isolationLevel()
			if err != nil {
				return err
			}
			st.Isolation = lvl
		case p.acceptKw("read", "only"):
			st.ReadOnly = true
		case p.acceptKw("read", "write"):
			st.ReadOnly = false
		case p.acceptKw("not", "deferrable"), p.acceptKw("deferrable"):
		default:
			return nil
		}
		p.acceptOp(",")
	}
}

func (p *Parser) isolationLevel() (string, error) {
	switch {
	case p.acceptKw("serializable"):
		return "serializable", nil
	case p.acceptKw("repeatable", "read"):
		return "repeatable read", nil
	case p.acceptKw("read", "committed"):
		return "read committed", nil
	case p.acceptKw("read", "uncommitted"):
		return "read uncommitted", nil
	}
	return "", p.unexpected()
}

func (p *Parser) settingName() (string, error) {
	name, err := p.colLabel()
	if err != nil {
		return "", err
	}
	for p.acceptOp(".") {
		n2, err := p.colLabel()
		if err != nil {
			return "", err
		}
		name += "." + n2
	}
	return name, nil
}

func (p *Parser) parseSet() (Stmt, error) {
	p.next() // SET
	st := &SetStmt{}
	sessionChars := p.acceptKw("session", "characteristics", "as", "transaction")
	if p.acceptKw("local") {
		st.Local = true
	} else if !sessionChars {
		p.acceptKw("session")
	}
	if sessionChars || p.acceptKw("transaction") {
		ts := &TransactionStmt{}
		if err := p.parseTxnModes(ts); err != nil {
			return nil, err
		}
		st.Name = "transaction"
		st.TxnIsolation = ts.Isolation
		if sessionChars {
			st.Name = "default_transaction_isolation"
			st.Value = ts.Isolation
			if st.Value == "" {
				st.Reset = true
			}
		}
		return st, nil
	}
	if p.acceptKw("time", "zone") {
		st.Name = "timezone"
	} else if p.acceptKw("names") {
		st.Name = "client_encoding"
	} else {
		name, err := p.settingName()
		if err != nil {
			return nil, err
		}
		st.Name = name
		if !p.acceptOp("=") && !p.acceptKw("to") {
			return nil, p.unexpected()
		}
	}
	// Value: a list of identifiers, strings or numbers.
	var parts []string
	for {
		t := p.peek()
		switch {
		case t.Kind == TIdent && t.Val == "default":
			p.next()
			st.Reset = true
		case t.Kind == TIdent || t.Kind == TQuotedIdent || t.Kind == TString || t.Kind == TNumber:
			p.next()
			parts = append(parts, t.Val)
		case t.Kind == TOp && (t.Val == "-" || t.Val == "+") && p.peekN(1).Kind == TNumber:
			p.next()
			parts = append(parts, t.Val+p.next().Val)
		default:
			return nil, p.unexpected()
		}
		if !p.acceptOp(",") {
			break
		}
	}
	st.Value = strings.Join(parts, ", ")
	return st, nil
}

func (p *Parser) parseExplain() (Stmt, error) {
	p.next()
	st := &ExplainStmt{Costs: true}
	if p.acceptOp("(") {
		for {
			opt, err := p.colLabel()
			if err != nil {
				return nil, err
			}
			on := true
			if t := p.peek(); t.Kind == TIdent && (t.Val == "true" || t.Val == "on" || t.Val == "false" || t.Val == "off") {
				on = t.Val == "true" || t.Val == "on"
				p.next()
			} else if t.Kind == TIdent && opt == "format" {
				p.next()
			}
			switch opt {
			case "analyze", "analyse":
				st.Analyze = on
			case "verbose":
				st.Verbose = on
			case "costs":
				st.Costs = on
			}
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
	} else {
		if p.acceptKw("analyze") || p.acceptKw("analyse") {
			st.Analyze = true
		}
		if p.acceptKw("verbose") {
			st.Verbose = true
		}
	}
	inner, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	st.Stmt = inner
	return st, nil
}

func (p *Parser) parseWithStatement() (Stmt, error) {
	with, err := p.parseWith()
	if err != nil {
		return nil, err
	}
	switch {
	case p.isKw("insert"):
		return p.parseInsert(with)
	case p.isKw("update"):
		return p.parseUpdate(with)
	case p.isKw("delete"):
		return p.parseDelete(with)
	}
	q, err := p.parseQueryNoWith()
	if err != nil {
		return nil, err
	}
	if q.With != nil {
		// A parenthesised inner WITH: wrap it.
		q = &SelectStmt{Targets: []*Target{{Star: true}}, From: []TableExpr{&SubqueryTable{Query: q, Alias: "q"}}}
	}
	q.With = with
	return q, nil
}

func (p *Parser) parseWith() (*With, error) {
	if err := p.expectKw("with"); err != nil {
		return nil, err
	}
	w := &With{}
	if p.acceptKw("recursive") {
		w.Recursive = true
	}
	for {
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		cte := &CTE{Name: name}
		if p.isOp("(") {
			cols, err := p.parenIdentList()
			if err != nil {
				return nil, err
			}
			cte.Columns = cols
		}
		if err := p.expectKw("as"); err != nil {
			return nil, err
		}
		p.acceptKw("not")
		p.acceptKw("materialized")
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		q, err := p.parseQuery()
		if err != nil {
			return nil, err
		}
		cte.Query = q
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		w.CTEs = append(w.CTEs, cte)
		if !p.acceptOp(",") {
			return w, nil
		}
	}
}

// parseQuery parses a full query including WITH, ORDER BY and LIMIT.
func (p *Parser) parseQuery() (*SelectStmt, error) {
	if p.isKw("with") {
		with, err := p.parseWith()
		if err != nil {
			return nil, err
		}
		q, err := p.parseQueryNoWith()
		if err != nil {
			return nil, err
		}
		if q.With != nil {
			q = &SelectStmt{Targets: []*Target{{Star: true}}, From: []TableExpr{&SubqueryTable{Query: q, Alias: "q"}}}
		}
		q.With = with
		return q, nil
	}
	return p.parseQueryNoWith()
}

func (p *Parser) parseQueryNoWith() (*SelectStmt, error) {
	q, err := p.parseSetOps()
	if err != nil {
		return nil, err
	}
	if p.isKw("order") || p.isKw("limit") || p.isKw("offset") || p.isKw("fetch") || p.isKw("for") {
		// ORDER BY/LIMIT apply to the whole set operation. If q already has
		// its own (from parentheses), wrap it.
		if q.OrderBy != nil || q.Limit != nil || q.Offset != nil {
			q = wrapQuery(q)
		}
		if err := p.parseOrderLimit(q); err != nil {
			return nil, err
		}
	}
	return q, nil
}

func wrapQuery(q *SelectStmt) *SelectStmt {
	return &SelectStmt{Targets: []*Target{{Star: true}}, From: []TableExpr{&SubqueryTable{Query: q, Alias: "subq"}}}
}

func (p *Parser) parseOrderLimit(q *SelectStmt) error {
	if p.acceptKw("order") {
		if err := p.expectKw("by"); err != nil {
			return err
		}
		items, err := p.parseOrderList()
		if err != nil {
			return err
		}
		q.OrderBy = items
	}
	for {
		switch {
		case p.acceptKw("limit"):
			if p.acceptKw("all") {
				continue
			}
			e, err := p.parseExpr()
			if err != nil {
				return err
			}
			q.Limit = e
		case p.acceptKw("offset"):
			e, err := p.parseExpr()
			if err != nil {
				return err
			}
			q.Offset = e
			if !p.acceptKw("rows") {
				p.acceptKw("row")
			}
		case p.acceptKw("fetch"):
			if !p.acceptKw("first") && !p.acceptKw("next") {
				return p.unexpected()
			}
			var e Expr = &Literal{Kind: LitInt, Val: "1"}
			if !p.isKw("row") && !p.isKw("rows") {
				var err error
				e, err = p.parseExpr()
				if err != nil {
					return err
				}
			}
			if !p.acceptKw("rows") && !p.acceptKw("row") {
				return p.unexpected()
			}
			if err := p.expectKw("only"); err != nil {
				return err
			}
			q.Limit = e
		case p.acceptKw("for"):
			if !p.acceptKw("update") && !p.acceptKw("share") && !p.acceptKw("no", "key", "update") && !p.acceptKw("key", "share") {
				return p.unexpected()
			}
			q.ForUpdate = true
			if p.acceptKw("of") {
				if _, err := p.identList(); err != nil {
					return err
				}
			}
			p.acceptKw("nowait")
			p.acceptKw("skip", "locked")
		default:
			return nil
		}
	}
}

func (p *Parser) parseOrderList() ([]*OrderItem, error) {
	var items []*OrderItem
	for {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		it := &OrderItem{Expr: e}
		if p.acceptKw("desc") {
			it.Desc = true
		} else {
			p.acceptKw("asc")
		}
		if p.acceptKw("nulls") {
			first := false
			if p.acceptKw("first") {
				first = true
			} else if err := p.expectKw("last"); err != nil {
				return nil, err
			}
			it.NullsFirst = &first
		}
		items = append(items, it)
		if !p.acceptOp(",") {
			return items, nil
		}
	}
}

// parseSetOps handles UNION/EXCEPT (lower precedence) over INTERSECT.
func (p *Parser) parseSetOps() (*SelectStmt, error) {
	left, err := p.parseIntersect()
	if err != nil {
		return nil, err
	}
	for p.isKw("union") || p.isKw("except") {
		op := SetUnion
		if p.next().Val == "except" {
			op = SetExcept
		}
		all := p.acceptKw("all")
		if !all {
			p.acceptKw("distinct")
		}
		right, err := p.parseIntersect()
		if err != nil {
			return nil, err
		}
		left = &SelectStmt{SetOp: op, SetAll: all, Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseIntersect() (*SelectStmt, error) {
	left, err := p.parseSelectPrimary()
	if err != nil {
		return nil, err
	}
	for p.isKw("intersect") {
		p.next()
		all := p.acceptKw("all")
		if !all {
			p.acceptKw("distinct")
		}
		right, err := p.parseSelectPrimary()
		if err != nil {
			return nil, err
		}
		left = &SelectStmt{SetOp: SetIntersect, SetAll: all, Left: left, Right: right}
	}
	return left, nil
}

func (p *Parser) parseSelectPrimary() (*SelectStmt, error) {
	switch {
	case p.isOp("("):
		p.next()
		q, err := p.parseQuery()
		if err != nil {
			return nil, err
		}
		return q, p.expectOp(")")
	case p.isKw("values"):
		p.next()
		q := &SelectStmt{}
		for {
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			row, err := p.parseExprListWithDefault()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			q.Values = append(q.Values, row)
			if !p.acceptOp(",") {
				return q, nil
			}
		}
	case p.isKw("table"):
		p.next()
		tn, err := p.qualifiedName()
		if err != nil {
			return nil, err
		}
		return &SelectStmt{Targets: []*Target{{Star: true}}, From: []TableExpr{tn}}, nil
	case p.isKw("select"):
		return p.parseSelectCore()
	}
	return nil, p.unexpected()
}

func (p *Parser) parseExprListWithDefault() ([]Expr, error) {
	var out []Expr
	for {
		if p.acceptKw("default") {
			out = append(out, &DefaultExpr{})
		} else {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		if !p.acceptOp(",") {
			return out, nil
		}
	}
}

func (p *Parser) parseSelectCore() (*SelectStmt, error) {
	if err := p.expectKw("select"); err != nil {
		return nil, err
	}
	q := &SelectStmt{}
	if p.acceptKw("distinct") {
		q.Distinct = true
		if p.acceptKw("on") {
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			l, err := p.parseExprList()
			if err != nil {
				return nil, err
			}
			q.DistinctOn = l
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
		}
	} else {
		p.acceptKw("all")
	}
	// The target list may be empty: SELECT FROM t, or SELECT;
	if !p.isKw("from") && !p.isOp(";") && !p.isOp(")") && p.peek().Kind != TEOF && !p.isKw("where") &&
		!p.isKw("union") && !p.isKw("except") && !p.isKw("intersect") {
		targets, err := p.parseTargets()
		if err != nil {
			return nil, err
		}
		q.Targets = targets
	}
	if p.acceptKw("into") {
		return nil, pgerr.Unsupported("SELECT INTO is not supported")
	}
	if p.acceptKw("from") {
		from, err := p.parseFromList()
		if err != nil {
			return nil, err
		}
		q.From = from
	}
	if p.acceptKw("where") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		q.Where = e
	}
	if p.acceptKw("group") {
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		p.acceptKw("all")
		l, err := p.parseExprList()
		if err != nil {
			return nil, err
		}
		q.GroupBy = l
	}
	if p.acceptKw("having") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		q.Having = e
	}
	if p.isKw("window") {
		return nil, pgerr.Unsupported("window functions are not supported")
	}
	return q, nil
}

func (p *Parser) parseTargets() ([]*Target, error) {
	var out []*Target
	for {
		pos := p.peek().Pos
		if p.isOp("*") {
			p.next()
			out = append(out, &Target{Star: true, Pos: pos})
		} else if (p.peek().Kind == TIdent || p.peek().Kind == TQuotedIdent) && p.peekN(1).Kind == TOp && p.peekN(1).Val == "." &&
			p.peekN(2).Kind == TOp && p.peekN(2).Val == "*" {
			tbl := p.next().Val
			p.next()
			p.next()
			out = append(out, &Target{Star: true, StarTable: tbl, Pos: pos})
		} else {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			t := &Target{Expr: e, Pos: pos}
			if p.acceptKw("as") {
				a, err := p.colLabel()
				if err != nil {
					return nil, err
				}
				t.Alias = a
			} else if p.canBeImplicitAlias() {
				t.Alias = p.next().Val
			}
			out = append(out, t)
		}
		if !p.acceptOp(",") {
			return out, nil
		}
	}
}

func (p *Parser) parseFromList() ([]TableExpr, error) {
	var out []TableExpr
	for {
		te, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		out = append(out, te)
		if !p.acceptOp(",") {
			return out, nil
		}
	}
}

func (p *Parser) parseTableRef() (TableExpr, error) {
	left, err := p.parseTablePrimary()
	if err != nil {
		return nil, err
	}
	for {
		natural := p.acceptKw("natural")
		var jt JoinType
		switch {
		case p.acceptKw("cross", "join"):
			jt = JoinCross
		case p.acceptKw("join"), p.acceptKw("inner", "join"):
			jt = JoinInner
		case p.acceptKw("left", "join"), p.acceptKw("left", "outer", "join"):
			jt = JoinLeft
		case p.acceptKw("right", "join"), p.acceptKw("right", "outer", "join"):
			jt = JoinRight
		case p.acceptKw("full", "join"), p.acceptKw("full", "outer", "join"):
			jt = JoinFull
		default:
			if natural {
				return nil, p.unexpected()
			}
			return left, nil
		}
		right, err := p.parseTablePrimary()
		if err != nil {
			return nil, err
		}
		j := &JoinExpr{Type: jt, Left: left, Right: right, Natural: natural}
		if jt != JoinCross && !natural {
			switch {
			case p.acceptKw("on"):
				e, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				j.On = e
			case p.acceptKw("using"):
				cols, err := p.parenIdentList()
				if err != nil {
					return nil, err
				}
				j.Using = cols
			default:
				return nil, p.unexpected()
			}
		}
		left = j
	}
}

func (p *Parser) parseAlias() (alias string, cols []string, err error) {
	if p.acceptKw("as") {
		alias, err = p.colLabel()
		if err != nil {
			return
		}
	} else if p.canBeImplicitAlias() {
		alias = p.next().Val
	} else {
		return "", nil, nil
	}
	if p.isOp("(") {
		cols, err = p.parenIdentList()
	}
	return
}

func (p *Parser) parseTablePrimary() (TableExpr, error) {
	lateral := p.acceptKw("lateral")
	if p.isOp("(") {
		// Either a subquery or a parenthesised join.
		save := p.pos
		p.next()
		if p.isKw("select") || p.isKw("values") || p.isKw("with") || p.isOp("(") && p.looksLikeQuery() {
			q, err := p.parseQuery()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			alias, cols, err := p.parseAlias()
			if err != nil {
				return nil, err
			}
			return &SubqueryTable{Query: q, Alias: alias, ColAliases: cols, Lateral: lateral}, nil
		}
		p.pos = save + 1
		te, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		if alias, _, _ := p.parseAlias(); alias != "" {
			return nil, pgerr.Unsupported("aliases on parenthesised joins are not supported")
		}
		return te, nil
	}
	save := p.pos
	tn, err := p.qualifiedName()
	if err != nil {
		return nil, err
	}
	if p.isOp("(") {
		// Table function such as generate_series(1, 10).
		p.pos = save
		e, err := p.parseNameOrCall()
		if err != nil {
			return nil, err
		}
		fc, ok := e.(*FuncCall)
		if !ok {
			return nil, p.unexpected()
		}
		alias, cols, err := p.parseAlias()
		if err != nil {
			return nil, err
		}
		return &FuncTable{Func: fc, Alias: alias, ColAliases: cols}, nil
	}
	alias, cols, err := p.parseAlias()
	if err != nil {
		return nil, err
	}
	tn.Alias, tn.ColAliases = alias, cols
	return tn, nil
}

// looksLikeQuery peeks past nested parentheses for SELECT/VALUES/WITH.
func (p *Parser) looksLikeQuery() bool {
	i := p.pos
	for i < len(p.toks) && p.toks[i].Kind == TOp && p.toks[i].Val == "(" {
		i++
	}
	if i >= len(p.toks) {
		return false
	}
	t := p.toks[i]
	return t.Kind == TIdent && (t.Val == "select" || t.Val == "values" || t.Val == "with")
}

// ---- DML ----

func (p *Parser) parseReturning() ([]*Target, error) {
	if !p.acceptKw("returning") {
		return nil, nil
	}
	return p.parseTargets()
}

func (p *Parser) parseInsert(with *With) (Stmt, error) {
	if err := p.expectKw("insert", "into"); err != nil {
		return nil, err
	}
	tn, err := p.qualifiedName()
	if err != nil {
		return nil, err
	}
	if p.acceptKw("as") {
		a, err := p.colLabel()
		if err != nil {
			return nil, err
		}
		tn.Alias = a
	}
	st := &InsertStmt{With: with, Table: tn}
	if p.isOp("(") && !p.looksLikeQueryAt(p.pos+1) {
		cols, err := p.parenIdentList()
		if err != nil {
			return nil, err
		}
		st.Columns = cols
	}
	if p.acceptKw("default", "values") {
		st.DefaultValues = true
	} else {
		p.acceptKw("overriding", "system", "value")
		p.acceptKw("overriding", "user", "value")
		q, err := p.parseQuery()
		if err != nil {
			return nil, err
		}
		st.Source = q
	}
	if p.acceptKw("on", "conflict") {
		oc := &OnConflict{}
		if p.isOp("(") {
			cols, err := p.parenIdentList()
			if err != nil {
				return nil, err
			}
			oc.Columns = cols
		} else if p.acceptKw("on", "constraint") {
			if _, err := p.ident(); err != nil {
				return nil, err
			}
		}
		if err := p.expectKw("do"); err != nil {
			return nil, err
		}
		if p.acceptKw("nothing") {
			oc.DoNothing = true
		} else {
			if err := p.expectKw("update", "set"); err != nil {
				return nil, err
			}
			set, err := p.parseSetList()
			if err != nil {
				return nil, err
			}
			oc.Set = set
			if p.acceptKw("where") {
				e, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				oc.Where = e
			}
		}
		st.OnConflict = oc
	}
	ret, err := p.parseReturning()
	if err != nil {
		return nil, err
	}
	st.Returning = ret
	return st, nil
}

func (p *Parser) looksLikeQueryAt(i int) bool {
	for i < len(p.toks) && p.toks[i].Kind == TOp && p.toks[i].Val == "(" {
		i++
	}
	if i >= len(p.toks) {
		return false
	}
	t := p.toks[i]
	return t.Kind == TIdent && (t.Val == "select" || t.Val == "values" || t.Val == "with")
}

func (p *Parser) parseSetList() ([]*SetClause, error) {
	var out []*SetClause
	for {
		if p.isOp("(") {
			cols, err := p.parenIdentList()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp("="); err != nil {
				return nil, err
			}
			p.acceptKw("row")
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			vals, err := p.parseExprListWithDefault()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			if len(vals) != len(cols) {
				return nil, pgerr.New(pgerr.SyntaxError, "number of columns does not match number of values")
			}
			for i := range cols {
				out = append(out, &SetClause{Column: cols[i], Value: vals[i]})
			}
		} else {
			col, err := p.colLabel()
			if err != nil {
				return nil, err
			}
			if p.acceptOp(".") { // table.col
				col, err = p.colLabel()
				if err != nil {
					return nil, err
				}
			}
			if err := p.expectOp("="); err != nil {
				return nil, err
			}
			var v Expr
			if p.acceptKw("default") {
				v = &DefaultExpr{}
			} else {
				v, err = p.parseExpr()
				if err != nil {
					return nil, err
				}
			}
			out = append(out, &SetClause{Column: col, Value: v})
		}
		if !p.acceptOp(",") {
			return out, nil
		}
	}
}

func (p *Parser) parseUpdate(with *With) (Stmt, error) {
	if err := p.expectKw("update"); err != nil {
		return nil, err
	}
	p.acceptKw("only")
	tn, err := p.qualifiedName()
	if err != nil {
		return nil, err
	}
	if p.acceptKw("as") {
		if tn.Alias, err = p.colLabel(); err != nil {
			return nil, err
		}
	} else if p.canBeImplicitAlias() {
		tn.Alias = p.next().Val
	}
	if err := p.expectKw("set"); err != nil {
		return nil, err
	}
	set, err := p.parseSetList()
	if err != nil {
		return nil, err
	}
	st := &UpdateStmt{With: with, Table: tn, Set: set}
	if p.acceptKw("from") {
		if st.From, err = p.parseFromList(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("where") {
		if p.acceptKw("current", "of") {
			return nil, pgerr.Unsupported("WHERE CURRENT OF is not supported")
		}
		if st.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	if st.Returning, err = p.parseReturning(); err != nil {
		return nil, err
	}
	return st, nil
}

func (p *Parser) parseDelete(with *With) (Stmt, error) {
	if err := p.expectKw("delete", "from"); err != nil {
		return nil, err
	}
	p.acceptKw("only")
	tn, err := p.qualifiedName()
	if err != nil {
		return nil, err
	}
	if p.acceptKw("as") {
		if tn.Alias, err = p.colLabel(); err != nil {
			return nil, err
		}
	} else if p.canBeImplicitAlias() {
		tn.Alias = p.next().Val
	}
	st := &DeleteStmt{With: with, Table: tn}
	if p.acceptKw("using") {
		if st.Using, err = p.parseFromList(); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("where") {
		if st.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	if st.Returning, err = p.parseReturning(); err != nil {
		return nil, err
	}
	return st, nil
}

// ---- DDL ----

func (p *Parser) parseCreate() (Stmt, error) {
	p.next() // CREATE
	p.acceptKw("or", "replace")
	if p.acceptKw("temp") || p.acceptKw("temporary") {
		return nil, pgerr.Unsupported("temporary tables are not supported")
	}
	p.acceptKw("unlogged")
	switch {
	case p.acceptKw("table"):
		return p.parseCreateTable()
	case p.isKw("unique") || p.isKw("index"):
		unique := p.acceptKw("unique")
		if err := p.expectKw("index"); err != nil {
			return nil, err
		}
		p.acceptKw("concurrently")
		st := &CreateIndexStmt{Unique: unique}
		if p.acceptKw("if", "not", "exists") {
			st.IfNotExists = true
		}
		if !p.isKw("on") {
			name, err := p.ident()
			if err != nil {
				return nil, err
			}
			st.Name = name
		}
		if err := p.expectKw("on"); err != nil {
			return nil, err
		}
		p.acceptKw("only")
		tn, err := p.qualifiedName()
		if err != nil {
			return nil, err
		}
		st.Table = tn
		if p.acceptKw("using") {
			m, err := p.ident()
			if err != nil {
				return nil, err
			}
			st.Using = m
		}
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		for {
			if p.isOp("(") {
				return nil, pgerr.Unsupported("expression indexes are not supported")
			}
			col, err := p.colLabel()
			if err != nil {
				return nil, err
			}
			if p.isOp("(") {
				return nil, pgerr.Unsupported("expression indexes are not supported")
			}
			el := &IndexElem{Column: col}
			if p.acceptKw("desc") {
				el.Desc = true
			} else {
				p.acceptKw("asc")
			}
			if p.acceptKw("nulls") {
				if !p.acceptKw("first") {
					p.acceptKw("last")
				}
			}
			st.Columns = append(st.Columns, el)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		if p.isKw("where") {
			return nil, pgerr.Unsupported("partial indexes are not supported")
		}
		return st, nil
	case p.acceptKw("sequence"):
		st := &CreateSequenceStmt{Start: 1, Increment: 1}
		if p.acceptKw("if", "not", "exists") {
			st.IfNotExists = true
		}
		tn, err := p.qualifiedName()
		if err != nil {
			return nil, err
		}
		st.Name = tn
		for {
			switch {
			case p.acceptKw("start"):
				p.acceptKw("with")
				n, err := p.signedInt()
				if err != nil {
					return nil, err
				}
				st.Start = n
			case p.acceptKw("increment"):
				p.acceptKw("by")
				n, err := p.signedInt()
				if err != nil {
					return nil, err
				}
				st.Increment = n
			case p.acceptKw("as"):
				if _, err := p.parseTypeName(); err != nil {
					return nil, err
				}
			case p.acceptKw("no", "minvalue"), p.acceptKw("no", "maxvalue"), p.acceptKw("no", "cycle"):
			case p.acceptKw("minvalue"), p.acceptKw("maxvalue"), p.acceptKw("cache"):
				if _, err := p.signedInt(); err != nil {
					return nil, err
				}
			default:
				return st, nil
			}
		}
	case p.isKw("view"), p.isKw("function"), p.isKw("trigger"), p.isKw("schema"), p.isKw("database"), p.isKw("type"), p.isKw("extension"), p.isKw("materialized"):
		return nil, pgerr.Unsupported("CREATE %s is not supported", strings.ToUpper(p.peek().Val))
	}
	return nil, p.unexpected()
}

func (p *Parser) signedInt() (int64, error) {
	neg := p.acceptOp("-")
	t := p.peek()
	if t.Kind != TNumber {
		return 0, p.unexpected()
	}
	p.next()
	n, err := strconv.ParseInt(t.Val, 10, 64)
	if err != nil {
		return 0, p.unexpected()
	}
	if neg {
		n = -n
	}
	return n, nil
}

func (p *Parser) parseCreateTable() (Stmt, error) {
	st := &CreateTableStmt{}
	if p.acceptKw("if", "not", "exists") {
		st.IfNotExists = true
	}
	tn, err := p.qualifiedName()
	if err != nil {
		return nil, err
	}
	st.Table = tn
	if p.acceptKw("as") {
		q, err := p.parseQuery()
		if err != nil {
			return nil, err
		}
		st.AsSelect = q
		return st, nil
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	if !p.isOp(")") {
		for {
			if p.isKw("constraint") || p.isKw("primary") || p.isKw("unique") || p.isKw("check") || p.isKw("foreign") {
				c, err := p.parseTableConstraint()
				if err != nil {
					return nil, err
				}
				st.Constraints = append(st.Constraints, c)
			} else if p.isKw("like") {
				return nil, pgerr.Unsupported("CREATE TABLE ... LIKE is not supported")
			} else {
				cd, err := p.parseColumnDef()
				if err != nil {
					return nil, err
				}
				st.Columns = append(st.Columns, cd)
			}
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	if p.isKw("inherits") || p.isKw("partition") {
		return nil, pgerr.Unsupported("table inheritance and partitioning are not supported")
	}
	if p.acceptKw("with") {
		if err := p.skipParens(); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (p *Parser) skipParens() error {
	if err := p.expectOp("("); err != nil {
		return err
	}
	depth := 1
	for depth > 0 {
		t := p.next()
		switch {
		case t.Kind == TEOF:
			return p.unexpected()
		case t.Kind == TOp && t.Val == "(":
			depth++
		case t.Kind == TOp && t.Val == ")":
			depth--
		}
	}
	return nil
}

func (p *Parser) parseColumnDef() (*ColumnDef, error) {
	name, err := p.colLabel()
	if err != nil {
		return nil, err
	}
	typ, err := p.parseTypeName()
	if err != nil {
		return nil, err
	}
	cd := &ColumnDef{Name: name, Type: typ}
	for {
		conName := ""
		if p.acceptKw("constraint") {
			if conName, err = p.ident(); err != nil {
				return nil, err
			}
		}
		switch {
		case p.acceptKw("not", "null"):
			cd.NotNull = true
		case p.acceptKw("null"):
		case p.acceptKw("default"):
			start := p.peek().Pos
			e, err := p.parseBinary(precCompare + 1)
			if err != nil {
				return nil, err
			}
			cd.Default = e
			cd.DefaultSQL = strings.TrimSpace(p.src[start:p.peek().Pos])
		case p.acceptKw("primary", "key"):
			cd.Constraints = append(cd.Constraints, &Constraint{Name: conName, Kind: ConPrimaryKey})
		case p.acceptKw("unique"):
			cd.Constraints = append(cd.Constraints, &Constraint{Name: conName, Kind: ConUnique})
		case p.isKw("check"):
			c, err := p.parseCheck(conName)
			if err != nil {
				return nil, err
			}
			cd.Constraints = append(cd.Constraints, c)
		case p.acceptKw("references"):
			c := &Constraint{Name: conName, Kind: ConForeignKey, Columns: []string{name}}
			if err := p.parseReferences(c); err != nil {
				return nil, err
			}
			cd.Constraints = append(cd.Constraints, c)
		case p.acceptKw("generated"):
			if !p.acceptKw("always") {
				if err := p.expectKw("by", "default"); err != nil {
					return nil, err
				}
			}
			if p.acceptKw("as", "identity") {
				if p.isOp("(") {
					if err := p.skipParens(); err != nil {
						return nil, err
					}
				}
				cd.Identity = true
			} else {
				return nil, pgerr.Unsupported("generated columns are not supported")
			}
		case p.acceptKw("collate"):
			if _, err := p.qualifiedName(); err != nil {
				return nil, err
			}
		default:
			if conName != "" {
				return nil, p.unexpected()
			}
			return cd, nil
		}
	}
}

func (p *Parser) parseCheck(name string) (*Constraint, error) {
	if err := p.expectKw("check"); err != nil {
		return nil, err
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	start := p.peek().Pos
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	end := p.peek().Pos
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	p.acceptKw("no", "inherit")
	return &Constraint{Name: name, Kind: ConCheck, Check: e, CheckSQL: strings.TrimSpace(p.src[start:end])}, nil
}

func (p *Parser) parseReferences(c *Constraint) error {
	tn, err := p.qualifiedName()
	if err != nil {
		return err
	}
	c.RefTable = tn
	if p.isOp("(") {
		if c.RefCols, err = p.parenIdentList(); err != nil {
			return err
		}
	}
	p.acceptKw("match", "simple")
	p.acceptKw("match", "full")
	for {
		switch {
		case p.acceptKw("on", "delete"):
			a, err := p.refAction()
			if err != nil {
				return err
			}
			c.OnDelete = a
		case p.acceptKw("on", "update"):
			a, err := p.refAction()
			if err != nil {
				return err
			}
			c.OnUpdate = a
		case p.acceptKw("deferrable"), p.acceptKw("not", "deferrable"), p.acceptKw("initially", "immediate"):
		case p.acceptKw("initially", "deferred"):
			return pgerr.Unsupported("deferred constraints are not supported")
		default:
			return nil
		}
	}
}

func (p *Parser) refAction() (string, error) {
	switch {
	case p.acceptKw("cascade"):
		return "CASCADE", nil
	case p.acceptKw("restrict"):
		return "RESTRICT", nil
	case p.acceptKw("no", "action"):
		return "NO ACTION", nil
	case p.acceptKw("set", "null"):
		return "SET NULL", nil
	case p.acceptKw("set", "default"):
		return "SET DEFAULT", nil
	}
	return "", p.unexpected()
}

func (p *Parser) parseTableConstraint() (*Constraint, error) {
	name := ""
	var err error
	if p.acceptKw("constraint") {
		if name, err = p.ident(); err != nil {
			return nil, err
		}
	}
	switch {
	case p.acceptKw("primary", "key"):
		cols, err := p.parenIdentList()
		if err != nil {
			return nil, err
		}
		return &Constraint{Name: name, Kind: ConPrimaryKey, Columns: cols}, nil
	case p.acceptKw("unique"):
		p.acceptKw("nulls", "distinct")
		cols, err := p.parenIdentList()
		if err != nil {
			return nil, err
		}
		return &Constraint{Name: name, Kind: ConUnique, Columns: cols}, nil
	case p.isKw("check"):
		return p.parseCheck(name)
	case p.acceptKw("foreign", "key"):
		cols, err := p.parenIdentList()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("references"); err != nil {
			return nil, err
		}
		c := &Constraint{Name: name, Kind: ConForeignKey, Columns: cols}
		return c, p.parseReferences(c)
	}
	return nil, p.unexpected()
}

// parseTypeName parses a SQL type name such as "double precision",
// "varchar(20)", "numeric(10,2)", "timestamp with time zone" or "int[]".
func (p *Parser) parseTypeName() (*TypeName, error) {
	t := p.peek()
	if t.Kind != TIdent && t.Kind != TQuotedIdent {
		return nil, p.unexpected()
	}
	p.next()
	name := t.Val
	if t.Kind == TQuotedIdent && name == "char" {
		name = `"char"`
	}
	if p.isOp(".") { // pg_catalog.int4
		p.next()
		n2, err := p.colLabel()
		if err != nil {
			return nil, err
		}
		if t.Val != "pg_catalog" && t.Val != "public" {
			return nil, pgerr.New(pgerr.UndefinedObject, "type \"%s.%s\" does not exist", t.Val, n2)
		}
		name = n2
	}
	switch name {
	case "double":
		if err := p.expectKw("precision"); err != nil {
			return nil, err
		}
		name = "double precision"
	case "character", "char", "national":
		if name == "national" {
			if !p.acceptKw("character") {
				p.acceptKw("char")
			}
		}
		if p.acceptKw("varying") {
			name = "varchar"
		} else {
			name = "bpchar"
		}
	case "timestamp", "time":
		if p.isOp("(") {
			if _, err := p.typeMods(); err != nil {
				return nil, err
			}
		}
		if p.acceptKw("with", "time", "zone") {
			name += "tz"
		} else {
			p.acceptKw("without", "time", "zone")
		}
	case "interval":
		for _, f := range []string{"year", "month", "day", "hour", "minute", "second"} {
			if p.acceptKw(f) {
				if p.acceptKw("to") {
					p.next()
				}
				break
			}
		}
	case "bit":
		return nil, pgerr.Unsupported("type bit is not supported")
	}
	tn := &TypeName{Name: name}
	if p.isOp("(") {
		mods, err := p.typeMods()
		if err != nil {
			return nil, err
		}
		tn.Mods = mods
	}
	if name == "bpchar" && tn.Mods == nil && t.Val != "bpchar" {
		tn.Mods = []int{1} // char without length means char(1)
	}
	for p.isOp("[") {
		p.next()
		if p.peek().Kind == TNumber {
			p.next()
		}
		if err := p.expectOp("]"); err != nil {
			return nil, err
		}
		tn.Array = true
	}
	if p.acceptKw("array") {
		tn.Array = true
	}
	return tn, nil
}

func (p *Parser) typeMods() ([]int, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var mods []int
	for {
		t := p.peek()
		if t.Kind != TNumber {
			return nil, p.unexpected()
		}
		p.next()
		n, err := strconv.Atoi(t.Val)
		if err != nil {
			return nil, p.unexpected()
		}
		mods = append(mods, n)
		if !p.acceptOp(",") {
			break
		}
	}
	return mods, p.expectOp(")")
}

func (p *Parser) parseDrop() (Stmt, error) {
	p.next()
	st := &DropStmt{}
	switch {
	case p.acceptKw("table"):
		st.Kind = DropTable
	case p.acceptKw("index"):
		st.Kind = DropIndex
		p.acceptKw("concurrently")
	case p.acceptKw("sequence"):
		st.Kind = DropSequence
	case p.acceptKw("view"):
		st.Kind = DropView
	default:
		return nil, p.unexpected()
	}
	if p.acceptKw("if", "exists") {
		st.IfExists = true
	}
	for {
		tn, err := p.qualifiedName()
		if err != nil {
			return nil, err
		}
		st.Names = append(st.Names, tn)
		if !p.acceptOp(",") {
			break
		}
	}
	if p.acceptKw("cascade") {
		st.Cascade = true
	} else {
		p.acceptKw("restrict")
	}
	return st, nil
}

func (p *Parser) parseAlter() (Stmt, error) {
	p.next()
	if !p.acceptKw("table") {
		return nil, pgerr.Unsupported("only ALTER TABLE is supported")
	}
	st := &AlterTableStmt{}
	if p.acceptKw("if", "exists") {
		st.IfExists = true
	}
	p.acceptKw("only")
	tn, err := p.qualifiedName()
	if err != nil {
		return nil, err
	}
	st.Table = tn
	switch {
	case p.acceptKw("add"):
		if p.isKw("constraint") || p.isKw("primary") || p.isKw("unique") || p.isKw("check") || p.isKw("foreign") {
			c, err := p.parseTableConstraint()
			if err != nil {
				return nil, err
			}
			st.AddConstraint = c
			return st, nil
		}
		p.acceptKw("column")
		p.acceptKw("if", "not", "exists")
		cd, err := p.parseColumnDef()
		if err != nil {
			return nil, err
		}
		st.AddColumn = cd
	case p.acceptKw("drop"):
		p.acceptKw("column")
		p.acceptKw("if", "exists")
		col, err := p.colLabel()
		if err != nil {
			return nil, err
		}
		st.DropColumn = col
		p.acceptKw("cascade")
		p.acceptKw("restrict")
	case p.acceptKw("rename"):
		if p.acceptKw("to") {
			n, err := p.ident()
			if err != nil {
				return nil, err
			}
			st.RenameTo = n
		} else {
			p.acceptKw("column")
			from, err := p.colLabel()
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("to"); err != nil {
				return nil, err
			}
			to, err := p.colLabel()
			if err != nil {
				return nil, err
			}
			st.RenameCol = [2]string{from, to}
		}
	default:
		return nil, pgerr.Unsupported("this form of ALTER TABLE is not supported")
	}
	return st, nil
}

func (p *Parser) parseCopy() (Stmt, error) {
	p.next()
	st := &CopyStmt{Format: "text", Delim: "\t"}
	if p.isOp("(") {
		p.next()
		q, err := p.parseQuery()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		st.Query = q
	} else {
		tn, err := p.qualifiedName()
		if err != nil {
			return nil, err
		}
		st.Table = tn
		if p.isOp("(") {
			cols, err := p.parenIdentList()
			if err != nil {
				return nil, err
			}
			st.Columns = cols
		}
	}
	switch {
	case p.acceptKw("from"):
		st.From = true
		if err := p.expectKw("stdin"); err != nil {
			return nil, pgerr.Unsupported("COPY FROM a file is not supported; use FROM STDIN")
		}
	case p.acceptKw("to"):
		if err := p.expectKw("stdout"); err != nil {
			return nil, pgerr.Unsupported("COPY TO a file is not supported; use TO STDOUT")
		}
	default:
		return nil, p.unexpected()
	}
	p.acceptKw("with")
	parseOpt := func() error {
		switch {
		case p.acceptKw("format"):
			f, err := p.colLabel()
			if err != nil {
				return err
			}
			st.Format = f
		case p.acceptKw("csv"):
			st.Format = "csv"
		case p.acceptKw("header"):
			st.Header = true
			if p.isKw("true") || p.isKw("on") {
				p.next()
			} else if p.isKw("false") || p.isKw("off") {
				p.next()
				st.Header = false
			}
		case p.acceptKw("delimiter"):
			p.acceptKw("as")
			t := p.next()
			if t.Kind != TString {
				return p.unexpected()
			}
			st.Delim = t.Val
		case p.acceptKw("null"):
			p.acceptKw("as")
			p.next()
		default:
			return p.unexpected()
		}
		return nil
	}
	if p.acceptOp("(") {
		for {
			if err := parseOpt(); err != nil {
				return nil, err
			}
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
	} else {
		for p.peek().Kind == TIdent && !p.isKw("where") {
			if err := parseOpt(); err != nil {
				return nil, err
			}
		}
	}
	if st.Format == "csv" && st.Delim == "\t" {
		st.Delim = ","
	}
	return st, nil
}

// ---- expressions ----

// Operator precedence levels (higher binds tighter).
const (
	precOr      = 1
	precAnd     = 2
	precNot     = 3
	precIs      = 4
	precCompare = 5
	precLike    = 6 // BETWEEN, IN, LIKE, ILIKE, SIMILAR
	precOther   = 7 // ||, ~, and other operators
	precAdd     = 8
	precMul     = 9
	precExp     = 10
	precUnary   = 11
	precAt      = 12
	precCollate = 13
	precPostfix = 14 // :: and []
)

func (p *Parser) parseExpr() (Expr, error) { return p.parseBinary(0) }

func (p *Parser) parseExprList() ([]Expr, error) {
	var out []Expr
	for {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		if !p.acceptOp(",") {
			return out, nil
		}
	}
}

func isCompareOp(op string) bool {
	switch op {
	case "=", "<", ">", "<=", ">=", "<>", "!=":
		return true
	}
	return false
}

func (p *Parser) parseBinary(minPrec int) (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		pos := t.Pos
		switch {
		case t.Kind == TIdent && t.Val == "or":
			if precOr < minPrec {
				return left, nil
			}
			p.next()
			right, err := p.parseBinary(precOr + 1)
			if err != nil {
				return nil, err
			}
			left = &BinaryExpr{Op: "OR", Left: left, Right: right, Pos: pos}
		case t.Kind == TIdent && t.Val == "and":
			if precAnd < minPrec {
				return left, nil
			}
			p.next()
			right, err := p.parseBinary(precAnd + 1)
			if err != nil {
				return nil, err
			}
			left = &BinaryExpr{Op: "AND", Left: left, Right: right, Pos: pos}
		case t.Kind == TIdent && (t.Val == "isnull" || t.Val == "notnull"):
			if precIs < minPrec {
				return left, nil
			}
			p.next()
			left = &IsExpr{Expr: left, What: IsNull, Not: t.Val == "notnull"}
		case t.Kind == TIdent && t.Val == "is":
			if precIs < minPrec {
				return left, nil
			}
			p.next()
			not := p.acceptKw("not")
			switch {
			case p.acceptKw("null"):
				left = &IsExpr{Expr: left, What: IsNull, Not: not}
			case p.acceptKw("true"):
				left = &IsExpr{Expr: left, What: IsTrue, Not: not}
			case p.acceptKw("false"):
				left = &IsExpr{Expr: left, What: IsFalse, Not: not}
			case p.acceptKw("unknown"):
				left = &IsExpr{Expr: left, What: IsUnknown, Not: not}
			case p.acceptKw("distinct", "from"):
				right, err := p.parseBinary(precIs + 1)
				if err != nil {
					return nil, err
				}
				left = &IsDistinctExpr{Left: left, Right: right, Not: not}
			default:
				return nil, p.unexpected()
			}
		case t.Kind == TIdent && (t.Val == "between" || t.Val == "in" || t.Val == "like" || t.Val == "ilike" || t.Val == "similar" ||
			(t.Val == "not" && (p.isKwN(1, "between") || p.isKwN(1, "in") || p.isKwN(1, "like") || p.isKwN(1, "ilike") || p.isKwN(1, "similar")))):
			if precLike < minPrec {
				return left, nil
			}
			not := p.acceptKw("not")
			kw := p.next().Val
			switch kw {
			case "between":
				sym := p.acceptKw("symmetric")
				if !sym {
					p.acceptKw("asymmetric")
				}
				low, err := p.parseBinary(precLike + 1)
				if err != nil {
					return nil, err
				}
				if err := p.expectKw("and"); err != nil {
					return nil, err
				}
				high, err := p.parseBinary(precLike + 1)
				if err != nil {
					return nil, err
				}
				left = &BetweenExpr{Expr: left, Low: low, High: high, Not: not, Symmetric: sym}
			case "in":
				if err := p.expectOp("("); err != nil {
					return nil, err
				}
				if p.looksLikeQuery() {
					q, err := p.parseQuery()
					if err != nil {
						return nil, err
					}
					left = &InExpr{Expr: left, Subquery: q, Not: not}
				} else {
					l, err := p.parseExprList()
					if err != nil {
						return nil, err
					}
					left = &InExpr{Expr: left, List: l, Not: not}
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
			case "like", "ilike":
				pat, err := p.parseBinary(precLike + 1)
				if err != nil {
					return nil, err
				}
				le := &LikeExpr{Expr: left, Pattern: pat, Not: not, ILike: kw == "ilike"}
				if p.acceptKw("escape") {
					esc, err := p.parseBinary(precLike + 1)
					if err != nil {
						return nil, err
					}
					le.Escape = esc
				}
				left = le
			case "similar":
				return nil, pgerr.Unsupported("SIMILAR TO is not supported")
			}
		case t.Kind == TIdent && t.Val == "collate":
			p.next()
			if _, err := p.qualifiedName(); err != nil {
				return nil, err
			}
		case t.Kind == TIdent && t.Val == "at" && p.isKwN(1, "time") && p.isKwN(2, "zone"):
			if precAt < minPrec {
				return left, nil
			}
			p.pos += 3
			zone, err := p.parseBinary(precAt + 1)
			if err != nil {
				return nil, err
			}
			left = &FuncCall{Name: "timezone", Args: []Expr{zone, left}, Pos: pos}
		case t.Kind == TIdent && t.Val == "operator" && p.peekN(1).Kind == TOp && p.peekN(1).Val == "(":
			// OPERATOR(pg_catalog.~) — psql uses this form.
			if precOther < minPrec {
				return left, nil
			}
			p.pos += 2
			if p.peek().Kind == TIdent && p.peekN(1).Val == "." {
				p.pos += 2
			}
			op := p.next()
			if op.Kind != TOp {
				return nil, p.unexpected()
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			prec := opPrec(op.Val)
			right, err := p.parseBinary(prec + 1)
			if err != nil {
				return nil, err
			}
			left = &BinaryExpr{Op: op.Val, Left: left, Right: right, Pos: pos}
		case t.Kind == TOp:
			op := t.Val
			switch op {
			case "::":
				p.next()
				typ, err := p.parseTypeName()
				if err != nil {
					return nil, err
				}
				left = &CastExpr{Expr: left, Type: typ, Pos: pos}
				continue
			case "[":
				p.next()
				idx, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				if p.acceptOp(":") {
					return nil, pgerr.Unsupported("array slices are not supported")
				}
				if err := p.expectOp("]"); err != nil {
					return nil, err
				}
				left = &SubscriptExpr{Expr: left, Index: idx}
				continue
			case ")", ",", ";", "]", "(", ".":
				return left, nil
			}
			prec := opPrec(op)
			if prec < minPrec {
				return left, nil
			}
			p.next()
			if isCompareOp(op) && (p.isKw("any") || p.isKw("some") || p.isKw("all")) {
				all := p.next().Val == "all"
				if err := p.expectOp("("); err != nil {
					return nil, err
				}
				ae := &AnyAllExpr{Left: left, Op: op, All: all}
				if p.looksLikeQuery() {
					q, err := p.parseQuery()
					if err != nil {
						return nil, err
					}
					ae.Subquery = q
				} else {
					r, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					ae.Right = r
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				left = ae
				continue
			}
			nextMin := prec + 1
			if op == "^" {
				nextMin = prec + 1
			}
			right, err := p.parseBinary(nextMin)
			if err != nil {
				return nil, err
			}
			if op == "!=" {
				op = "<>"
			}
			left = &BinaryExpr{Op: op, Left: left, Right: right, Pos: pos}
		default:
			return left, nil
		}
	}
}

func opPrec(op string) int {
	switch op {
	case "=", "<", ">", "<=", ">=", "<>", "!=":
		return precCompare
	case "+", "-":
		return precAdd
	case "*", "/", "%":
		return precMul
	case "^":
		return precExp
	}
	return precOther
}

func (p *Parser) parseUnary() (Expr, error) {
	t := p.peek()
	switch {
	case t.Kind == TIdent && t.Val == "not":
		p.next()
		e, err := p.parseBinary(precNot)
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: "NOT", Expr: e, Pos: t.Pos}, nil
	case t.Kind == TOp && t.Val == "-":
		p.next()
		// Fold the sign into a numeric literal, as PostgreSQL does, so that
		// -9223372036854775808 is a valid bigint.
		if n := p.peek(); n.Kind == TNumber {
			e, err := p.parseBinary(precUnary)
			if err != nil {
				return nil, err
			}
			if lit, ok := e.(*Literal); ok {
				return makeNumberLiteral("-"+lit.Val, t.Pos), nil
			}
			return &UnaryExpr{Op: "-", Expr: e, Pos: t.Pos}, nil
		}
		e, err := p.parseBinary(precUnary)
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: "-", Expr: e, Pos: t.Pos}, nil
	case t.Kind == TOp && t.Val == "+":
		p.next()
		return p.parseBinary(precUnary)
	}
	return p.parsePrimary()
}

func makeNumberLiteral(s string, pos int) *Literal {
	if !strings.ContainsAny(s, ".eE") {
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			return &Literal{Kind: LitInt, Val: s, Pos: pos}
		}
	}
	return &Literal{Kind: LitNumeric, Val: s, Pos: pos}
}

// typeLiteralNames are type names that may prefix a string literal.
var typeLiteralNames = map[string]bool{
	"date": true, "timestamp": true, "timestamptz": true, "time": true, "interval": true, "int": true,
	"int2": true, "int4": true, "int8": true, "integer": true, "bigint": true, "smallint": true,
	"numeric": true, "decimal": true, "float4": true, "float8": true, "real": true, "double": true,
	"bool": true, "boolean": true, "text": true, "varchar": true, "char": true, "character": true,
	"bytea": true, "name": true, "oid": true, "regclass": true, "regtype": true, "json": true, "jsonb": true,
}

func (p *Parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.Kind {
	case TNumber:
		p.next()
		return makeNumberLiteral(t.Val, t.Pos), nil
	case TString:
		p.next()
		return &Literal{Kind: LitString, Val: t.Val, Pos: t.Pos}, nil
	case TParam:
		p.next()
		n, err := strconv.Atoi(t.Val)
		if err != nil || n < 1 || n > math.MaxUint16 {
			return nil, syntaxErr(t.Pos, "invalid parameter number $%s", t.Val)
		}
		return &ParamRef{N: n, Pos: t.Pos}, nil
	case TOp:
		if t.Val == "(" {
			p.next()
			if p.looksLikeQuery() {
				q, err := p.parseQuery()
				if err != nil {
					return nil, err
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				return &SubqueryExpr{Query: q, Pos: t.Pos}, nil
			}
			l, err := p.parseExprList()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			if len(l) == 1 {
				return l[0], nil
			}
			return &RowExpr{Exprs: l}, nil
		}
		if t.Val == "*" {
			p.next()
			return &StarExpr{}, nil
		}
		return nil, p.unexpected()
	case TQuotedIdent:
		return p.parseNameOrCall()
	case TIdent:
	default:
		return nil, p.unexpected()
	}
	// Keywords that start expressions.
	switch t.Val {
	case "null":
		p.next()
		return &Literal{Kind: LitNull, Pos: t.Pos}, nil
	case "true", "false":
		p.next()
		return &Literal{Kind: LitBool, Val: t.Val, Pos: t.Pos}, nil
	case "default":
		p.next()
		return &DefaultExpr{}, nil
	case "case":
		return p.parseCase()
	case "cast":
		p.next()
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("as"); err != nil {
			return nil, err
		}
		typ, err := p.parseTypeName()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return &CastExpr{Expr: e, Type: typ, Pos: t.Pos}, nil
	case "exists":
		p.next()
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		q, err := p.parseQuery()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return &ExistsExpr{Query: q}, nil
	case "array":
		p.next()
		if p.acceptOp("(") {
			q, err := p.parseQuery()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return &ArrayExpr{Subquery: q}, nil
		}
		if err := p.expectOp("["); err != nil {
			return nil, err
		}
		var elems []Expr
		if !p.isOp("]") {
			l, err := p.parseExprList()
			if err != nil {
				return nil, err
			}
			elems = l
		}
		if err := p.expectOp("]"); err != nil {
			return nil, err
		}
		return &ArrayExpr{Elems: elems}, nil
	case "row":
		if p.peekN(1).Kind == TOp && p.peekN(1).Val == "(" {
			p.next()
			p.next()
			var l []Expr
			if !p.isOp(")") {
				var err error
				if l, err = p.parseExprList(); err != nil {
					return nil, err
				}
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return &RowExpr{Exprs: l}, nil
		}
	case "current_date", "current_timestamp", "localtimestamp", "current_time", "localtime", "now":
		if t.Val != "now" || !(p.peekN(1).Kind == TOp && p.peekN(1).Val == "(") {
			if t.Val != "now" {
				p.next()
				if p.isOp("(") { // current_timestamp(3)
					if _, err := p.typeMods(); err != nil {
						return nil, err
					}
				}
				return &FuncCall{Name: t.Val, Pos: t.Pos}, nil
			}
		}
	case "current_user", "session_user", "user", "current_role", "current_catalog", "current_schema":
		if !(p.peekN(1).Kind == TOp && p.peekN(1).Val == "(") || t.Val == "current_user" || t.Val == "session_user" || t.Val == "user" {
			p.next()
			if t.Val == "current_schema" && p.isOp("(") {
				p.next()
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
			}
			return &FuncCall{Name: t.Val, Pos: t.Pos}, nil
		}
	case "extract":
		if p.peekN(1).Kind == TOp && p.peekN(1).Val == "(" {
			p.pos += 2
			ft := p.next()
			field := ft.Val
			if ft.Kind != TIdent && ft.Kind != TString {
				return nil, p.unexpected()
			}
			if err := p.expectKw("from"); err != nil {
				return nil, err
			}
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return &FuncCall{Name: "extract", Args: []Expr{&Literal{Kind: LitString, Val: strings.ToLower(field)}, e}, Pos: t.Pos}, nil
		}
	case "position":
		if p.peekN(1).Kind == TOp && p.peekN(1).Val == "(" {
			p.pos += 2
			sub, err := p.parseBinary(precLike + 1)
			if err != nil {
				return nil, err
			}
			if err := p.expectKw("in"); err != nil {
				return nil, err
			}
			s, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return &FuncCall{Name: "position", Args: []Expr{s, sub}, Pos: t.Pos}, nil
		}
	case "substring":
		if p.peekN(1).Kind == TOp && p.peekN(1).Val == "(" {
			save := p.pos
			p.pos += 2
			s, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if p.isKw("from") || p.isKw("for") {
				args := []Expr{s}
				var from, forE Expr
				if p.acceptKw("from") {
					if from, err = p.parseExpr(); err != nil {
						return nil, err
					}
				}
				if p.acceptKw("for") {
					if forE, err = p.parseExpr(); err != nil {
						return nil, err
					}
				}
				if from == nil {
					from = &Literal{Kind: LitInt, Val: "1"}
				}
				args = append(args, from)
				if forE != nil {
					args = append(args, forE)
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				return &FuncCall{Name: "substring", Args: args, Pos: t.Pos}, nil
			}
			p.pos = save
		}
	case "trim":
		if p.peekN(1).Kind == TOp && p.peekN(1).Val == "(" {
			p.pos += 2
			mode := "btrim"
			switch {
			case p.acceptKw("both"):
			case p.acceptKw("leading"):
				mode = "ltrim"
			case p.acceptKw("trailing"):
				mode = "rtrim"
			}
			var chars Expr
			if !p.isKw("from") {
				e, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				chars = e
			}
			var s Expr
			if p.acceptKw("from") {
				e, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				s = e
			} else {
				s, chars = chars, nil
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			args := []Expr{s}
			if chars != nil {
				args = append(args, chars)
			}
			return &FuncCall{Name: mode, Args: args, Pos: t.Pos}, nil
		}
	case "interval", "date", "timestamp", "timestamptz", "time":
		// Typed literal: DATE '2020-01-01', TIMESTAMP WITH TIME ZONE '...'.
		save := p.pos
		typ, err := p.parseTypeName()
		if err == nil && p.peek().Kind == TString {
			s := p.next()
			return &CastExpr{Expr: &Literal{Kind: LitString, Val: s.Val, Pos: s.Pos}, Type: typ, Pos: t.Pos}, nil
		}
		p.pos = save
	case "double":
		if p.isKwN(1, "precision") && p.peekN(2).Kind == TString {
			typ, err := p.parseTypeName()
			if err != nil {
				return nil, err
			}
			s := p.next()
			return &CastExpr{Expr: &Literal{Kind: LitString, Val: s.Val}, Type: typ, Pos: t.Pos}, nil
		}
	}
	if typeLiteralNames[t.Val] && p.peekN(1).Kind == TString {
		typ, err := p.parseTypeName()
		if err != nil {
			return nil, err
		}
		s := p.next()
		return &CastExpr{Expr: &Literal{Kind: LitString, Val: s.Val, Pos: s.Pos}, Type: typ, Pos: t.Pos}, nil
	}
	if reserved[t.Val] && !(p.peekN(1).Kind == TOp && p.peekN(1).Val == "(") {
		return nil, p.unexpected()
	}
	return p.parseNameOrCall()
}

func (p *Parser) parseNameOrCall() (Expr, error) {
	start := p.peek()
	parts := []string{p.next().Val}
	for p.isOp(".") {
		if p.peekN(1).Kind == TOp && p.peekN(1).Val == "*" {
			break // t.* handled by caller
		}
		p.next()
		id, err := p.colLabel()
		if err != nil {
			return nil, err
		}
		parts = append(parts, id)
	}
	if !p.isOp("(") {
		return &ColumnRef{Parts: parts, Pos: start.Pos}, nil
	}
	// Function call.
	p.next()
	fc := &FuncCall{Name: parts[len(parts)-1], Pos: start.Pos}
	if len(parts) > 1 {
		fc.Schema = parts[len(parts)-2]
	}
	if p.acceptOp("*") {
		fc.Star = true
	} else if !p.isOp(")") {
		if p.acceptKw("distinct") {
			fc.Distinct = true
		} else {
			p.acceptKw("all")
		}
		p.acceptKw("variadic")
		args, err := p.parseExprList()
		if err != nil {
			return nil, err
		}
		fc.Args = args
		if p.acceptKw("order") {
			if err := p.expectKw("by"); err != nil {
				return nil, err
			}
			ob, err := p.parseOrderList()
			if err != nil {
				return nil, err
			}
			fc.OrderBy = ob
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	if p.acceptKw("within", "group") {
		return nil, pgerr.Unsupported("ordered-set aggregates are not supported")
	}
	if p.acceptKw("filter") {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		if err := p.expectKw("where"); err != nil {
			return nil, err
		}
		f, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		fc.Filter = f
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
	}
	if p.acceptKw("over") {
		return nil, pgerr.Unsupported("window functions are not supported")
	}
	return fc, nil
}

func (p *Parser) parseCase() (Expr, error) {
	p.next() // CASE
	ce := &CaseExpr{}
	if !p.isKw("when") {
		op, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		ce.Operand = op
	}
	for p.acceptKw("when") {
		c, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("then"); err != nil {
			return nil, err
		}
		r, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		ce.Whens = append(ce.Whens, &When{Cond: c, Result: r})
	}
	if len(ce.Whens) == 0 {
		return nil, p.unexpected()
	}
	if p.acceptKw("else") {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		ce.Else = e
	}
	if err := p.expectKw("end"); err != nil {
		return nil, err
	}
	return ce, nil
}
