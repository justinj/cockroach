// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package sessiondata

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/duration"
	yaml "gopkg.in/yaml.v2"
)

// SessionData contains session parameters. They are all user-configurable.
// A SQL Session changes fields in SessionData through sql.sessionDataMutator.
type SessionData struct {
	// ApplicationName is the name of the application running the
	// current session. This can be used for logging and per-application
	// statistics.
	ApplicationName string
	// Database indicates the "current" database for the purpose of
	// resolving names. See searchAndQualifyDatabase() for details.
	Database string
	// DefaultReadOnly indicates the default read-only status of newly created
	// transactions.
	DefaultReadOnly bool
	// DistSQLMode indicates whether to run queries using the distributed
	// execution engine.
	DistSQLMode DistSQLExecMode
	// ForceSplitAt indicates whether checks to prevent incorrect usage of ALTER
	// TABLE ... SPLIT AT should be skipped.
	ForceSplitAt bool
	// OptimizerMode indicates whether to use the experimental optimizer for
	// query planning.
	OptimizerMode OptimizerMode
	// OptimizerMutations indicates whether to use the cost-based optimizer to
	// plan UPDATE statements.
	OptimizerMutations bool
	// SerialNormalizationMode indicates how to handle the SERIAL pseudo-type.
	SerialNormalizationMode SerialNormalizationMode
	// SearchPath is a list of namespaces to search builtins in.
	SearchPath SearchPath
	// StmtTimeout is the duration a query is permitted to run before it is
	// canceled by the session. If set to 0, there is no timeout.
	StmtTimeout time.Duration
	// User is the name of the user logged into the session.
	User string
	// SafeUpdates causes errors when the client
	// sends syntax that may have unwanted side effects.
	SafeUpdates bool
	// RemoteAddr is used to generate logging events.
	RemoteAddr net.Addr
	// ZigzagJoinEnabled indicates whether the optimizer should try and plan a
	// zigzag join.
	ZigzagJoinEnabled bool
	// ReorderJoins indicates whether the optimizer should try to reorder the
	// joins in a query.
	ReorderJoins bool

	OptimizerCostConfig OptimizerCostConfig
	// SequenceState gives access to the SQL sequences that have been manipulated
	// by the session.
	SequenceState *SequenceState
	// DataConversion gives access to the data conversion configuration.
	DataConversion DataConversionConfig
	// DurationAdditionMode enables math compatibility options to be enabled.
	// TODO(bob): Remove this once the 2.2 release branch is cut.
	DurationAdditionMode duration.AdditionMode
	// Vectorize enables automatic planning of vectorized operators.
	Vectorize VectorizeExecMode
	// ForceSavepointRestart overrides the default SAVEPOINT behavior
	// for compatibility with certain ORMs. When this flag is set,
	// the savepoint name will no longer be compared against the magic
	// identifier `cockroach_restart` in order use a restartable
	// transaction.
	ForceSavepointRestart bool
	// DefaultIntSize specifies the size in bits or bytes (preferred)
	// of how a "naked" INT type should be parsed.
	DefaultIntSize int
}

// DataConversionConfig contains the parameters that influence
// the conversion between SQL data types and strings/byte arrays.
type DataConversionConfig struct {
	// Location indicates the current time zone.
	Location *time.Location

	// BytesEncodeFormat indicates how to encode byte arrays when converting
	// to string.
	BytesEncodeFormat BytesEncodeFormat

	// ExtraFloatDigits indicates the number of digits beyond the
	// standard number to use for float conversions.
	// This must be set to a value between -15 and 3, inclusive.
	ExtraFloatDigits int
}

// GetFloatPrec computes a precision suitable for a call to
// strconv.FormatFloat() or for use with '%.*g' in a printf-like
// function.
func (c *DataConversionConfig) GetFloatPrec() int {
	// The user-settable parameter ExtraFloatDigits indicates the number
	// of digits to be used to format the float value. PostgreSQL
	// combines this with %g.
	// The formula is <type>_DIG + extra_float_digits,
	// where <type> is either FLT (float4) or DBL (float8).

	// Also the value "3" in PostgreSQL is special and meant to mean
	// "all the precision needed to reproduce the float exactly". The Go
	// formatter uses the special value -1 for this and activates a
	// separate path in the formatter. We compare >= 3 here
	// just in case the value is not gated properly in the implementation
	// of SET.
	if c.ExtraFloatDigits >= 3 {
		return -1
	}

	// CockroachDB only implements float8 at this time and Go does not
	// expose DBL_DIG, so we use the standard literal constant for
	// 64bit floats.
	const StdDoubleDigits = 15

	nDigits := StdDoubleDigits + c.ExtraFloatDigits
	if nDigits < 1 {
		// Ensure the value is clamped at 1: printf %g does not allow
		// values lower than 1. PostgreSQL does this too.
		nDigits = 1
	}
	return nDigits
}

// Equals returns true if the two DataConversionConfigs are identical.
func (c *DataConversionConfig) Equals(other *DataConversionConfig) bool {
	if c.BytesEncodeFormat != other.BytesEncodeFormat ||
		c.ExtraFloatDigits != other.ExtraFloatDigits {
		return false
	}
	if c.Location != other.Location && c.Location.String() != other.Location.String() {
		return false
	}
	return true
}

// Copy performs a deep copy of SessionData.
func (s *SessionData) Copy() SessionData {
	cp := *s
	cp.SequenceState = s.SequenceState.copy()
	return cp
}

// BytesEncodeFormat controls which format to use for BYTES->STRING
// conversions.
type BytesEncodeFormat int

const (
	// BytesEncodeHex uses the hex format: e'abc\n'::BYTES::STRING -> '\x61626312'.
	// This is the default, for compatibility with PostgreSQL.
	BytesEncodeHex BytesEncodeFormat = iota
	// BytesEncodeEscape uses the escaped format: e'abc\n'::BYTES::STRING -> 'abc\012'.
	BytesEncodeEscape
	// BytesEncodeBase64 uses base64 encoding.
	BytesEncodeBase64
)

func (f BytesEncodeFormat) String() string {
	switch f {
	case BytesEncodeHex:
		return "hex"
	case BytesEncodeEscape:
		return "escape"
	case BytesEncodeBase64:
		return "base64"
	default:
		return fmt.Sprintf("invalid (%d)", f)
	}
}

// BytesEncodeFormatFromString converts a string into a BytesEncodeFormat.
func BytesEncodeFormatFromString(val string) (_ BytesEncodeFormat, ok bool) {
	switch strings.ToUpper(val) {
	case "HEX":
		return BytesEncodeHex, true
	case "ESCAPE":
		return BytesEncodeEscape, true
	case "BASE64":
		return BytesEncodeBase64, true
	default:
		return -1, false
	}
}

// DistSQLExecMode controls if and when the Executor distributes queries.
// Since 2.1, we run everything through the DistSQL infrastructure,
// and these settings control whether to use a distributed plan, or use a plan
// that only involves local DistSQL processors.
type DistSQLExecMode int64

const (
	// DistSQLOff means that we never distribute queries.
	DistSQLOff DistSQLExecMode = iota
	// DistSQLAuto means that we automatically decide on a case-by-case basis if
	// we distribute queries.
	DistSQLAuto
	// DistSQLOn means that we distribute queries that are supported.
	DistSQLOn
	// DistSQLAlways means that we only distribute; unsupported queries fail.
	DistSQLAlways
)

func (m DistSQLExecMode) String() string {
	switch m {
	case DistSQLOff:
		return "off"
	case DistSQLAuto:
		return "auto"
	case DistSQLOn:
		return "on"
	case DistSQLAlways:
		return "always"
	default:
		return fmt.Sprintf("invalid (%d)", m)
	}
}

// DistSQLExecModeFromString converts a string into a DistSQLExecMode
func DistSQLExecModeFromString(val string) (_ DistSQLExecMode, ok bool) {
	switch strings.ToUpper(val) {
	case "OFF":
		return DistSQLOff, true
	case "AUTO":
		return DistSQLAuto, true
	case "ON":
		return DistSQLOn, true
	case "ALWAYS":
		return DistSQLAlways, true
	default:
		return 0, false
	}
}

// OptimizerCostConfig controls the configuration of the optimizer cost model.
type OptimizerCostConfig struct {
	CpuCostFactor    float64
	SeqIOCostFactor  float64
	RandIOCostFactor float64

	LookupJoinDisabled bool
	MergeJoinDisabled  bool
	HashJoinDisabled   bool
}

// todo: comment
var DefaultCostConfig = OptimizerCostConfig{
	CpuCostFactor:    0.01,
	SeqIOCostFactor:  1,
	RandIOCostFactor: 4,
}

func (d OptimizerCostConfig) Encode() string {
	result := make(map[string]string)
	if d.CpuCostFactor != DefaultCostConfig.CpuCostFactor {
		result["CpuCostFactor"] = fmt.Sprintf("%f", d.CpuCostFactor)
	}
	if d.RandIOCostFactor != DefaultCostConfig.RandIOCostFactor {
		result["RandIOCostFactor"] = fmt.Sprintf("%f", d.RandIOCostFactor)
	}
	if d.SeqIOCostFactor != DefaultCostConfig.SeqIOCostFactor {
		result["SeqIOCostFactor"] = fmt.Sprintf("%f", d.SeqIOCostFactor)
	}
	if d.LookupJoinDisabled {
		result["LookupJoinDisabled"] = "true"
	}
	if d.MergeJoinDisabled {
		result["MergeJoinDisabled"] = "true"
	}
	if d.HashJoinDisabled {
		result["HashJoinDisabled"] = "true"
	}
	encoded, err := yaml.Marshal(result)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// VectorizeExecMode controls if an when the Executor executes queries using the
// columnar execution engine.
type VectorizeExecMode int64

const (
	// VectorizeOff means that columnar execution is disabled.
	VectorizeOff VectorizeExecMode = iota
	// VectorizeOn means that any supported queries will be run using the columnar
	// execution on.
	VectorizeOn
	// VectorizeAlways means that we attempt to vectorize all queries; unsupported
	// queries will fail. Mostly used for testing.
	VectorizeAlways
)

func (m VectorizeExecMode) String() string {
	switch m {
	case VectorizeOff:
		return "off"
	case VectorizeOn:
		return "on"
	case VectorizeAlways:
		return "always"
	default:
		return fmt.Sprintf("invalid (%d)", m)
	}
}

// VectorizeExecModeFromSring converts a string into a VectorizeExecMode. False
// is returned if the conversion was unsuccessful.
func VectorizeExecModeFromString(val string) (VectorizeExecMode, bool) {
	var m VectorizeExecMode
	switch strings.ToUpper(val) {
	case "OFF":
		m = VectorizeOff
	case "ON":
		m = VectorizeOn
	case "ALWAYS":
		m = VectorizeAlways
	default:
		return 0, false
	}
	return m, true
}

// OptimizerMode controls if and when the Executor uses the optimizer.
type OptimizerMode int64

const (
	// OptimizerOff means that we don't use the optimizer.
	OptimizerOff = iota
	// OptimizerOn means that we use the optimizer for all supported statements.
	OptimizerOn
	// OptimizerLocal means that we use the optimizer for all supported
	// statements, but we don't try to distribute the resulting plan.
	OptimizerLocal
	// OptimizerAlways means that we attempt to use the optimizer always, even
	// for unsupported statements which result in errors. This mode is useful
	// for testing.
	OptimizerAlways
)

func (m OptimizerMode) String() string {
	switch m {
	case OptimizerOff:
		return "off"
	case OptimizerOn:
		return "on"
	case OptimizerLocal:
		return "local"
	case OptimizerAlways:
		return "always"
	default:
		return fmt.Sprintf("invalid (%d)", m)
	}
}

// OptimizerModeFromString converts a string into a OptimizerMode
func OptimizerModeFromString(val string) (_ OptimizerMode, ok bool) {
	switch strings.ToUpper(val) {
	case "OFF":
		return OptimizerOff, true
	case "ON":
		return OptimizerOn, true
	case "LOCAL":
		return OptimizerLocal, true
	case "ALWAYS":
		return OptimizerAlways, true
	default:
		return 0, false
	}
}

// SerialNormalizationMode controls if and when the Executor uses DistSQL.
type SerialNormalizationMode int64

const (
	// SerialUsesRowID means use INT NOT NULL DEFAULT unique_rowid().
	SerialUsesRowID SerialNormalizationMode = iota
	// SerialUsesVirtualSequences means create a virtual sequence and
	// use INT NOT NULL DEFAULT nextval(...).
	SerialUsesVirtualSequences
	// SerialUsesSQLSequences means create a regular SQL sequence and
	// use INT NOT NULL DEFAULT nextval(...).
	SerialUsesSQLSequences
)

func (m SerialNormalizationMode) String() string {
	switch m {
	case SerialUsesRowID:
		return "rowid"
	case SerialUsesVirtualSequences:
		return "virtual_sequence"
	case SerialUsesSQLSequences:
		return "sql_sequence"
	default:
		return fmt.Sprintf("invalid (%d)", m)
	}
}

// SerialNormalizationModeFromString converts a string into a SerialNormalizationMode
func SerialNormalizationModeFromString(val string) (_ SerialNormalizationMode, ok bool) {
	switch strings.ToUpper(val) {
	case "ROWID":
		return SerialUsesRowID, true
	case "VIRTUAL_SEQUENCE":
		return SerialUsesVirtualSequences, true
	case "SQL_SEQUENCE":
		return SerialUsesSQLSequences, true
	default:
		return 0, false
	}
}
