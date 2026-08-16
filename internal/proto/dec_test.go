package proto

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func TestParseDecRejectsNonCanonical(t *testing.T) {
	// Canonical form is load-bearing: entity revisions live in a TEXT unique
	// index, so "1" and "01" would be two rows for one logical revision.
	for _, s := range []string{
		"", " ", "1 ", " 1", "01", "007", "+1", "-1", "1.0", "1e3",
		"0x1", "abc", "1_000", "١٢٣", "\n1",
	} {
		if _, err := ParseDec(s); err == nil {
			t.Errorf("ParseDec(%q) accepted a non-canonical value", s)
		}
	}
}

func TestParseDecAccepts(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Dec
	}{
		{"0", 0},
		{"1", 1},
		{"403", 403},
		{"18446744073709551615", MaxDec},
	} {
		got, err := ParseDec(tc.in)
		if err != nil {
			t.Fatalf("ParseDec(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseDec(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseDecOverflow(t *testing.T) {
	// One past uint64 max.
	if _, err := ParseDec("18446744073709551616"); err == nil {
		t.Error("ParseDec accepted a value above uint64")
	}
	if _, err := ParseDec("99999999999999999999999"); err == nil {
		t.Error("ParseDec accepted a wildly out-of-range value")
	}
}

func TestParseDecPositive(t *testing.T) {
	if _, err := ParseDecPositive("0"); err == nil {
		t.Error(`ParseDecPositive("0") should fail: the first op is seq 1`)
	}
	if d, err := ParseDecPositive("1"); err != nil || d != 1 {
		t.Errorf("ParseDecPositive(\"1\") = %d, %v", d, err)
	}
}

func TestInt64Boundary(t *testing.T) {
	// SQLite INTEGER is signed. A plain conversion above MaxInt64 wraps
	// negative, which would turn a large cursor into one matching no rows.
	max := Dec(math.MaxInt64)
	if v, err := max.Int64(); err != nil || v != math.MaxInt64 {
		t.Fatalf("Int64 at MaxInt64 = %d, %v", v, err)
	}
	if _, err := (max + 1).Int64(); err == nil {
		t.Fatal("Int64 above MaxInt64 must fail rather than wrap")
	}
	if _, err := MaxDec.Int64(); err == nil {
		t.Fatal("Int64 at uint64 max must fail rather than wrap")
	}
}

func TestDecFromInt64RejectsNegative(t *testing.T) {
	if _, err := DecFromInt64(-1); err == nil {
		t.Fatal("DecFromInt64(-1) should fail")
	}
}

func TestMarshalJSONIsAlwaysQuoted(t *testing.T) {
	b, err := json.Marshal(Dec(403))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"403"` {
		t.Fatalf("Marshal = %s, want a quoted decimal", b)
	}

	// Nested in a struct, the way a response is built.
	type env struct {
		HeadSeq Dec `json:"head_seq"`
	}
	b, err = json.Marshal(env{HeadSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"head_seq":"1"}` {
		t.Fatalf("Marshal = %s, want head_seq as a string", b)
	}
}

func TestUnmarshalJSONRejectsNumbers(t *testing.T) {
	// A JSON number here is exactly what wedges the client: it throws while
	// applying the page, the cursor never advances, and that page retries forever.
	var d Dec
	if err := json.Unmarshal([]byte(`403`), &d); err == nil {
		t.Fatal("Unmarshal accepted a JSON number")
	}
	if err := json.Unmarshal([]byte(`null`), &d); err == nil {
		t.Fatal("Unmarshal accepted null")
	}
	if err := json.Unmarshal([]byte(`"01"`), &d); err == nil {
		t.Fatal("Unmarshal accepted a non-canonical string")
	}
	if err := json.Unmarshal([]byte(`"403"`), &d); err != nil || d != 403 {
		t.Fatalf("Unmarshal(\"403\") = %d, %v", d, err)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, want := range []Dec{0, 1, 42, math.MaxInt64, MaxDec} {
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got Dec
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("round trip of %d: %v", want, err)
		}
		if got != want {
			t.Errorf("round trip of %d produced %d", want, got)
		}
	}
}

func TestDecIsUnsigned(t *testing.T) {
	// Guards against someone redefining Dec as int64 later; the wire contract
	// permits values above MaxInt64 even though our sequences never reach them.
	if reflect.TypeOf(Dec(0)).Kind() != reflect.Uint64 {
		t.Fatal("Dec must be backed by uint64")
	}
}
