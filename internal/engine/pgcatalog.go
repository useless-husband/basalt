package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/planner"
	"github.com/useless-husband/basalt/internal/storage"
	"github.com/useless-husband/basalt/internal/txn"
	"github.com/useless-husband/basalt/internal/types"
)

// System object ids, matching PostgreSQL where clients may rely on them.
const (
	oidPgCatalog   = 11
	oidPublic      = 2200
	oidInfoSchema  = 13181
	oidSuperuser   = 10
	oidDatabase    = 5
	oidBtreeAM     = 403
	oidHeapAM      = 2
	oidDefaultColl = 100
)

// vt helpers

type vcol = planner.VirtualColumn

func cols(spec ...any) []vcol {
	var out []vcol
	for i := 0; i < len(spec); i += 2 {
		out = append(out, vcol{Name: spec[i].(string), T: spec[i+1].(types.T)})
	}
	return out
}

var (
	tOid      = types.Oid
	tName     = types.Name
	tText     = types.Text
	tBool     = types.Bool
	tInt2     = types.Int2
	tInt4     = types.Int4
	tInt8     = types.Int8
	tFloat4   = types.Float4
	tChar     = types.Char
	tInt2Vec  = types.Int2Vector
	tOidVec   = types.OidVector
	tNodeTree = types.PgNodeTree
	tTextArr  = types.T{Oid: types.OidTextArray, Mod: -1}
	tOidArr   = types.T{Oid: types.OidOidArray, Mod: -1}
	tInt2Arr  = types.T{Oid: types.OidInt2Array, Mod: -1}
	tCharArr  = types.T{Oid: types.OidCharArray, Mod: -1}
	tAclArr   = types.T{Oid: types.OidAclItemArr, Mod: -1}
	tTS       = types.TimestampTZ
)

func vText(s string) types.Value { return types.NewText(s) }
func vInt(i int64) types.Value   { return types.NewInt(i) }
func vBool(b bool) types.Value   { return types.NewBool(b) }

var null = types.Null

// relKindOf returns pg_class.relkind for an oid.
func relInfo(cat *catalog.Catalog, oid uint32) (name string, kind byte, ok bool) {
	if t := cat.Tables[oid]; t != nil {
		return t.Name, 'r', true
	}
	if ix := cat.Indexes[oid]; ix != nil {
		return ix.Name, 'i', true
	}
	if s := cat.Sequences[oid]; s != nil {
		return s.Name, 'S', true
	}
	return "", 0, false
}

// virtualTable resolves pg_catalog and information_schema tables.
func (s *Session) virtualTable(schema, name string) (*planner.VirtualTable, bool) {
	cat := s.catalog()
	mk := func(columns []vcol, rows func() [][]types.Value) (*planner.VirtualTable, bool) {
		return &planner.VirtualTable{Name: name, Columns: columns, Rows: func() ([][]types.Value, error) { return rows(), nil }}, true
	}
	if schema == "information_schema" {
		return s.infoSchema(cat, name)
	}
	switch name {
	case "pg_namespace":
		return mk(cols("oid", tOid, "nspname", tName, "nspowner", tOid, "nspacl", tAclArr), func() [][]types.Value {
			return [][]types.Value{
				{vInt(oidPgCatalog), vText("pg_catalog"), vInt(oidSuperuser), null},
				{vInt(oidPublic), vText("public"), vInt(oidSuperuser), null},
				{vInt(oidInfoSchema), vText("information_schema"), vInt(oidSuperuser), null},
			}
		})
	case "pg_class":
		return mk(cols("oid", tOid, "relname", tName, "relnamespace", tOid, "reltype", tOid, "reloftype", tOid,
			"relowner", tOid, "relam", tOid, "relfilenode", tOid, "reltablespace", tOid, "relpages", tInt4,
			"reltuples", tFloat4, "relallvisible", tInt4, "reltoastrelid", tOid, "relhasindex", tBool,
			"relisshared", tBool, "relpersistence", tChar, "relkind", tChar, "relnatts", tInt2, "relchecks", tInt2,
			"relhasrules", tBool, "relhastriggers", tBool, "relhassubclass", tBool, "relrowsecurity", tBool,
			"relforcerowsecurity", tBool, "relispopulated", tBool, "relreplident", tChar, "relispartition", tBool,
			"relrewrite", tOid, "relfrozenxid", tInt8, "relminmxid", tInt8, "relacl", tAclArr, "reloptions", tTextArr,
			"relpartbound", tNodeTree), func() [][]types.Value {
			var out [][]types.Value
			row := func(oid uint32, name string, kind byte, am uint32, natts int, checks int, hasIndex bool, pages int, tuples float64) {
				out = append(out, []types.Value{vInt(int64(oid)), vText(name), vInt(oidPublic), vInt(0), vInt(0),
					vInt(oidSuperuser), vInt(int64(am)), vInt(int64(oid)), vInt(0), vInt(int64(pages)),
					types.NewFloat(tuples), vInt(0), vInt(0), vBool(hasIndex),
					vBool(false), vText("p"), vText(string(kind)), vInt(int64(natts)), vInt(int64(checks)),
					vBool(false), vBool(false), vBool(false), vBool(false),
					vBool(false), vBool(true), vText("d"), vBool(false),
					vInt(0), vInt(0), vInt(0), null, null, null})
			}
			for _, t := range cat.SortedTables() {
				tuples := -1.0
				if t.Stats != nil {
					tuples = t.Stats.Rows
				}
				row(t.Oid, t.Name, 'r', oidHeapAM, len(t.Columns), len(t.Checks), len(t.Indexes) > 0, s.db.heapPages(t), tuples)
			}
			for _, ix := range cat.Indexes {
				row(ix.Oid, ix.Name, 'i', oidBtreeAM, len(ix.Columns), 0, false, 1, -1)
			}
			for _, sq := range cat.Sequences {
				row(sq.Oid, sq.Name, 'S', 0, 3, 0, false, 1, 1)
			}
			sortRows(out, 0)
			return out
		})
	case "pg_attribute":
		return mk(cols("attrelid", tOid, "attname", tName, "atttypid", tOid, "attlen", tInt2, "attnum", tInt2,
			"attcacheoff", tInt4, "atttypmod", tInt4, "attndims", tInt2, "attbyval", tBool, "attalign", tChar,
			"attstorage", tChar, "attcompression", tChar, "attnotnull", tBool, "atthasdef", tBool, "atthasmissing", tBool,
			"attidentity", tChar, "attgenerated", tChar, "attisdropped", tBool, "attislocal", tBool, "attinhcount", tInt2,
			"attstattarget", tInt2, "attcollation", tOid, "attacl", tAclArr, "attoptions", tTextArr, "attfdwoptions", tTextArr,
			"attmissingval", tTextArr), func() [][]types.Value {
			var out [][]types.Value
			add := func(rel uint32, num int, c *catalog.Column) {
				coll := int64(0)
				if c.Type.IsString() {
					coll = oidDefaultColl
				}
				ident := ""
				if c.Identity {
					ident = "d"
				}
				out = append(out, []types.Value{vInt(int64(rel)), vText(c.Name), vInt(int64(c.Type.Oid)), vInt(int64(c.Type.Len())),
					vInt(int64(num)), vInt(-1), vInt(int64(c.Type.Mod)), vInt(0), vBool(c.Type.Len() > 0 && c.Type.Len() <= 8), vText("i"),
					vText("p"), vText(""), vBool(c.NotNull), vBool(c.Default != ""), vBool(c.HasMissing),
					vText(ident), vText(""), vBool(false), vBool(true), vInt(0),
					vInt(-1), vInt(coll), null, null, null, null})
			}
			for _, t := range cat.SortedTables() {
				for i, c := range t.Columns {
					add(t.Oid, i+1, c)
				}
			}
			for _, ix := range cat.Indexes {
				t := cat.Tables[ix.Table]
				for i, c := range ix.Columns {
					add(ix.Oid, i+1, &catalog.Column{Name: t.Columns[c].Name, Type: t.Columns[c].Type})
				}
			}
			return out
		})
	case "pg_type":
		return mk(cols("oid", tOid, "typname", tName, "typnamespace", tOid, "typowner", tOid, "typlen", tInt2,
			"typbyval", tBool, "typtype", tChar, "typcategory", tChar, "typispreferred", tBool, "typisdefined", tBool,
			"typdelim", tChar, "typrelid", tOid, "typsubscript", tOid, "typelem", tOid, "typarray", tOid,
			"typinput", tOid, "typoutput", tOid, "typreceive", tOid, "typsend", tOid, "typbasetype", tOid,
			"typtypmod", tInt4, "typndims", tInt4, "typcollation", tOid, "typnotnull", tBool, "typdefault", tText,
			"typacl", tAclArr), func() [][]types.Value {
			var out [][]types.Value
			oids := types.AllTypeOids()
			sort.Slice(oids, func(i, j int) bool { return oids[i] < oids[j] })
			for _, o := range oids {
				t := types.T{Oid: o, Mod: -1}
				typtype := "b"
				if t.Category() == 'P' {
					typtype = "p"
				}
				var elem, arr int64
				if t.IsArray() {
					elem = int64(t.Elem().Oid)
				}
				if a, ok := types.ArrayOf(t); ok {
					arr = int64(a.Oid)
				}
				coll := int64(0)
				if t.IsString() {
					coll = oidDefaultColl
				}
				out = append(out, []types.Value{vInt(int64(o)), vText(t.TypName()), vInt(oidPgCatalog), vInt(oidSuperuser),
					vInt(int64(t.Len())), vBool(t.Len() > 0 && t.Len() <= 8), vText(typtype), vText(string(t.Category())),
					vBool(false), vBool(true), vText(","), vInt(0), vInt(0), vInt(elem), vInt(arr),
					vInt(0), vInt(0), vInt(0), vInt(0), vInt(0), vInt(-1), vInt(0), vInt(coll), vBool(false), null, null})
			}
			return out
		})
	case "pg_index":
		return mk(cols("indexrelid", tOid, "indrelid", tOid, "indnatts", tInt2, "indnkeyatts", tInt2,
			"indisunique", tBool, "indnullsnotdistinct", tBool, "indisprimary", tBool, "indisexclusion", tBool,
			"indimmediate", tBool, "indisclustered", tBool, "indisvalid", tBool, "indcheckxmin", tBool,
			"indisready", tBool, "indislive", tBool, "indisreplident", tBool, "indkey", tInt2Vec,
			"indcollation", tOidVec, "indclass", tOidVec, "indoption", tInt2Vec, "indexprs", tNodeTree, "indpred", tNodeTree),
			func() [][]types.Value {
				var out [][]types.Value
				for _, ix := range cat.Indexes {
					var keys, opts, coll, class []types.Value
					for _, c := range ix.Columns {
						keys = append(keys, vInt(int64(c+1)))
						opts = append(opts, vInt(0))
						coll = append(coll, vInt(0))
						class = append(class, vInt(0))
					}
					n := int64(len(ix.Columns))
					out = append(out, []types.Value{vInt(int64(ix.Oid)), vInt(int64(ix.Table)), vInt(n), vInt(n),
						vBool(ix.Unique), vBool(false), vBool(ix.Primary), vBool(false),
						vBool(true), vBool(false), vBool(true), vBool(false),
						vBool(true), vBool(true), vBool(false), types.NewArray(types.Int2, keys),
						types.NewArray(types.Oid, coll), types.NewArray(types.Oid, class), types.NewArray(types.Int2, opts), null, null})
				}
				sortRows(out, 0)
				return out
			})
	case "pg_constraint":
		return mk(cols("oid", tOid, "conname", tName, "connamespace", tOid, "contype", tChar, "condeferrable", tBool,
			"condeferred", tBool, "convalidated", tBool, "conrelid", tOid, "contypid", tOid, "conindid", tOid,
			"conparentid", tOid, "confrelid", tOid, "confupdtype", tChar, "confdeltype", tChar, "confmatchtype", tChar,
			"conislocal", tBool, "coninhcount", tInt2, "connoinherit", tBool, "conkey", tInt2Arr, "confkey", tInt2Arr,
			"conpfeqop", tOidArr, "conppeqop", tOidArr, "conffeqop", tOidArr, "confdelsetcols", tInt2Arr, "conexclop", tOidArr,
			"conbin", tNodeTree, "conperiod", tBool, "conenforced", tBool), func() [][]types.Value {
			return constraintRows(cat)
		})
	case "pg_attrdef":
		return mk(cols("oid", tOid, "adrelid", tOid, "adnum", tInt2, "adbin", tNodeTree), func() [][]types.Value {
			var out [][]types.Value
			for _, t := range cat.SortedTables() {
				for i, c := range t.Columns {
					if c.Default != "" {
						out = append(out, []types.Value{vInt(int64(attrdefOid(t.Oid, i))), vInt(int64(t.Oid)), vInt(int64(i + 1)), vText(c.Default)})
					}
				}
			}
			return out
		})
	case "pg_am":
		return mk(cols("oid", tOid, "amname", tName, "amhandler", tOid, "amtype", tChar), func() [][]types.Value {
			return [][]types.Value{{vInt(oidHeapAM), vText("heap"), vInt(0), vText("t")}, {vInt(oidBtreeAM), vText("btree"), vInt(0), vText("i")}}
		})
	case "pg_database":
		return mk(cols("oid", tOid, "datname", tName, "datdba", tOid, "encoding", tInt4, "datlocprovider", tChar,
			"datistemplate", tBool, "datallowconn", tBool, "datconnlimit", tInt4, "datcollate", tText, "datctype", tText,
			"daticulocale", tText, "datlocale", tText, "daticurules", tText, "datcollversion", tText, "datacl", tAclArr, "dattablespace", tOid),
			func() [][]types.Value {
				return [][]types.Value{{vInt(oidDatabase), vText(s.database), vInt(oidSuperuser), vInt(6), vText("c"),
					vBool(false), vBool(true), vInt(-1), vText("C"), vText("C"), null, null, null, null, null, vInt(1663)}}
			})
	case "pg_roles", "pg_authid", "pg_user":
		return mk(cols("oid", tOid, "rolname", tName, "rolsuper", tBool, "rolinherit", tBool, "rolcreaterole", tBool,
			"rolcreatedb", tBool, "rolcanlogin", tBool, "rolreplication", tBool, "rolconnlimit", tInt4,
			"rolpassword", tText, "rolvaliduntil", tTS, "rolbypassrls", tBool, "rolconfig", tTextArr,
			"usename", tName, "usesysid", tOid, "usesuper", tBool), func() [][]types.Value {
			return [][]types.Value{{vInt(oidSuperuser), vText(s.user), vBool(true), vBool(true), vBool(true),
				vBool(true), vBool(true), vBool(true), vInt(-1), vText("********"), null, vBool(true), null,
				vText(s.user), vInt(oidSuperuser), vBool(true)}}
		})
	case "pg_tables":
		return mk(cols("schemaname", tName, "tablename", tName, "tableowner", tName, "tablespace", tName,
			"hasindexes", tBool, "hasrules", tBool, "hastriggers", tBool, "rowsecurity", tBool), func() [][]types.Value {
			var out [][]types.Value
			for _, t := range cat.SortedTables() {
				out = append(out, []types.Value{vText("public"), vText(t.Name), vText(s.user), null, vBool(len(t.Indexes) > 0), vBool(false), vBool(false), vBool(false)})
			}
			return out
		})
	case "pg_indexes":
		return mk(cols("schemaname", tName, "tablename", tName, "indexname", tName, "tablespace", tName, "indexdef", tText), func() [][]types.Value {
			var out [][]types.Value
			for _, t := range cat.SortedTables() {
				for _, ix := range cat.TableIndexes(t) {
					out = append(out, []types.Value{vText("public"), vText(t.Name), vText(ix.Name), null, vText(ix.Definition(t))})
				}
			}
			return out
		})
	case "pg_sequence":
		return mk(cols("seqrelid", tOid, "seqtypid", tOid, "seqstart", tInt8, "seqincrement", tInt8, "seqmax", tInt8,
			"seqmin", tInt8, "seqcache", tInt8, "seqcycle", tBool), func() [][]types.Value {
			var out [][]types.Value
			for _, sq := range cat.Sequences {
				out = append(out, []types.Value{vInt(int64(sq.Oid)), vInt(int64(types.OidInt8)), vInt(sq.Start), vInt(sq.Increment),
					vInt(1<<63 - 1), vInt(1), vInt(1), vBool(false)})
			}
			return out
		})
	case "pg_settings":
		return mk(cols("name", tText, "setting", tText, "unit", tText, "category", tText, "short_desc", tText,
			"context", tText, "vartype", tText, "source", tText), func() [][]types.Value {
			var keys []string
			for k := range s.settings {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var out [][]types.Value
			for _, k := range keys {
				out = append(out, []types.Value{vText(k), vText(s.settings[k]), null, vText(""), vText(""), vText("user"), vText("string"), vText("default")})
			}
			return out
		})
	case "pg_stat_user_tables", "pg_stat_all_tables":
		return mk(cols("relid", tOid, "schemaname", tName, "relname", tName, "n_live_tup", tInt8, "n_dead_tup", tInt8,
			"n_tup_ins", tInt8, "n_tup_del", tInt8), func() [][]types.Value {
			var out [][]types.Value
			for _, t := range cat.SortedTables() {
				var ins, del int64
				if v, ok := s.db.changes.Load(t.Oid); ok {
					ins, del = v.(*tableChanges).inserted.Load(), v.(*tableChanges).deleted.Load()
				}
				live := int64(0)
				if t.Stats != nil {
					live = int64(t.Stats.Rows)
				}
				out = append(out, []types.Value{vInt(int64(t.Oid)), vText("public"), vText(t.Name), vInt(live), vInt(del), vInt(ins), vInt(del)})
			}
			return out
		})
	case "pg_locks":
		return mk(cols("locktype", tText, "relation", tOid, "transactionid", tInt8, "virtualtransaction", tText,
			"mode", tText, "granted", tBool), func() [][]types.Value {
			var out [][]types.Value
			for _, l := range s.db.txns.Locks.Snapshot() {
				kind := map[txn.TagKind]string{txn.TagXid: "transactionid", txn.TagRelation: "relation", txn.TagKey: "tuple"}[l.Tag.Kind]
				rel, xid := null, null
				if l.Tag.Kind == txn.TagRelation {
					rel = vInt(int64(l.Tag.ID))
				} else if l.Tag.Kind == txn.TagXid {
					xid = vInt(int64(l.Tag.ID))
				}
				out = append(out, []types.Value{vText(kind), rel, xid, vText(fmt.Sprintf("v%d", l.Owner)), vText(l.Mode.String()), vBool(l.Granted)})
			}
			return out
		})
	case "pg_stat_activity":
		return mk(cols("datname", tName, "pid", tInt4, "usename", tName, "application_name", tText, "state", tText, "query", tText), func() [][]types.Value {
			s.db.sessMu.Lock()
			defer s.db.sessMu.Unlock()
			var out [][]types.Value
			for pid, ss := range s.db.sessions {
				out = append(out, []types.Value{vText(ss.database), vInt(pid), vText(ss.user), vText(ss.settings["application_name"]), vText("active"), vText("")})
			}
			sortRows(out, 1)
			return out
		})
	case "pg_proc":
		return mk(cols("oid", tOid, "proname", tName, "pronamespace", tOid, "prokind", tChar, "prorettype", tOid,
			"proargtypes", tOidVec, "proretset", tBool), func() [][]types.Value {
			var out [][]types.Value
			for i, n := range expr.FunctionNames() {
				out = append(out, []types.Value{vInt(int64(1000000 + i)), vText(n), vInt(oidPgCatalog), vText("f"), vInt(0), types.NewArray(types.Oid, nil), vBool(false)})
			}
			return out
		})
	case "pg_collation":
		return mk(cols("oid", tOid, "collname", tName, "collnamespace", tOid, "collprovider", tChar), func() [][]types.Value {
			return [][]types.Value{{vInt(oidDefaultColl), vText("default"), vInt(oidPgCatalog), vText("d")}}
		})
	case "pg_tablespace":
		return mk(cols("oid", tOid, "spcname", tName), func() [][]types.Value {
			return [][]types.Value{{vInt(1663), vText("pg_default")}}
		})
	case "pg_description", "pg_shdescription":
		return mk(cols("objoid", tOid, "classoid", tOid, "objsubid", tInt4, "description", tText), empty)
	case "pg_inherits":
		return mk(cols("inhrelid", tOid, "inhparent", tOid, "inhseqno", tInt4, "inhdetachpending", tBool), empty)
	case "pg_trigger":
		return mk(cols("oid", tOid, "tgrelid", tOid, "tgparentid", tOid, "tgname", tName, "tgfoid", tOid, "tgtype", tInt2,
			"tgenabled", tChar, "tgisinternal", tBool, "tgconstraint", tOid), empty)
	case "pg_rewrite":
		return mk(cols("oid", tOid, "rulename", tName, "ev_class", tOid, "ev_type", tChar, "is_instead", tBool), empty)
	case "pg_policy":
		return mk(cols("oid", tOid, "polname", tName, "polrelid", tOid, "polcmd", tChar, "polpermissive", tBool,
			"polroles", tOidArr, "polqual", tNodeTree, "polwithcheck", tNodeTree), empty)
	case "pg_statistic_ext":
		return mk(cols("oid", tOid, "stxrelid", tOid, "stxname", tName, "stxnamespace", tOid, "stxowner", tOid,
			"stxstattarget", tInt2, "stxkeys", tInt2Vec, "stxkind", tCharArr, "stxexprs", tNodeTree), empty)
	case "pg_statistic_ext_data":
		return mk(cols("stxoid", tOid, "stxdinherit", tBool), empty)
	case "pg_publication":
		return mk(cols("oid", tOid, "pubname", tName, "pubowner", tOid, "puballtables", tBool, "pubinsert", tBool,
			"pubupdate", tBool, "pubdelete", tBool, "pubtruncate", tBool, "pubviaroot", tBool), empty)
	case "pg_publication_rel":
		return mk(cols("oid", tOid, "prpubid", tOid, "prrelid", tOid, "prqual", tNodeTree, "prattrs", tInt2Vec), empty)
	case "pg_publication_namespace":
		return mk(cols("oid", tOid, "pnpubid", tOid, "pnnspid", tOid), empty)
	case "pg_partitioned_table":
		return mk(cols("partrelid", tOid, "partstrat", tChar, "partnatts", tInt2), empty)
	case "pg_foreign_table":
		return mk(cols("ftrelid", tOid, "ftserver", tOid, "ftoptions", tTextArr), empty)
	case "pg_extension":
		return mk(cols("oid", tOid, "extname", tName, "extowner", tOid, "extnamespace", tOid, "extrelocatable", tBool, "extversion", tText), empty)
	case "pg_event_trigger":
		return mk(cols("oid", tOid, "evtname", tName, "evtevent", tName, "evtowner", tOid, "evtfoid", tOid, "evtenabled", tChar), empty)
	case "pg_depend":
		return mk(cols("classid", tOid, "objid", tOid, "objsubid", tInt4, "refclassid", tOid, "refobjid", tOid, "refobjsubid", tInt4, "deptype", tChar), empty)
	case "pg_opclass":
		return mk(cols("oid", tOid, "opcmethod", tOid, "opcname", tName, "opcnamespace", tOid, "opcdefault", tBool), empty)
	}
	return nil, false
}

func empty() [][]types.Value { return nil }

func sortRows(rows [][]types.Value, col int) {
	sort.SliceStable(rows, func(i, j int) bool { return types.Compare(rows[i][col], rows[j][col]) < 0 })
}

// Constraint oids are derived from the owning object so they are stable.
func checkOid(table uint32, i int) uint32   { return 0x40000000 | table<<8 | uint32(i) }
func fkOid(table uint32, i int) uint32      { return 0x50000000 | table<<8 | uint32(i) }
func attrdefOid(table uint32, i int) uint32 { return 0x70000000 | table<<8 | uint32(i) }

func int2s(cols []int) types.Value {
	var vs []types.Value
	for _, c := range cols {
		vs = append(vs, vInt(int64(c+1)))
	}
	return types.NewArray(types.Int2, vs)
}

func constraintRows(cat *catalog.Catalog) [][]types.Value {
	var out [][]types.Value
	actionCode := map[string]string{"NO ACTION": "a", "RESTRICT": "r", "CASCADE": "c", "SET NULL": "n", "SET DEFAULT": "d"}
	row := func(oid uint32, name string, typ string, rel uint32, ind uint32, frel uint32, upd, del string, key, fkey types.Value, bin string) {
		var b types.Value = null
		if bin != "" {
			b = vText(bin)
		}
		out = append(out, []types.Value{vInt(int64(oid)), vText(name), vInt(oidPublic), vText(typ), vBool(false),
			vBool(false), vBool(true), vInt(int64(rel)), vInt(0), vInt(int64(ind)),
			vInt(0), vInt(int64(frel)), vText(upd), vText(del), vText("s"),
			vBool(true), vInt(0), vBool(false), key, fkey,
			null, null, null, null, null, b, vBool(false), vBool(true)})
	}
	for _, t := range cat.SortedTables() {
		for _, ix := range cat.TableIndexes(t) {
			if !ix.Constraint {
				continue
			}
			typ := "u"
			if ix.Primary {
				typ = "p"
			}
			row(ix.Oid, ix.Name, typ, t.Oid, ix.Oid, 0, " ", " ", int2s(ix.Columns), null, "")
		}
		for i, ch := range t.Checks {
			row(checkOid(t.Oid, i), ch.Name, "c", t.Oid, 0, 0, " ", " ", null, null, ch.Expr)
		}
		for i, fk := range t.FKs {
			pix := uint32(0)
			if p := cat.Tables[fk.RefTable]; p != nil {
				for _, ix := range cat.TableIndexes(p) {
					if ix.Unique && sameInts(ix.Columns, fk.RefColumns) {
						pix = ix.Oid
					}
				}
			}
			row(fkOid(t.Oid, i), fk.Name, "f", t.Oid, pix, fk.RefTable, actionCode[fk.OnUpdate], actionCode[fk.OnDelete], int2s(fk.Columns), int2s(fk.RefColumns), "")
		}
	}
	return out
}

func (s *Session) infoSchema(cat *catalog.Catalog, name string) (*planner.VirtualTable, bool) {
	mk := func(columns []vcol, rows func() [][]types.Value) (*planner.VirtualTable, bool) {
		return &planner.VirtualTable{Name: name, Columns: columns, Rows: func() ([][]types.Value, error) { return rows(), nil }}, true
	}
	db := vText(s.database)
	switch name {
	case "tables":
		return mk(cols("table_catalog", tName, "table_schema", tName, "table_name", tName, "table_type", tText), func() [][]types.Value {
			var out [][]types.Value
			for _, t := range cat.SortedTables() {
				out = append(out, []types.Value{db, vText("public"), vText(t.Name), vText("BASE TABLE")})
			}
			return out
		})
	case "columns":
		return mk(cols("table_catalog", tName, "table_schema", tName, "table_name", tName, "column_name", tName,
			"ordinal_position", tInt4, "column_default", tText, "is_nullable", tText, "data_type", tText,
			"character_maximum_length", tInt4, "numeric_precision", tInt4, "numeric_scale", tInt4, "udt_name", tName), func() [][]types.Value {
			var out [][]types.Value
			for _, t := range cat.SortedTables() {
				for i, c := range t.Columns {
					def := null
					if c.Default != "" {
						def = vText(c.Default)
					}
					nullable := "YES"
					if c.NotNull {
						nullable = "NO"
					}
					dt := c.Type.String()
					if j := strings.IndexByte(dt, '('); j > 0 {
						dt = dt[:j]
					}
					maxLen, prec, scale := null, null, null
					if n := c.Type.CharLen(); n > 0 {
						maxLen = vInt(int64(n))
					}
					if p, sc, ok := c.Type.NumericPrecScale(); ok {
						prec, scale = vInt(int64(p)), vInt(int64(sc))
					}
					out = append(out, []types.Value{db, vText("public"), vText(t.Name), vText(c.Name), vInt(int64(i + 1)),
						def, vText(nullable), vText(dt), maxLen, prec, scale, vText(c.Type.TypName())})
				}
			}
			return out
		})
	case "schemata":
		return mk(cols("catalog_name", tName, "schema_name", tName, "schema_owner", tName), func() [][]types.Value {
			return [][]types.Value{{db, vText("information_schema"), vText(s.user)}, {db, vText("pg_catalog"), vText(s.user)}, {db, vText("public"), vText(s.user)}}
		})
	case "table_constraints":
		return mk(cols("constraint_catalog", tName, "constraint_schema", tName, "constraint_name", tName,
			"table_schema", tName, "table_name", tName, "constraint_type", tText), func() [][]types.Value {
			var out [][]types.Value
			kinds := map[string]string{"p": "PRIMARY KEY", "u": "UNIQUE", "c": "CHECK", "f": "FOREIGN KEY"}
			for _, r := range constraintRows(cat) {
				t := cat.Tables[uint32(r[7].I)]
				out = append(out, []types.Value{db, vText("public"), r[1], vText("public"), vText(t.Name), vText(kinds[r[3].S])})
			}
			return out
		})
	case "key_column_usage":
		return mk(cols("constraint_catalog", tName, "constraint_schema", tName, "constraint_name", tName,
			"table_schema", tName, "table_name", tName, "column_name", tName, "ordinal_position", tInt4), func() [][]types.Value {
			var out [][]types.Value
			for _, r := range constraintRows(cat) {
				if r[3].S == "c" {
					continue
				}
				t := cat.Tables[uint32(r[7].I)]
				for i, k := range r[18].Arr().Vals {
					out = append(out, []types.Value{db, vText("public"), r[1], vText("public"), vText(t.Name), vText(t.Columns[k.I-1].Name), vInt(int64(i + 1))})
				}
			}
			return out
		})
	}
	return nil, false
}

// ---- catalog functions ----

// CatalogFunc implements the pg_* functions and catalog-aware casts.
func (s *Session) CatalogFunc(name string, a []types.Value) (types.Value, error) {
	cat := s.catalog()
	argNull := func(i int) bool { return i >= len(a) || a[i].IsNull() }
	switch name {
	case "cast":
		return s.regCast(cat, a[0], uint32(a[1].I), uint32(a[2].I))
	case "pg_get_userbyid":
		if argNull(0) {
			return null, nil
		}
		if a[0].I == oidSuperuser {
			return vText(s.user), nil
		}
		return vText(fmt.Sprintf("unknown (OID=%d)", a[0].I)), nil
	case "pg_table_is_visible", "pg_type_is_visible", "pg_function_is_visible":
		if argNull(0) {
			return null, nil
		}
		return vBool(true), nil
	case "format_type":
		if argNull(0) {
			return null, nil
		}
		mod := int32(-1)
		if !argNull(1) {
			mod = int32(a[1].I)
		}
		t := types.T{Oid: uint32(a[0].I), Mod: mod}
		if !types.Known(t.Oid) {
			return vText("???"), nil
		}
		return vText(t.String()), nil
	case "pg_get_expr":
		if argNull(0) {
			return null, nil
		}
		return vText(a[0].S), nil
	case "pg_get_indexdef":
		if argNull(0) {
			return null, nil
		}
		ix := cat.Indexes[uint32(a[0].I)]
		if ix == nil {
			return null, nil
		}
		t := cat.Tables[ix.Table]
		if !argNull(1) && a[1].I > 0 {
			c := int(a[1].I) - 1
			if c < len(ix.Columns) {
				return vText(catalog.QuoteIdent(t.Columns[ix.Columns[c]].Name)), nil
			}
			return vText(""), nil
		}
		return vText(ix.Definition(t)), nil
	case "pg_get_constraintdef":
		if argNull(0) {
			return null, nil
		}
		return s.constraintDef(cat, uint32(a[0].I))
	case "pg_relation_is_publishable":
		return vBool(false), nil
	case "pg_get_triggerdef", "pg_get_ruledef", "pg_tablespace_location", "pg_get_function_arguments":
		return null, nil
	case "pg_get_viewdef", "pg_get_statisticsobjdef_columns", "pg_get_partkeydef", "pg_get_function_identity_arguments",
		"pg_get_function_result", "obj_description", "col_description", "shobj_description":
		return null, nil
	case "pg_get_serial_sequence":
		if argNull(0) || argNull(1) {
			return null, nil
		}
		t := cat.TableByName(strings.TrimPrefix(a[0].S, "public."))
		if t == nil {
			return null, pgerr.New(pgerr.UndefinedTable, "relation \"%s\" does not exist", a[0].S)
		}
		ci := t.ColumnIndex(a[1].S)
		for _, sq := range cat.Sequences {
			if sq.OwnerTable == t.Oid && sq.OwnerCol == ci {
				return vText("public." + sq.Name), nil
			}
		}
		return null, nil
	case "pg_relation_size", "pg_total_relation_size", "pg_table_size", "pg_indexes_size":
		if argNull(0) {
			return null, nil
		}
		oid := uint32(a[0].I)
		var pages int
		if t := cat.Tables[oid]; t != nil {
			if name != "pg_indexes_size" {
				pages = s.db.heapPages(t)
			}
			if name == "pg_total_relation_size" || name == "pg_indexes_size" {
				for _, ix := range cat.TableIndexes(t) {
					_, l, err := s.db.store.BTree(ix.Root).Stats()
					if err == nil {
						pages += l
					}
				}
			}
		} else if ix := cat.Indexes[oid]; ix != nil {
			_, l, err := s.db.store.BTree(ix.Root).Stats()
			if err != nil {
				return null, err
			}
			pages = l
		}
		return vInt(int64(pages) * storage.PageSize), nil
	case "pg_database_size":
		return vInt(int64(s.db.store.PageCount()) * storage.PageSize), nil
	case "pg_size_pretty":
		if argNull(0) {
			return null, nil
		}
		return vText(prettySize(a[0].I)), nil
	case "pg_encoding_to_char":
		return vText("UTF8"), nil
	case "has_table_privilege", "has_schema_privilege", "has_database_privilege":
		return vBool(true), nil
	case "pg_is_in_recovery":
		return vBool(false), nil
	case "pg_postmaster_start_time":
		return types.NewTimestampTZ(types.TimestampFromTime(s.db.start)), nil
	case "pg_stat_get_numscans":
		return vInt(0), nil
	case "basalt_stats":
		st := s.db.store
		p := st.Pool()
		return vText(fmt.Sprintf("pages=%d reads=%d writes=%d hits=%d wal_syncs=%d checkpoints=%d commits=%d aborts=%d deadlocks=%d",
			st.PageCount(), p.Reads.Load(), p.Writes.Load(), p.Hits.Load(), st.WAL().Syncs.Load(), st.Checkpoints.Load(),
			s.db.txns.Commits.Load(), s.db.txns.Aborts.Load(), s.db.txns.Locks.Deadlocks)), nil
	}
	return null, pgerr.New(pgerr.UndefinedFunction, "function %s is not implemented", name)
}

func prettySize(n int64) string {
	units := []string{"bytes", "kB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 10*1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d bytes", n)
	}
	return fmt.Sprintf("%.0f %s", v+0.5-0.5, units[i])
}

// regCast converts between reg* types and text/oid.
func (s *Session) regCast(cat *catalog.Catalog, v types.Value, from, to uint32) (types.Value, error) {
	isText := func(o uint32) bool { return types.T{Oid: o}.IsString() }
	switch {
	case isText(from):
		name := strings.TrimSpace(v.S)
		if n, err := strconv.ParseInt(name, 10, 64); err == nil && n >= 0 {
			return vInt(n), nil // a numeric OID
		}
		switch to {
		case types.OidRegclass:
			n := strings.TrimPrefix(strings.TrimPrefix(name, "public."), "pg_catalog.")
			if strings.HasPrefix(n, `"`) {
				n = strings.Trim(n, `"`)
			} else {
				n = strings.ToLower(n)
			}
			if oid, _, ok := cat.Lookup(n); ok {
				return vInt(int64(oid)), nil
			}
			if _, ok := s.virtualTable("pg_catalog", n); ok {
				return vInt(int64(virtualOid(n))), nil
			}
			return null, pgerr.New(pgerr.UndefinedTable, "relation \"%s\" does not exist", name)
		case types.OidRegtype:
			t, ok := types.ByName(strings.ToLower(name))
			if !ok {
				return null, pgerr.New(pgerr.UndefinedObject, "type \"%s\" does not exist", name)
			}
			return vInt(int64(t.Oid)), nil
		case types.OidRegnamespace:
			switch name {
			case "pg_catalog":
				return vInt(oidPgCatalog), nil
			case "public":
				return vInt(oidPublic), nil
			}
			return null, pgerr.New(pgerr.UndefinedObject, "schema \"%s\" does not exist", name)
		case types.OidRegrole:
			return vInt(oidSuperuser), nil
		case types.OidRegproc:
			return vInt(0), nil
		}
	case isText(to):
		switch from {
		case types.OidRegclass:
			if n, _, ok := relInfo(cat, uint32(v.I)); ok {
				return vText(catalog.QuoteIdent(n)), nil
			}
			return vText(fmt.Sprint(v.I)), nil
		case types.OidRegtype:
			if types.Known(uint32(v.I)) {
				return vText(types.T{Oid: uint32(v.I), Mod: -1}.String()), nil
			}
			return vText(fmt.Sprint(v.I)), nil
		case types.OidRegnamespace:
			switch v.I {
			case oidPgCatalog:
				return vText("pg_catalog"), nil
			case oidPublic:
				return vText("public"), nil
			}
		case types.OidRegrole:
			return vText(s.user), nil
		}
		return vText(fmt.Sprint(v.I)), nil
	}
	return v, nil
}

func virtualOid(name string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(name); i++ {
		h = (h ^ uint32(name[i])) * 16777619
	}
	return 1000 + h%10000
}

func (s *Session) constraintDef(cat *catalog.Catalog, oid uint32) (types.Value, error) {
	colList := func(t *catalog.Table, cs []int) string {
		var ns []string
		for _, c := range cs {
			ns = append(ns, catalog.QuoteIdent(t.Columns[c].Name))
		}
		return strings.Join(ns, ", ")
	}
	if ix := cat.Indexes[oid]; ix != nil {
		t := cat.Tables[ix.Table]
		if ix.Primary {
			return vText("PRIMARY KEY (" + colList(t, ix.Columns) + ")"), nil
		}
		return vText("UNIQUE (" + colList(t, ix.Columns) + ")"), nil
	}
	for _, t := range cat.Tables {
		for i, ch := range t.Checks {
			if checkOid(t.Oid, i) == oid {
				return vText("CHECK (" + ch.Expr + ")"), nil
			}
		}
		for i, fk := range t.FKs {
			if fkOid(t.Oid, i) == oid {
				p := cat.Tables[fk.RefTable]
				def := fmt.Sprintf("FOREIGN KEY (%s) REFERENCES %s(%s)", colList(t, fk.Columns), catalog.QuoteIdent(p.Name), colList(p, fk.RefColumns))
				if fk.OnUpdate != "NO ACTION" {
					def += " ON UPDATE " + fk.OnUpdate
				}
				if fk.OnDelete != "NO ACTION" {
					def += " ON DELETE " + fk.OnDelete
				}
				return vText(def), nil
			}
		}
	}
	return null, nil
}
