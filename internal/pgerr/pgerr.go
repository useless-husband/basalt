// Package pgerr defines the error type used throughout basalt. Every error
// that can reach a client carries a PostgreSQL SQLSTATE code so that drivers
// can react to it (for example retry on 40001 or 40P01).
package pgerr

import (
	"errors"
	"fmt"
)

// SQLSTATE codes used by basalt. The names follow PostgreSQL's errcodes.txt.
const (
	SuccessfulCompletion         = "00000"
	FeatureNotSupported          = "0A000"
	CardinalityViolation         = "21000"
	DataException                = "22000"
	StringDataRightTruncation    = "22001"
	NumericValueOutOfRange       = "22003"
	NullValueNotAllowed          = "22004"
	InvalidDatetimeFormat        = "22007"
	DatetimeFieldOverflow        = "22008"
	DivisionByZero               = "22012"
	InvalidParameterValue        = "22023"
	InvalidTextRepresentation    = "22P02"
	InvalidBinaryRepresentation  = "22P03"
	InvalidRegularExpression     = "2201B"
	InvalidRowCountInLimit       = "2201W"
	InvalidRowCountInOffset      = "2201X"
	ArraySubscriptError          = "2202E"
	IntegrityConstraintViolation = "23000"
	NotNullViolation             = "23502"
	ForeignKeyViolation          = "23503"
	UniqueViolation              = "23505"
	CheckViolation               = "23514"
	ActiveSQLTransaction         = "25001"
	NoActiveSQLTransaction       = "25P01"
	InFailedSQLTransaction       = "25P02"
	ReadOnlySQLTransaction       = "25006"
	InvalidSQLStatementName      = "26000"
	InvalidCursorName            = "34000"
	SerializationFailure         = "40001"
	DeadlockDetected             = "40P01"
	SyntaxError                  = "42601"
	InsufficientPrivilege        = "42501"
	UndefinedColumn              = "42703"
	UndefinedFunction            = "42883"
	UndefinedTable               = "42P01"
	UndefinedParameter           = "42P02"
	UndefinedObject              = "42704"
	DuplicateColumn              = "42701"
	DuplicateTable               = "42P07"
	DuplicateObject              = "42710"
	AmbiguousColumn              = "42702"
	AmbiguousFunction            = "42725"
	DatatypeMismatch             = "42804"
	WrongObjectType              = "42809"
	InvalidColumnReference       = "42P10"
	GroupingError                = "42803"
	InvalidTableDefinition       = "42P16"
	IndeterminateDatatype        = "42P18"
	CannotCoerce                 = "42846"
	ProgramLimitExceeded         = "54000"
	StatementTooComplex          = "54001"
	ObjectInUse                  = "55006"
	QueryCanceled                = "57014"
	AdminShutdown                = "57P01"
	ProtocolViolation            = "08P01"
	ConnectionFailure            = "08006"
	InternalError                = "XX000"
	DataCorrupted                = "XX001"
	IOError                      = "58030"
	OutOfMemory                  = "53200"
	ConfigurationLimitExceeded   = "53400"
)

// Error is an error with a SQLSTATE code and optional detail and hint.
type Error struct {
	Code       string
	Message    string
	Detail     string
	Hint       string
	Severity   string // defaults to ERROR
	Position   int    // 1-based character position in the query, 0 if unknown
	Table      string
	Column     string
	Constraint string
}

func (e *Error) Error() string { return e.Message }

// New returns an error with the given code and formatted message.
func New(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithDetail returns e with a detail line set.
func (e *Error) WithDetail(format string, args ...any) *Error {
	e.Detail = fmt.Sprintf(format, args...)
	return e
}

// WithHint returns e with a hint line set.
func (e *Error) WithHint(format string, args ...any) *Error {
	e.Hint = fmt.Sprintf(format, args...)
	return e
}

// Code returns the SQLSTATE of err, or XX000 if err does not carry one.
func Code(err error) string {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return InternalError
}

// As converts any error to an *Error, wrapping unknown errors as internal errors.
func As(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	return &Error{Code: InternalError, Message: err.Error()}
}

// Internal reports a bug or an unexpected condition.
func Internal(format string, args ...any) *Error {
	return New(InternalError, format, args...)
}

// Unsupported reports a feature basalt does not implement.
func Unsupported(format string, args ...any) *Error {
	return New(FeatureNotSupported, format, args...)
}
