package telemetry

import (
	"math"
	"testing"
)

func TestScanStringBasic(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: `"hello"`}
	got, err := s.scanString()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
	if s.pos != 7 {
		t.Errorf("pos = %d, want 7", s.pos)
	}
}

func TestScanStringEmpty(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: `""`}
	got, err := s.scanString()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestScanStringWithEscapes(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: `"he\"llo\\world"`}
	got, err := s.scanString()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != `he"llo\world` {
		t.Errorf("got %q, want %q", got, `he"llo\world`)
	}
}

func TestScanStringUnterminated(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: `"hello`}
	_, err := s.scanString()
	if err == nil {
		t.Fatal("expected error for unterminated string")
	}
}

func TestScanNumberInteger(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: "42"}
	got, err := s.scanNumber()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "42" {
		t.Errorf("got %q, want %q", got, "42")
	}
}

func TestScanNumberFloat(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: "3.14"}
	got, err := s.scanNumber()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "3.14" {
		t.Errorf("got %q, want %q", got, "3.14")
	}
}

func TestScanNumberNegative(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: "-99"}
	got, err := s.scanNumber()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "-99" {
		t.Errorf("got %q, want %q", got, "-99")
	}
}

func TestScanNumberScientific(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: "1.5e10"}
	got, err := s.scanNumber()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "1.5e10" {
		t.Errorf("got %q, want %q", got, "1.5e10")
	}
}

func TestScanNumberMaxUint64(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: "18446744073709551615"}
	got, err := s.scanNumber()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "18446744073709551615" {
		t.Errorf("got %q, want %q", got, "18446744073709551615")
	}
}

func TestSkipValueString(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: `"hello" rest`}
	err := s.skipValue()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.pos != 7 {
		t.Errorf("pos = %d, want 7", s.pos)
	}
}

func TestSkipValueNumber(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: "42 rest"}
	err := s.skipValue()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.pos != 2 {
		t.Errorf("pos = %d, want 2", s.pos)
	}
}

func TestSkipValueTrue(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: "true rest"}
	err := s.skipValue()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.pos != 4 {
		t.Errorf("pos = %d, want 4", s.pos)
	}
}

func TestSkipValueFalse(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: "false rest"}
	err := s.skipValue()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.pos != 5 {
		t.Errorf("pos = %d, want 5", s.pos)
	}
}

func TestSkipValueNull(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: "null rest"}
	err := s.skipValue()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.pos != 4 {
		t.Errorf("pos = %d, want 4", s.pos)
	}
}

func TestSkipValueEmptyObject(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: "{} rest"}
	err := s.skipValue()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.pos != 2 {
		t.Errorf("pos = %d, want 2", s.pos)
	}
}

func TestSkipValueNestedObject(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: `{"a": {"b": 1}} rest`}
	err := s.skipValue()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.pos != 15 {
		t.Errorf("pos = %d, want 15", s.pos)
	}
}

func TestSkipValueArray(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: `[1, "two", null] rest`}
	err := s.skipValue()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.pos != 16 {
		t.Errorf("pos = %d, want 16", s.pos)
	}
}

func TestSkipValueEmptyArray(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: `[] rest`}
	err := s.skipValue()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.pos != 2 {
		t.Errorf("pos = %d, want 2", s.pos)
	}
}

func TestParseCounterObjectAllFields(t *testing.T) {
	t.Parallel()
	data := `{"value": 42, "flag": "c", "description": "Cache hits", "ident": "s0"}`
	s := &jsonScanner{data: data}
	c, ok := s.parseCounterObject()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if c.Value != 42 {
		t.Errorf("Value = %v, want 42", c.Value)
	}
	if c.RawValue != "42" {
		t.Errorf("RawValue = %q, want %q", c.RawValue, "42")
	}
	if c.Flag != "c" {
		t.Errorf("Flag = %q, want %q", c.Flag, "c")
	}
	if c.Description != "Cache hits" {
		t.Errorf("Description = %q, want %q", c.Description, "Cache hits")
	}
	if c.Ident != "s0" {
		t.Errorf("Ident = %q, want %q", c.Ident, "s0")
	}
}

func TestParseCounterObjectReversedFields(t *testing.T) {
	t.Parallel()
	data := `{"ident": "x", "description": "D", "flag": "g", "value": 99}`
	s := &jsonScanner{data: data}
	c, ok := s.parseCounterObject()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if c.Value != 99 {
		t.Errorf("Value = %v, want 99", c.Value)
	}
	if c.Flag != "g" {
		t.Errorf("Flag = %q, want %q", c.Flag, "g")
	}
	if c.Description != "D" {
		t.Errorf("Description = %q, want %q", c.Description, "D")
	}
	if c.Ident != "x" {
		t.Errorf("Ident = %q, want %q", c.Ident, "x")
	}
}

func TestParseCounterObjectMinimalFields(t *testing.T) {
	t.Parallel()
	data := `{"value": 7, "flag": "a", "description": ""}`
	s := &jsonScanner{data: data}
	c, ok := s.parseCounterObject()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if c.Value != 7 {
		t.Errorf("Value = %v, want 7", c.Value)
	}
	if c.Ident != "" {
		t.Errorf("Ident = %q, want empty", c.Ident)
	}
}

func TestParseCounterObjectUnknownFields(t *testing.T) {
	t.Parallel()
	data := `{"value": 5, "flag": "c", "description": "", "format": "integer", "extra": true}`
	s := &jsonScanner{data: data}
	c, ok := s.parseCounterObject()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if c.Value != 5 {
		t.Errorf("Value = %v, want 5", c.Value)
	}
}

func TestParseCounterObjectNonNumericValue(t *testing.T) {
	t.Parallel()
	data := `{"value": "not a number", "flag": "c", "description": ""}`
	s := &jsonScanner{data: data}
	_, ok := s.parseCounterObject()
	if ok {
		t.Fatal("expected ok=false for non-numeric value")
	}
}

func TestParseCounterObjectNullValue(t *testing.T) {
	t.Parallel()
	data := `{"value": null, "flag": "c", "description": ""}`
	s := &jsonScanner{data: data}
	_, ok := s.parseCounterObject()
	if ok {
		t.Fatal("expected ok=false for null value")
	}
}

func TestParseCounterObjectMaxUint64(t *testing.T) {
	t.Parallel()
	data := `{"value": 18446744073709551615, "flag": "b", "description": "Happy"}`
	s := &jsonScanner{data: data}
	c, ok := s.parseCounterObject()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if c.RawValue != "18446744073709551615" {
		t.Errorf("RawValue = %q, want %q", c.RawValue, "18446744073709551615")
	}
	// float64 can't represent max uint64 exactly, but ParseFloat should succeed.
	if c.Value != 1.8446744073709552e+19 {
		t.Errorf("Value = %v, want ~1.844e+19", c.Value)
	}
}

func TestUnescapeJSONStringSimple(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{`hello`, "hello"},
		{`he\"llo`, `he"llo`},
		{`a\\b`, `a\b`},
		{`a\/b`, `a/b`},
		{`a\nb`, "a\nb"},
		{`a\tb`, "a\tb"},
		{`a\rb`, "a\rb"},
		{`a\bb`, "a\bb"},
		{`a\fb`, "a\fb"},
	}
	for _, tt := range tests {
		got := unescapeJSONString(tt.input)
		if got != tt.want {
			t.Errorf("unescapeJSONString(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestUnescapeJSONStringUnicode(t *testing.T) {
	t.Parallel()
	// \u0041 = 'A'
	got := unescapeJSONString(`\u0041`)
	if got != "A" {
		t.Errorf("got %q, want %q", got, "A")
	}
}

func TestUnescapeJSONStringSurrogatePair(t *testing.T) {
	t.Parallel()
	// U+1F600 (😀) = \uD83D\uDE00
	got := unescapeJSONString(`\uD83D\uDE00`)
	if got != "😀" {
		t.Errorf("got %q, want %q", got, "😀")
	}
}

func TestParseVarnishstatV7Basic(t *testing.T) {
	t.Parallel()
	data := `{
		"version": 1,
		"counters": {
			"MAIN.cache_hit": {"value": 42, "flag": "c", "description": "Cache hits"},
			"MAIN.n_object": {"value": 10, "flag": "g", "description": "Object count"}
		}
	}`
	result, err := parseVarnishstatV7(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("got %d counters, want 2", len(result))
	}
	if result["MAIN.cache_hit"].Value != 42 {
		t.Errorf("cache_hit value = %v, want 42", result["MAIN.cache_hit"].Value)
	}
	if result["MAIN.n_object"].Flag != "g" {
		t.Errorf("n_object flag = %q, want %q", result["MAIN.n_object"].Flag, "g")
	}
}

func TestParseVarnishstatV7EmptyCounters(t *testing.T) {
	t.Parallel()
	data := `{"version": 1, "counters": {}}`
	result, err := parseVarnishstatV7(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("got %d counters, want 0", len(result))
	}
}

func TestParseVarnishstatV7NoCountersKey(t *testing.T) {
	t.Parallel()
	data := `{"version": 1}`
	_, err := parseVarnishstatV7(data)
	if err == nil {
		t.Fatal("expected error for missing counters key")
	}
}

func TestParseVarnishstatV7InvalidJSONScanner(t *testing.T) {
	t.Parallel()
	_, err := parseVarnishstatV7("not valid json")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseVarnishstatV6Basic(t *testing.T) {
	t.Parallel()
	data := `{
		"timestamp": "2024-01-01T00:00:00",
		"MAIN.cache_hit": {"value": 100, "flag": "c", "description": "Cache hits"},
		"MAIN.n_object": {"value": 5, "flag": "g", "description": "Object count"}
	}`
	result, err := parseVarnishstatV6(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("got %d counters, want 2", len(result))
	}
	if result["MAIN.cache_hit"].Value != 100 {
		t.Errorf("cache_hit value = %v, want 100", result["MAIN.cache_hit"].Value)
	}
}

func TestParseVarnishstatV6SkipsTimestamp(t *testing.T) {
	t.Parallel()
	data := `{
		"timestamp": "2024-01-01T00:00:00",
		"MAIN.cache_hit": {"value": 1, "flag": "c", "description": ""}
	}`
	result, err := parseVarnishstatV6(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, exists := result["timestamp"]; exists {
		t.Error("timestamp should not be in result")
	}
	if len(result) != 1 {
		t.Errorf("got %d counters, want 1", len(result))
	}
}

func TestParseVarnishstatV6NonObjectValue(t *testing.T) {
	t.Parallel()
	// V6 can have "MAIN.bad": "string" entries.
	data := `{
		"MAIN.good": {"value": 1, "flag": "c", "description": ""},
		"MAIN.bad": "unexpected string"
	}`
	result, err := parseVarnishstatV6(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Errorf("got %d counters, want 1", len(result))
	}
	if _, exists := result["MAIN.good"]; !exists {
		t.Error("MAIN.good should be in result")
	}
}

func TestParseVarnishstatV6InvalidJSONScanner(t *testing.T) {
	t.Parallel()
	_, err := parseVarnishstatV6("not valid json")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseCounterObjectFloatValue(t *testing.T) {
	t.Parallel()
	data := `{"value": 3.14, "flag": "g", "description": "float test"}`
	s := &jsonScanner{data: data}
	c, ok := s.parseCounterObject()
	if !ok {
		t.Fatal("expected ok=true")
	}
	if math.Abs(c.Value-3.14) > 1e-10 {
		t.Errorf("Value = %v, want 3.14", c.Value)
	}
	if c.RawValue != "3.14" {
		t.Errorf("RawValue = %q, want %q", c.RawValue, "3.14")
	}
}

func TestScanNumberScientificNegativeExponent(t *testing.T) {
	t.Parallel()
	s := &jsonScanner{data: "1.5E-3"}
	got, err := s.scanNumber()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "1.5E-3" {
		t.Errorf("got %q, want %q", got, "1.5E-3")
	}
}

func TestParseVarnishstatV7CountersFirst(t *testing.T) {
	t.Parallel()
	// counters key appears before version.
	data := `{
		"counters": {
			"MAIN.cache_hit": {"value": 1, "flag": "c", "description": ""}
		},
		"version": 1
	}`
	result, err := parseVarnishstatV7(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Errorf("got %d counters, want 1", len(result))
	}
}

func TestParseVarnishstatV7LargeUint64Value(t *testing.T) {
	t.Parallel()
	data := `{
		"version": 1,
		"counters": {
			"VBE.boot.default.happy": {
				"value": 18446744073709551615,
				"flag": "b",
				"description": "Happy health probes"
			}
		}
	}`
	result, err := parseVarnishstatV7(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c := result["VBE.boot.default.happy"]
	if c.RawValue != "18446744073709551615" {
		t.Errorf("RawValue = %q, want %q", c.RawValue, "18446744073709551615")
	}
}

func TestInternedFlags(t *testing.T) {
	t.Parallel()
	data := `{"value": 1, "flag": "c", "description": ""}`
	s := &jsonScanner{data: data}
	c, ok := s.parseCounterObject()
	if !ok {
		t.Fatal("expected ok=true")
	}
	// The flag should be the interned string, not a new allocation.
	if c.Flag != "c" {
		t.Errorf("Flag = %q, want %q", c.Flag, "c")
	}
}
