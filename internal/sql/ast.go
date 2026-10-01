package sql

// Stmt is a SQL statement.
type Stmt interface{ stmt() }

// Expr is a scalar expression.
type Expr interface{ expr() }

// TableExpr is an item of a FROM clause.
type TableExpr interface{ tableExpr() }

// ---- Queries ----

// SetOpKind is UNION, INTERSECT or EXCEPT.
type SetOpKind int

const (
	SetNone SetOpKind = iota
	SetUnion
	SetIntersect
	SetExcept
)

func (k SetOpKind) String() string {
	return [...]string{"", "UNION", "INTERSECT", "EXCEPT"}[k]
}

// SelectStmt is a query: a simple SELECT, a VALUES list, or a set
// operation over two queries, with optional WITH, ORDER BY and LIMIT.
type SelectStmt struct {
	With *With

	// Simple SELECT (when SetOp == SetNone and Values == nil).
	Distinct   bool
	DistinctOn []Expr
	Targets    []*Target
	From       []TableExpr
	Where      Expr
	GroupBy    []Expr
	Having     Expr

	// VALUES (...), (...)
	Values [][]Expr

	// Set operation.
	SetOp       SetOpKind
	SetAll      bool
	Left, Right *SelectStmt

	OrderBy   []*OrderItem
	Limit     Expr
	Offset    Expr
	ForUpdate bool
}

// Target is an item of a SELECT list or a RETURNING list.
type Target struct {
	Expr      Expr
	Alias     string
	Star      bool   // * or t.*
	StarTable string // qualifier of t.*
	Pos       int
}

// With is a WITH clause.
type With struct {
	Recursive bool
	CTEs      []*CTE
}

// CTE is one common table expression.
type CTE struct {
	Name    string
	Columns []string
	Query   *SelectStmt
}

// OrderItem is an ORDER BY item.
type OrderItem struct {
	Expr       Expr
	Desc       bool
	NullsFirst *bool
}

// ---- FROM items ----

// TableName is a possibly schema-qualified table reference with alias.
type TableName struct {
	Schema     string
	Name       string
	Alias      string
	ColAliases []string
	Pos        int
}

// SubqueryTable is (SELECT ...) AS alias.
type SubqueryTable struct {
	Query      *SelectStmt
	Alias      string
	ColAliases []string
	Lateral    bool
}

// FuncTable is a set-returning function in FROM, e.g. generate_series.
type FuncTable struct {
	Func       *FuncCall
	Alias      string
	ColAliases []string
}

// JoinType enumerates join kinds.
type JoinType int

const (
	JoinInner JoinType = iota
	JoinLeft
	JoinRight
	JoinFull
	JoinCross
)

func (j JoinType) String() string {
	return [...]string{"Inner", "Left", "Right", "Full", "Cross"}[j]
}

// JoinExpr is a join between two FROM items.
type JoinExpr struct {
	Type        JoinType
	Left, Right TableExpr
	On          Expr
	Using       []string
	Natural     bool
}

func (*TableName) tableExpr()     {}
func (*SubqueryTable) tableExpr() {}
func (*FuncTable) tableExpr()     {}
func (*JoinExpr) tableExpr()      {}

// ---- DML ----

// InsertStmt is INSERT INTO ... VALUES/SELECT ... [ON CONFLICT] [RETURNING].
type InsertStmt struct {
	With          *With
	Table         *TableName
	Columns       []string
	Source        *SelectStmt // nil with DefaultValues
	DefaultValues bool
	OnConflict    *OnConflict
	Returning     []*Target
}

// OnConflict is ON CONFLICT [(cols)] DO NOTHING | DO UPDATE SET ...
type OnConflict struct {
	Columns   []string
	DoNothing bool
	Set       []*SetClause
	Where     Expr
}

// SetClause is col = expr in UPDATE.
type SetClause struct {
	Column string
	Value  Expr // *DefaultExpr for DEFAULT
}

// UpdateStmt is UPDATE ... SET ... [FROM] [WHERE] [RETURNING].
type UpdateStmt struct {
	With      *With
	Table     *TableName
	Set       []*SetClause
	From      []TableExpr
	Where     Expr
	Returning []*Target
}

// DeleteStmt is DELETE FROM ... [USING] [WHERE] [RETURNING].
type DeleteStmt struct {
	With      *With
	Table     *TableName
	Using     []TableExpr
	Where     Expr
	Returning []*Target
}

// ---- DDL ----

// TypeName is a type as written in SQL.
type TypeName struct {
	Name  string
	Mods  []int
	Array bool
}

// ColumnDef is a column in CREATE TABLE.
type ColumnDef struct {
	Name        string
	Type        *TypeName
	NotNull     bool
	Default     Expr
	DefaultSQL  string
	Identity    bool
	Constraints []*Constraint // column constraints (PK, UNIQUE, CHECK, REFERENCES)
}

// ConstraintKind enumerates table constraints.
type ConstraintKind int

const (
	ConPrimaryKey ConstraintKind = iota
	ConUnique
	ConCheck
	ConForeignKey
	ConNotNull
)

// Constraint is a table or column constraint.
type Constraint struct {
	Name     string
	Kind     ConstraintKind
	Columns  []string // empty for column constraints until resolved
	Check    Expr
	CheckSQL string
	RefTable *TableName
	RefCols  []string
	OnDelete string // NO ACTION, RESTRICT, CASCADE, SET NULL, SET DEFAULT
	OnUpdate string
}

// CreateTableStmt is CREATE TABLE.
type CreateTableStmt struct {
	Table       *TableName
	IfNotExists bool
	Columns     []*ColumnDef
	Constraints []*Constraint
	AsSelect    *SelectStmt
}

// IndexElem is a column of CREATE INDEX.
type IndexElem struct {
	Column string
	Desc   bool
}

// CreateIndexStmt is CREATE [UNIQUE] INDEX.
type CreateIndexStmt struct {
	Name        string
	Table       *TableName
	Unique      bool
	IfNotExists bool
	Columns     []*IndexElem
	Using       string
}

// DropKind is the object kind of DROP.
type DropKind int

const (
	DropTable DropKind = iota
	DropIndex
	DropSequence
	DropView
)

// DropStmt is DROP TABLE/INDEX.
type DropStmt struct {
	Kind     DropKind
	Names    []*TableName
	IfExists bool
	Cascade  bool
}

// AlterTableStmt is ALTER TABLE with a single action.
type AlterTableStmt struct {
	Table         *TableName
	IfExists      bool
	AddColumn     *ColumnDef
	DropColumn    string
	RenameTo      string
	RenameCol     [2]string
	AddConstraint *Constraint
}

// TruncateStmt is TRUNCATE.
type TruncateStmt struct {
	Tables []*TableName
}

// CreateSequenceStmt is CREATE SEQUENCE (minimal).
type CreateSequenceStmt struct {
	Name        *TableName
	IfNotExists bool
	Start       int64
	Increment   int64
}

// ---- Transactions and utility ----

// TxnKind enumerates transaction control statements.
type TxnKind int

const (
	TxnBegin TxnKind = iota
	TxnCommit
	TxnRollback
	TxnSavepoint
	TxnRelease
	TxnRollbackTo
)

// TransactionStmt is BEGIN, COMMIT, ROLLBACK and friends.
type TransactionStmt struct {
	Kind      TxnKind
	Isolation string // "" or e.g. "repeatable read"
	ReadOnly  bool
	Savepoint string
}

// ExplainStmt is EXPLAIN [ANALYZE] stmt.
type ExplainStmt struct {
	Stmt    Stmt
	Analyze bool
	Verbose bool
	Costs   bool
}

// AnalyzeStmt is ANALYZE [table].
type AnalyzeStmt struct {
	Table *TableName
}

// VacuumStmt is VACUUM [FULL] [ANALYZE] [table].
type VacuumStmt struct {
	Table   *TableName
	Full    bool
	Analyze bool
}

// SetStmt is SET name = value / SET name TO value / RESET name.
type SetStmt struct {
	Name  string
	Value string // empty for DEFAULT / RESET
	Local bool
	Reset bool
	// SET TRANSACTION ISOLATION LEVEL ...
	TxnIsolation string
}

// ShowStmt is SHOW name.
type ShowStmt struct {
	Name string
}

// CheckpointStmt is CHECKPOINT.
type CheckpointStmt struct{}

// DeallocateStmt is DEALLOCATE [PREPARE] name | ALL.
type DeallocateStmt struct {
	Name string
	All  bool
}

// DiscardStmt is DISCARD ALL.
type DiscardStmt struct{}

// CopyStmt is COPY table [(cols)] FROM STDIN / TO STDOUT.
type CopyStmt struct {
	Table   *TableName
	Columns []string
	From    bool
	Format  string // text or csv
	Header  bool
	Delim   string
	Query   *SelectStmt
}

// ListenStmt and other no-op utility statements basalt accepts.
type NoopStmt struct {
	Tag string
}

func (*SelectStmt) stmt()         {}
func (*InsertStmt) stmt()         {}
func (*UpdateStmt) stmt()         {}
func (*DeleteStmt) stmt()         {}
func (*CreateTableStmt) stmt()    {}
func (*CreateIndexStmt) stmt()    {}
func (*DropStmt) stmt()           {}
func (*AlterTableStmt) stmt()     {}
func (*TruncateStmt) stmt()       {}
func (*CreateSequenceStmt) stmt() {}
func (*TransactionStmt) stmt()    {}
func (*ExplainStmt) stmt()        {}
func (*AnalyzeStmt) stmt()        {}
func (*VacuumStmt) stmt()         {}
func (*SetStmt) stmt()            {}
func (*ShowStmt) stmt()           {}
func (*CheckpointStmt) stmt()     {}
func (*DeallocateStmt) stmt()     {}
func (*DiscardStmt) stmt()        {}
func (*CopyStmt) stmt()           {}
func (*NoopStmt) stmt()           {}

// ---- Expressions ----

// ColumnRef is a possibly qualified column name: a, t.a, s.t.a.
type ColumnRef struct {
	Parts []string
	Pos   int
}

// LitKind enumerates literal kinds.
type LitKind int

const (
	LitNull LitKind = iota
	LitBool
	LitInt     // fits in int64
	LitNumeric // decimal point, exponent, or too large for int64
	LitString  // type unknown until resolved
)

// Literal is a constant as written.
type Literal struct {
	Kind LitKind
	Val  string
	Pos  int
}

// ParamRef is $n.
type ParamRef struct {
	N   int
	Pos int
}

// BinaryExpr is a binary operator, including AND and OR.
type BinaryExpr struct {
	Op          string
	Left, Right Expr
	Pos         int
}

// UnaryExpr is NOT, unary minus or unary plus.
type UnaryExpr struct {
	Op   string
	Expr Expr
	Pos  int
}

// FuncCall is a function or aggregate call.
type FuncCall struct {
	Name     string // lower-cased, schema prefix removed
	Schema   string
	Args     []Expr
	Star     bool // count(*)
	Distinct bool
	OrderBy  []*OrderItem
	Filter   Expr
	Over     *WindowSpec
	Pos      int
}

// WindowSpec is an OVER clause (parsed; window functions are not supported).
type WindowSpec struct {
	PartitionBy []Expr
	OrderBy     []*OrderItem
}

// CastExpr is CAST(x AS t), x::t or t 'literal'.
type CastExpr struct {
	Expr Expr
	Type *TypeName
	Pos  int
}

// When is one WHEN ... THEN ... arm.
type When struct {
	Cond, Result Expr
}

// CaseExpr is CASE [operand] WHEN ... END.
type CaseExpr struct {
	Operand Expr
	Whens   []*When
	Else    Expr
}

// IsKind is what an IS test checks for.
type IsKind int

const (
	IsNull IsKind = iota
	IsTrue
	IsFalse
	IsUnknown
)

// IsExpr is x IS [NOT] NULL/TRUE/FALSE/UNKNOWN.
type IsExpr struct {
	Expr Expr
	What IsKind
	Not  bool
}

// IsDistinctExpr is x IS [NOT] DISTINCT FROM y.
type IsDistinctExpr struct {
	Left, Right Expr
	Not         bool
}

// BetweenExpr is x [NOT] BETWEEN [SYMMETRIC] a AND b.
type BetweenExpr struct {
	Expr, Low, High Expr
	Not, Symmetric  bool
}

// InExpr is x [NOT] IN (list) or x [NOT] IN (subquery).
type InExpr struct {
	Expr     Expr
	List     []Expr
	Subquery *SelectStmt
	Not      bool
}

// ExistsExpr is EXISTS (subquery).
type ExistsExpr struct {
	Query *SelectStmt
}

// SubqueryExpr is a scalar subquery.
type SubqueryExpr struct {
	Query *SelectStmt
	Pos   int
}

// LikeExpr is x [NOT] LIKE/ILIKE pattern [ESCAPE e].
type LikeExpr struct {
	Expr, Pattern, Escape Expr
	Not, ILike            bool
}

// AnyAllExpr is x op ANY/ALL (array or subquery).
type AnyAllExpr struct {
	Left     Expr
	Op       string
	All      bool
	Right    Expr // array expression, nil when Subquery is set
	Subquery *SelectStmt
}

// ArrayExpr is ARRAY[...] or ARRAY(subquery).
type ArrayExpr struct {
	Elems    []Expr
	Subquery *SelectStmt
}

// SubscriptExpr is x[i].
type SubscriptExpr struct {
	Expr, Index Expr
}

// RowExpr is (a, b, ...) or ROW(a, b).
type RowExpr struct {
	Exprs []Expr
}

// DefaultExpr is the DEFAULT keyword in VALUES or SET.
type DefaultExpr struct{}

// StarExpr is * appearing as an expression (only valid in a few places).
type StarExpr struct{}

func (*ColumnRef) expr()      {}
func (*Literal) expr()        {}
func (*ParamRef) expr()       {}
func (*BinaryExpr) expr()     {}
func (*UnaryExpr) expr()      {}
func (*FuncCall) expr()       {}
func (*CastExpr) expr()       {}
func (*CaseExpr) expr()       {}
func (*IsExpr) expr()         {}
func (*IsDistinctExpr) expr() {}
func (*BetweenExpr) expr()    {}
func (*InExpr) expr()         {}
func (*ExistsExpr) expr()     {}
func (*SubqueryExpr) expr()   {}
func (*LikeExpr) expr()       {}
func (*AnyAllExpr) expr()     {}
func (*ArrayExpr) expr()      {}
func (*SubscriptExpr) expr()  {}
func (*RowExpr) expr()        {}
func (*DefaultExpr) expr()    {}
func (*StarExpr) expr()       {}
