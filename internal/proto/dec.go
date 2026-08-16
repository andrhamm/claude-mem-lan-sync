package proto

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Dec is an unsigned integer that crosses the wire as a canonical decimal
// string, never a JSON number.
//
// claude-mem's client validates every sequence, revision, epoch, and timestamp
// with a strict decimal-string check and rejects JSON numbers outright. A
// number where a string belongs makes the client throw while applying a page,
// which rolls the page back, leaves the cursor unmoved, and retries the same
// page forever — a permanently wedged device with no error surfaced to the user.
// Routing every such value through this one type is what keeps a handler from
// emitting a bare integer by accident.
type Dec uint64

// MaxDec is the largest value the protocol permits (uint64 max).
const MaxDec = Dec(math.MaxUint64)

var errNotCanonical = errors.New("proto: decimal must be an unsigned base-10 string without leading zeroes")

// ParseDec accepts a canonical decimal string: "0" or a digit string with no
// leading zero, no sign, no whitespace, no exponent, within uint64.
//
// Canonical form is enforced here rather than at the storage layer because
// entity revisions are stored in a TEXT unique index: "1" and "01" would become
// two rows for one logical revision, which breaks first-write-wins dedupe and
// lets a retry consume a fresh sequence number.
func ParseDec(s string) (Dec, error) {
	if !isCanonicalDecimal(s) {
		return 0, errNotCanonical
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("proto: decimal out of range: %w", err)
	}
	return Dec(v), nil
}

// ParseDecPositive additionally rejects "0". Sequence numbers and entity
// revisions must be positive; the hub's first op is seq 1.
func ParseDecPositive(s string) (Dec, error) {
	d, err := ParseDec(s)
	if err != nil {
		return 0, err
	}
	if d == 0 {
		return 0, errors.New("proto: decimal must be positive")
	}
	return d, nil
}

func isCanonicalDecimal(s string) bool {
	if s == "" {
		return false
	}
	if s == "0" {
		return true
	}
	if s[0] == '0' {
		return false // leading zero
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (d Dec) String() string { return strconv.FormatUint(uint64(d), 10) }

// Int64 converts for SQLite storage, which has no unsigned integer type.
//
// A value above MaxInt64 would silently wrap to a negative on a plain
// conversion, turning a large cursor into one that matches no rows. Sequence
// allocation therefore stops at MaxInt64 with an error rather than wrapping.
func (d Dec) Int64() (int64, error) {
	if uint64(d) > math.MaxInt64 {
		return 0, fmt.Errorf("proto: %s exceeds the signed 64-bit range SQLite can store", d)
	}
	return int64(d), nil
}

// DecFromInt64 converts a stored value back.
func DecFromInt64(v int64) (Dec, error) {
	if v < 0 {
		return 0, fmt.Errorf("proto: negative sequence %d in storage", v)
	}
	return Dec(v), nil
}

// MarshalJSON always emits a quoted decimal.
func (d Dec) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.String() + `"`), nil
}

// UnmarshalJSON accepts only a quoted canonical decimal. A JSON number is an
// error, matching the client's own validator.
func (d *Dec) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("proto: decimal must be a JSON string: %w", err)
	}
	v, err := ParseDec(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}
