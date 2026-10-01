// Package types implements basalt's SQL data types: the in-memory value
// representation, PostgreSQL type OIDs, text and binary wire formats,
// casts, arithmetic, and the byte encodings used on disk (rows) and in
// indexes (order-preserving keys).
package types

import (
	"fmt"
	"strings"
)

// PostgreSQL type OIDs. basalt uses the same OIDs as PostgreSQL so that
// clients can decode results without any knowledge of basalt.
const (
	OidBool         uint32 = 16
	OidBytea        uint32 = 17
	OidChar         uint32 = 18 // "char", a single byte
	OidName         uint32 = 19
	OidInt8         uint32 = 20
	OidInt2         uint32 = 21
	OidInt2Vector   uint32 = 22
	OidInt4         uint32 = 23
	OidRegproc      uint32 = 24
	OidText         uint32 = 25
	OidOid          uint32 = 26
	OidOidVector    uint32 = 30
	OidJSON         uint32 = 114
	OidPgNodeTree   uint32 = 194
	OidFloat4       uint32 = 700
	OidFloat8       uint32 = 701
	OidUnknown      uint32 = 705
	OidBoolArray    uint32 = 1000
	OidByteaArray   uint32 = 1001
	OidCharArray    uint32 = 1002
	OidNameArray    uint32 = 1003
	OidInt2Array    uint32 = 1005
	OidInt4Array    uint32 = 1007
	OidTextArray    uint32 = 1009
	OidVarcharArr   uint32 = 1015
	OidInt8Array    uint32 = 1016
	OidFloat4Array  uint32 = 1021
	OidFloat8Array  uint32 = 1022
	OidOidArray     uint32 = 1028
	OidAclItemArr   uint32 = 1034
	OidBpchar       uint32 = 1042
	OidVarchar      uint32 = 1043
	OidDate         uint32 = 1082
	OidTime         uint32 = 1083
	OidTimestamp    uint32 = 1114
	OidTSArray      uint32 = 1115
	OidDateArray    uint32 = 1182
	OidTimestampTZ  uint32 = 1184
	OidTSTZArray    uint32 = 1185
	OidInterval     uint32 = 1186
	OidNumericArr   uint32 = 1231
	OidNumeric      uint32 = 1700
	OidRegclass     uint32 = 2205
	OidRegtype      uint32 = 2206
	OidRecord       uint32 = 2249
	OidAny          uint32 = 2276
	OidAnyArray     uint32 = 2277
	OidVoid         uint32 = 2278
	OidAnyElement   uint32 = 2283
	OidRegnamespace uint32 = 4089
	OidRegrole      uint32 = 4096
)

// T is a SQL type: a PostgreSQL type OID plus a type modifier (for example
// the length of varchar(n) or the precision and scale of numeric(p,s)).
// Mod is -1 when the type has no modifier.
type T struct {
	Oid uint32
	Mod int32
}

// Common types.
var (
	Unknown      = T{OidUnknown, -1}
	Bool         = T{OidBool, -1}
	Int2         = T{OidInt2, -1}
	Int4         = T{OidInt4, -1}
	Int8         = T{OidInt8, -1}
	Float4       = T{OidFloat4, -1}
	Float8       = T{OidFloat8, -1}
	Numeric      = T{OidNumeric, -1}
	Text         = T{OidText, -1}
	Varchar      = T{OidVarchar, -1}
	Bpchar       = T{OidBpchar, -1}
	Name         = T{OidName, -1}
	Char         = T{OidChar, -1}
	Bytea        = T{OidBytea, -1}
	Date         = T{OidDate, -1}
	Timestamp    = T{OidTimestamp, -1}
	TimestampTZ  = T{OidTimestampTZ, -1}
	IntervalT    = T{OidInterval, -1}
	Oid          = T{OidOid, -1}
	Regclass     = T{OidRegclass, -1}
	Regtype      = T{OidRegtype, -1}
	Regproc      = T{OidRegproc, -1}
	Regnamespace = T{OidRegnamespace, -1}
	Regrole      = T{OidRegrole, -1}
	Void         = T{OidVoid, -1}
	Record       = T{OidRecord, -1}
	Int2Vector   = T{OidInt2Vector, -1}
	OidVector    = T{OidOidVector, -1}
	PgNodeTree   = T{OidPgNodeTree, -1}
)

// VarcharN returns varchar(n).
func VarcharN(n int) T { return T{OidVarchar, int32(n) + 4} }

// BpcharN returns char(n).
func BpcharN(n int) T { return T{OidBpchar, int32(n) + 4} }

// NumericPS returns numeric(p, s).
func NumericPS(p, s int) T { return T{OidNumeric, int32(p<<16|s) + 4} }

type typeInfo struct {
	name    string // pg_type.typname
	sqlName string // format_type output
	kind    Kind
	size    int16 // typlen
	elem    uint32
	array   uint32
	cat     byte // typcategory
}

var typeTable = map[uint32]*typeInfo{
	OidBool:         {"bool", "boolean", KBool, 1, 0, OidBoolArray, 'B'},
	OidBytea:        {"bytea", "bytea", KBytea, -1, 0, OidByteaArray, 'U'},
	OidChar:         {"char", `"char"`, KText, 1, 0, OidCharArray, 'Z'},
	OidName:         {"name", "name", KText, 64, 0, OidNameArray, 'S'},
	OidInt8:         {"int8", "bigint", KInt, 8, 0, OidInt8Array, 'N'},
	OidInt2:         {"int2", "smallint", KInt, 2, 0, OidInt2Array, 'N'},
	OidInt2Vector:   {"int2vector", "int2vector", KArray, -1, OidInt2, 0, 'A'},
	OidInt4:         {"int4", "integer", KInt, 4, 0, OidInt4Array, 'N'},
	OidRegproc:      {"regproc", "regproc", KInt, 4, 0, 0, 'N'},
	OidText:         {"text", "text", KText, -1, 0, OidTextArray, 'S'},
	OidOid:          {"oid", "oid", KInt, 4, 0, OidOidArray, 'N'},
	OidOidVector:    {"oidvector", "oidvector", KArray, -1, OidOid, 0, 'A'},
	OidJSON:         {"json", "json", KText, -1, 0, 0, 'U'},
	OidPgNodeTree:   {"pg_node_tree", "pg_node_tree", KText, -1, 0, 0, 'S'},
	OidFloat4:       {"float4", "real", KFloat, 4, 0, OidFloat4Array, 'N'},
	OidFloat8:       {"float8", "double precision", KFloat, 8, 0, OidFloat8Array, 'N'},
	OidUnknown:      {"unknown", "unknown", KText, -2, 0, 0, 'X'},
	OidBoolArray:    {"_bool", "boolean[]", KArray, -1, OidBool, 0, 'A'},
	OidByteaArray:   {"_bytea", "bytea[]", KArray, -1, OidBytea, 0, 'A'},
	OidCharArray:    {"_char", `"char"[]`, KArray, -1, OidChar, 0, 'A'},
	OidNameArray:    {"_name", "name[]", KArray, -1, OidName, 0, 'A'},
	OidInt2Array:    {"_int2", "smallint[]", KArray, -1, OidInt2, 0, 'A'},
	OidInt4Array:    {"_int4", "integer[]", KArray, -1, OidInt4, 0, 'A'},
	OidTextArray:    {"_text", "text[]", KArray, -1, OidText, 0, 'A'},
	OidVarcharArr:   {"_varchar", "character varying[]", KArray, -1, OidVarchar, 0, 'A'},
	OidInt8Array:    {"_int8", "bigint[]", KArray, -1, OidInt8, 0, 'A'},
	OidFloat4Array:  {"_float4", "real[]", KArray, -1, OidFloat4, 0, 'A'},
	OidFloat8Array:  {"_float8", "double precision[]", KArray, -1, OidFloat8, 0, 'A'},
	OidOidArray:     {"_oid", "oid[]", KArray, -1, OidOid, 0, 'A'},
	OidAclItemArr:   {"_aclitem", "aclitem[]", KArray, -1, OidText, 0, 'A'},
	OidBpchar:       {"bpchar", "character", KText, -1, 0, 0, 'S'},
	OidVarchar:      {"varchar", "character varying", KText, -1, 0, OidVarcharArr, 'S'},
	OidDate:         {"date", "date", KDate, 4, 0, OidDateArray, 'D'},
	OidTimestamp:    {"timestamp", "timestamp without time zone", KTimestamp, 8, 0, OidTSArray, 'D'},
	OidTSArray:      {"_timestamp", "timestamp without time zone[]", KArray, -1, OidTimestamp, 0, 'A'},
	OidDateArray:    {"_date", "date[]", KArray, -1, OidDate, 0, 'A'},
	OidTimestampTZ:  {"timestamptz", "timestamp with time zone", KTimestampTZ, 8, 0, OidTSTZArray, 'D'},
	OidTSTZArray:    {"_timestamptz", "timestamp with time zone[]", KArray, -1, OidTimestampTZ, 0, 'A'},
	OidInterval:     {"interval", "interval", KInterval, 16, 0, 0, 'T'},
	OidNumericArr:   {"_numeric", "numeric[]", KArray, -1, OidNumeric, 0, 'A'},
	OidNumeric:      {"numeric", "numeric", KNumeric, -1, 0, OidNumericArr, 'N'},
	OidRegclass:     {"regclass", "regclass", KInt, 4, 0, 0, 'N'},
	OidRegtype:      {"regtype", "regtype", KInt, 4, 0, 0, 'N'},
	OidRegnamespace: {"regnamespace", "regnamespace", KInt, 4, 0, 0, 'N'},
	OidRegrole:      {"regrole", "regrole", KInt, 4, 0, 0, 'N'},
	OidRecord:       {"record", "record", KArray, -1, 0, 0, 'P'},
	OidAny:          {"any", `"any"`, KNull, 4, 0, 0, 'P'},
	OidAnyArray:     {"anyarray", "anyarray", KArray, -1, 0, 0, 'P'},
	OidVoid:         {"void", "void", KNull, 4, 0, 0, 'P'},
	OidAnyElement:   {"anyelement", "anyelement", KNull, 4, 0, 0, 'P'},
}

// AllTypeOids lists the OIDs basalt knows, for pg_type emulation.
func AllTypeOids() []uint32 {
	out := make([]uint32, 0, len(typeTable))
	for oid := range typeTable {
		out = append(out, oid)
	}
	return out
}

func info(oid uint32) *typeInfo {
	if ti, ok := typeTable[oid]; ok {
		return ti
	}
	return typeTable[OidUnknown]
}

// Known reports whether basalt knows the type.
func Known(oid uint32) bool { _, ok := typeTable[oid]; return ok }

// Kind returns the runtime representation used for values of type t.
func (t T) Kind() Kind { return info(t.Oid).kind }

// IsArray reports whether t is an array type.
func (t T) IsArray() bool { return info(t.Oid).kind == KArray && t.Oid != OidRecord }

// Elem returns the element type of an array type.
func (t T) Elem() T { return T{info(t.Oid).elem, -1} }

// ArrayOf returns the array type whose elements are t, or false if none.
func ArrayOf(t T) (T, bool) {
	a := info(t.Oid).array
	if a == 0 {
		return Unknown, false
	}
	return T{a, -1}, true
}

// TypName returns pg_type.typname.
func (t T) TypName() string { return info(t.Oid).name }

// Len returns pg_type.typlen.
func (t T) Len() int16 { return info(t.Oid).size }

// Category returns pg_type.typcategory.
func (t T) Category() byte { return info(t.Oid).cat }

// IsNumeric reports whether t is one of the numeric types.
func (t T) IsNumeric() bool {
	switch t.Oid {
	case OidInt2, OidInt4, OidInt8, OidFloat4, OidFloat8, OidNumeric, OidOid:
		return true
	}
	return false
}

// IsInteger reports whether t is an integer type.
func (t T) IsInteger() bool {
	switch t.Oid {
	case OidInt2, OidInt4, OidInt8, OidOid, OidRegclass, OidRegtype, OidRegproc, OidRegnamespace, OidRegrole:
		return true
	}
	return false
}

// IsString reports whether t is a character string type.
func (t T) IsString() bool {
	switch t.Oid {
	case OidText, OidVarchar, OidBpchar, OidName, OidChar, OidUnknown:
		return true
	}
	return false
}

// String returns the SQL name of the type as format_type prints it.
func (t T) String() string {
	ti := info(t.Oid)
	name := ti.sqlName
	if t.Mod >= 4 {
		switch t.Oid {
		case OidVarchar:
			return fmt.Sprintf("character varying(%d)", t.Mod-4)
		case OidBpchar:
			return fmt.Sprintf("character(%d)", t.Mod-4)
		case OidNumeric:
			m := t.Mod - 4
			return fmt.Sprintf("numeric(%d,%d)", m>>16, m&0xffff)
		}
	}
	if t.Oid == OidBpchar && t.Mod < 0 {
		return "bpchar"
	}
	return name
}

// NumericPrecScale returns the declared precision and scale of a numeric
// type, or ok=false when it is unconstrained.
func (t T) NumericPrecScale() (prec, scale int, ok bool) {
	if t.Oid != OidNumeric || t.Mod < 4 {
		return 0, 0, false
	}
	m := t.Mod - 4
	return int(m >> 16), int(m & 0xffff), true
}

// CharLen returns the declared length of varchar(n) / char(n), or -1.
func (t T) CharLen() int {
	if (t.Oid == OidVarchar || t.Oid == OidBpchar) && t.Mod >= 4 {
		return int(t.Mod - 4)
	}
	return -1
}

// ByName resolves a SQL type name (already lower-cased, possibly with a
// pg_catalog. prefix) to a type. Modifiers are applied by the caller.
func ByName(name string) (T, bool) {
	name = strings.TrimPrefix(name, "pg_catalog.")
	switch name {
	case "bool", "boolean":
		return Bool, true
	case "int2", "smallint", "smallserial", "serial2":
		return Int2, true
	case "int", "int4", "integer", "serial", "serial4":
		return Int4, true
	case "int8", "bigint", "bigserial", "serial8":
		return Int8, true
	case "float4", "real":
		return Float4, true
	case "float8", "float", "double precision", "double":
		return Float8, true
	case "numeric", "decimal", "dec":
		return Numeric, true
	case "text", "string":
		return Text, true
	case "varchar", "character varying":
		return Varchar, true
	case "char", "character", "bpchar", "nchar":
		return Bpchar, true
	case `"char"`:
		return Char, true
	case "name":
		return Name, true
	case "bytea", "blob":
		return Bytea, true
	case "date":
		return Date, true
	case "timestamp", "timestamp without time zone", "datetime":
		return Timestamp, true
	case "timestamptz", "timestamp with time zone":
		return TimestampTZ, true
	case "interval":
		return IntervalT, true
	case "oid":
		return Oid, true
	case "regclass":
		return Regclass, true
	case "regtype":
		return Regtype, true
	case "regproc", "regprocedure":
		return Regproc, true
	case "regnamespace":
		return Regnamespace, true
	case "regrole":
		return Regrole, true
	case "unknown":
		return Unknown, true
	case "json", "jsonb":
		return T{OidJSON, -1}, true
	case "void":
		return Void, true
	case "record":
		return Record, true
	case "int2vector":
		return Int2Vector, true
	case "oidvector":
		return OidVector, true
	case "pg_node_tree":
		return PgNodeTree, true
	case "anyarray":
		return T{OidAnyArray, -1}, true
	}
	return Unknown, false
}

// IsSerial reports whether a type name is one of the serial pseudo types.
func IsSerial(name string) bool {
	switch name {
	case "serial", "serial4", "bigserial", "serial8", "smallserial", "serial2":
		return true
	}
	return false
}
