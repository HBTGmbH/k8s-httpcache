package telemetry

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// jsonScanner is a minimal, zero-allocation JSON scanner that operates on
// string offsets into the original data. Substring operations like
// data[start:end] return zero-allocation string views into the same backing
// memory, eliminating per-field allocations for stored counter values.
type jsonScanner struct {
	data string
	pos  int
}

// internedFlags avoids per-counter allocation for the common single-char flag
// strings emitted by varnishstat ("c", "g", "a", "b").
var internedFlags = map[string]string{
	"c": "c",
	"g": "g",
	"a": "a",
	"b": "b",
}

// Sentinel errors for the JSON scanner.
var (
	errUnterminatedObject     = errors.New("unterminated object")
	errUnterminatedArray      = errors.New("unterminated array")
	errUnexpectedEndSkip      = errors.New("unexpected end of input while skipping value")
	errUnterminatedCounterMap = errors.New("unterminated counter map")
	errV7NoCountersKey        = errors.New("decoding varnishstat v7 output: no counters key found")
	errV7UnterminatedObject   = errors.New("decoding varnishstat v7 output: unterminated object")
)

func (s *jsonScanner) skipWhitespace() {
	for s.pos < len(s.data) {
		switch s.data[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return
		}
	}
}

func (s *jsonScanner) expectByte(b byte) error {
	s.skipWhitespace()
	if s.pos >= len(s.data) {
		return fmt.Errorf("unexpected end of input, expected %q", b) //nolint:err113 // position-specific parse error
	}
	if s.data[s.pos] != b {
		return fmt.Errorf("expected %q at position %d, got %q", b, s.pos, s.data[s.pos]) //nolint:err113 // position-specific parse error
	}
	s.pos++

	return nil
}

// scanString scans a JSON string value (the opening '"' must be the current byte).
// Returns the content between quotes as a string. When no escape sequences are
// present, the returned string is a zero-allocation substring of the original data.
// When escapes are detected, unescapeJSONString is called to produce a new string.
func (s *jsonScanner) scanString() (string, error) {
	if s.pos >= len(s.data) || s.data[s.pos] != '"' {
		return "", fmt.Errorf("expected '\"' at position %d", s.pos) //nolint:err113 // position-specific parse error
	}
	s.pos++ // skip opening quote
	start := s.pos
	hasEscape := false

	for s.pos < len(s.data) {
		ch := s.data[s.pos]
		if ch == '\\' {
			hasEscape = true
			s.pos++
			if s.pos >= len(s.data) {
				return "", fmt.Errorf("unterminated string escape at position %d", s.pos) //nolint:err113 // position-specific parse error
			}
			// Skip the escaped character; \uXXXX needs 4 more hex digits.
			if s.data[s.pos] == 'u' {
				if s.pos+4 >= len(s.data) {
					return "", fmt.Errorf("unterminated \\u escape at position %d", s.pos) //nolint:err113 // position-specific parse error
				}
				s.pos += 4 // skip the 4 hex digits (the loop increment handles the 'u')
			}
			s.pos++

			continue
		}
		if ch == '"' {
			raw := s.data[start:s.pos]
			s.pos++ // skip closing quote
			if hasEscape {
				return unescapeJSONString(raw), nil
			}

			return raw, nil
		}
		s.pos++
	}

	return "", fmt.Errorf("unterminated string starting at position %d", start-1) //nolint:err113 // position-specific parse error
}

// scanNumber scans a JSON number literal and returns the raw string.
// The returned string is a zero-allocation substring of the original data.
func (s *jsonScanner) scanNumber() (string, error) {
	start := s.pos
	if s.pos < len(s.data) && s.data[s.pos] == '-' {
		s.pos++
	}
	if s.pos >= len(s.data) {
		return "", fmt.Errorf("unexpected end of number at position %d", start) //nolint:err113 // position-specific parse error
	}
	// Integer part.
	for s.pos < len(s.data) && s.data[s.pos] >= '0' && s.data[s.pos] <= '9' {
		s.pos++
	}
	// Fractional part.
	if s.pos < len(s.data) && s.data[s.pos] == '.' {
		s.pos++
		for s.pos < len(s.data) && s.data[s.pos] >= '0' && s.data[s.pos] <= '9' {
			s.pos++
		}
	}
	// Exponent part.
	if s.pos < len(s.data) && (s.data[s.pos] == 'e' || s.data[s.pos] == 'E') {
		s.pos++
		if s.pos < len(s.data) && (s.data[s.pos] == '+' || s.data[s.pos] == '-') {
			s.pos++
		}
		for s.pos < len(s.data) && s.data[s.pos] >= '0' && s.data[s.pos] <= '9' {
			s.pos++
		}
	}
	if s.pos == start {
		return "", fmt.Errorf("invalid number at position %d", start) //nolint:err113 // position-specific parse error
	}

	return s.data[start:s.pos], nil
}

// skipValue skips over the next JSON value (string, number, object, array, bool, null).
func (s *jsonScanner) skipValue() error {
	s.skipWhitespace()
	if s.pos >= len(s.data) {
		return errUnexpectedEndSkip
	}

	switch s.data[s.pos] {
	case '"':
		_, err := s.scanString()

		return err
	case '{':
		return s.skipObject()
	case '[':
		return s.skipArray()
	case 't': // true
		return s.skipLiteral("true")
	case 'f': // false
		return s.skipLiteral("false")
	case 'n': // null
		return s.skipLiteral("null")
	default:
		// Must be a number.
		_, err := s.scanNumber()

		return err
	}
}

func (s *jsonScanner) skipObject() error {
	s.pos++ // skip '{'
	s.skipWhitespace()
	if s.pos < len(s.data) && s.data[s.pos] == '}' {
		s.pos++

		return nil
	}
	for {
		s.skipWhitespace()
		// key
		_, err := s.scanString()
		if err != nil {
			return err
		}

		err = s.expectByte(':')
		if err != nil {
			return err
		}

		// value
		err = s.skipValue()
		if err != nil {
			return err
		}

		s.skipWhitespace()
		if s.pos >= len(s.data) {
			return errUnterminatedObject
		}
		if s.data[s.pos] == '}' {
			s.pos++

			return nil
		}
		if s.data[s.pos] == ',' {
			s.pos++

			continue
		}

		return fmt.Errorf("expected ',' or '}' at position %d", s.pos) //nolint:err113 // position-specific parse error
	}
}

func (s *jsonScanner) skipArray() error {
	s.pos++ // skip '['
	s.skipWhitespace()
	if s.pos < len(s.data) && s.data[s.pos] == ']' {
		s.pos++

		return nil
	}
	for {
		err := s.skipValue()
		if err != nil {
			return err
		}

		s.skipWhitespace()
		if s.pos >= len(s.data) {
			return errUnterminatedArray
		}
		if s.data[s.pos] == ']' {
			s.pos++

			return nil
		}
		if s.data[s.pos] == ',' {
			s.pos++

			continue
		}

		return fmt.Errorf("expected ',' or ']' at position %d", s.pos) //nolint:err113 // position-specific parse error
	}
}

func (s *jsonScanner) skipLiteral(lit string) error {
	if s.pos+len(lit) > len(s.data) {
		return fmt.Errorf("unexpected end of input, expected %q", lit) //nolint:err113 // position-specific parse error
	}
	if s.data[s.pos:s.pos+len(lit)] != lit {
		return fmt.Errorf("expected %q at position %d", lit, s.pos) //nolint:err113 // position-specific parse error
	}
	s.pos += len(lit)

	return nil
}

// unescapeJSONString handles all JSON escape sequences including \uXXXX
// surrogate pairs. Only called when scanString detects a backslash.
// Uses [strings.Builder] whose String() method is zero-copy.
func unescapeJSONString(raw string) string {
	var buf strings.Builder
	buf.Grow(len(raw))
	for i := 0; i < len(raw); {
		if raw[i] != '\\' {
			_ = buf.WriteByte(raw[i])
			i++

			continue
		}
		i++ // skip backslash
		if i >= len(raw) {
			break
		}

		//nolint:revive // identical default branch is intentional: all unrecognized escapes pass through
		switch raw[i] {
		case '"', '\\', '/':
			_ = buf.WriteByte(raw[i])
		case 'b':
			_ = buf.WriteByte('\b')
		case 'f':
			_ = buf.WriteByte('\f')
		case 'n':
			_ = buf.WriteByte('\n')
		case 'r':
			_ = buf.WriteByte('\r')
		case 't':
			_ = buf.WriteByte('\t')
		case 'u':
			if i+4 >= len(raw) {
				_ = buf.WriteByte(raw[i])
				i++

				continue
			}
			r1 := parseHex4(raw[i+1 : i+5])
			i += 4
			// Handle surrogate pairs.
			if r1 >= 0xD800 && r1 <= 0xDBFF && i+2 < len(raw) && raw[i+1] == '\\' && raw[i+2] == 'u' && i+6 < len(raw) {
				r2 := parseHex4(raw[i+3 : i+7])
				if r2 >= 0xDC00 && r2 <= 0xDFFF {
					combined := 0x10000 + (r1-0xD800)*0x400 + (r2 - 0xDC00)
					_, _ = buf.WriteRune(rune(combined))
					i += 6 // skip \uXXXX of the low surrogate
					i++

					continue
				}
			}
			_, _ = buf.WriteRune(rune(r1)) //nolint:gosec // r1 is bounded 0..0xFFFF from parseHex4
		default:
			_ = buf.WriteByte(raw[i])
		}
		i++
	}

	return buf.String()
}

func parseHex4(b string) int {
	var n int
	for i := range len(b) {
		c := b[i]
		n <<= 4

		switch {
		case c >= '0' && c <= '9':
			n |= int(c - '0')
		case c >= 'a' && c <= 'f':
			n |= int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n |= int(c-'A') + 10
		}
	}

	return n
}

// parseCounterObject parses a single counter object: {"value":N, "flag":"c", ...}.
// Returns the counter and true on success, or zero value and false if the
// counter should be skipped (e.g. non-numeric value).
func (s *jsonScanner) parseCounterObject() (varnishstatCounter, bool) {
	s.pos++ // skip '{'

	var c varnishstatCounter
	var numStr string
	hasValue := false

	for {
		s.skipWhitespace()
		if s.pos >= len(s.data) {
			return c, false
		}
		if s.data[s.pos] == '}' {
			s.pos++
			if !hasValue {
				return c, false
			}
			fVal, err := strconv.ParseFloat(numStr, 64)
			if err != nil {
				return c, false
			}
			c.Value = fVal
			c.RawValue = numStr

			return c, true
		}
		if s.data[s.pos] == ',' {
			s.pos++
			s.skipWhitespace()
		}

		// Read field name.
		keyStr, err := s.scanString()
		if err != nil {
			return c, false
		}

		err = s.expectByte(':')
		if err != nil {
			return c, false
		}

		s.skipWhitespace()
		if s.pos >= len(s.data) {
			return c, false
		}

		// Dispatch on field name.
		switch keyStr {
		case "value":
			numStr, hasValue, err = s.parseCounterValue()
			if err != nil {
				return c, false
			}
			if !hasValue {
				return s.skipRemainingFields(c)
			}

			continue
		case "flag":
			flagStr, scanErr := s.scanString()
			if scanErr != nil {
				return c, false
			}
			if interned, ok := internedFlags[flagStr]; ok {
				c.Flag = interned
			} else {
				c.Flag = flagStr
			}

			continue
		case "description":
			c.Description, err = s.scanString()
			if err != nil {
				return c, false
			}

			continue
		case "ident":
			c.Ident, err = s.scanString()
			if err != nil {
				return c, false
			}

			continue
		default:
			// Unknown field — skip its value.
			err = s.skipValue()
			if err != nil {
				return c, false
			}
		}
	}
}

// parseCounterValue parses the "value" field of a counter object.
// Returns the raw number string, whether a valid number was found, and any error.
// When the value is not a number (string, null, etc.), it is skipped and
// hasValue is returned as false.
func (s *jsonScanner) parseCounterValue() (string, bool, error) {
	ch := s.data[s.pos]
	if ch == '"' || ch == 'n' || ch == '[' || ch == '{' || ch == 't' || ch == 'f' {
		err := s.skipValue()

		return "", false, err
	}

	numStr, err := s.scanNumber()
	if err != nil {
		return "", false, err
	}

	return numStr, true, nil
}

// skipRemainingFields consumes the rest of the current object (after a
// non-numeric value was encountered) and returns ok=false so the counter
// is skipped. This handles V6-style entries like "MAIN.bad": "string".
func (s *jsonScanner) skipRemainingFields(c varnishstatCounter) (varnishstatCounter, bool) {
	for {
		s.skipWhitespace()
		if s.pos >= len(s.data) {
			return c, false
		}
		if s.data[s.pos] == '}' {
			s.pos++

			return c, false
		}
		if s.data[s.pos] == ',' {
			s.pos++
			s.skipWhitespace()
		}
		// Skip key.
		_, err := s.scanString()
		if err != nil {
			return c, false
		}

		err = s.expectByte(':')
		if err != nil {
			return c, false
		}

		err = s.skipValue()
		if err != nil {
			return c, false
		}
	}
}

// parseCounterMap iterates "key": {counter} pairs inside a JSON object.
// skipKey is an optional predicate that returns true for keys that should be
// skipped (e.g. V6 metadata keys without a dot). Pass nil to accept all keys.
func (s *jsonScanner) parseCounterMap(skipKey func(string) bool) (map[string]varnishstatCounter, error) {
	err := s.expectByte('{')
	if err != nil {
		return nil, err
	}

	// Pre-size the map based on data length heuristic (~100 bytes per counter).
	result := make(map[string]varnishstatCounter, len(s.data)/100)

	s.skipWhitespace()
	if s.pos < len(s.data) && s.data[s.pos] == '}' {
		s.pos++

		return result, nil
	}

	for {
		s.skipWhitespace()

		keyStr, err := s.scanString()
		if err != nil {
			return nil, fmt.Errorf("reading counter key: %w", err)
		}

		err = s.expectByte(':')
		if err != nil {
			return nil, err
		}

		s.skipWhitespace()

		switch {
		case skipKey != nil && skipKey(keyStr):
			err = s.skipValue()
			if err != nil {
				return nil, err
			}
		case s.pos < len(s.data) && s.data[s.pos] == '{':
			counter, ok := s.parseCounterObject()
			if ok {
				result[keyStr] = counter
			}
		default:
			// Non-object value (e.g. V6 "MAIN.bad": "string").
			err = s.skipValue()
			if err != nil {
				return nil, err
			}
		}

		s.skipWhitespace()
		if s.pos >= len(s.data) {
			return nil, errUnterminatedCounterMap
		}
		if s.data[s.pos] == '}' {
			s.pos++

			return result, nil
		}
		if s.data[s.pos] == ',' {
			s.pos++

			continue
		}

		return nil, fmt.Errorf("expected ',' or '}' at position %d", s.pos) //nolint:err113 // position-specific parse error
	}
}

// parseVarnishstatV7 parses the Varnish 7+ JSON format where counters are
// nested under a "counters" key:
//
//	{"version": 1, "counters": {"MAIN.cache_hit": {...}, ...}}
func parseVarnishstatV7(data string) (map[string]varnishstatCounter, error) {
	s := &jsonScanner{data: data}

	err := s.expectByte('{')
	if err != nil {
		return nil, fmt.Errorf("decoding varnishstat v7 output: %w", err)
	}

	s.skipWhitespace()
	if s.pos < len(s.data) && s.data[s.pos] == '}' {
		return nil, errV7NoCountersKey
	}

	for {
		s.skipWhitespace()

		keyStr, err := s.scanString()
		if err != nil {
			return nil, fmt.Errorf("decoding varnishstat v7 output: %w", err)
		}

		err = s.expectByte(':')
		if err != nil {
			return nil, fmt.Errorf("decoding varnishstat v7 output: %w", err)
		}

		if keyStr == "counters" {
			result, parseErr := s.parseCounterMap(nil)
			if parseErr != nil {
				return nil, fmt.Errorf("decoding varnishstat v7 output: %w", parseErr)
			}

			return result, nil
		}

		// Skip non-counters keys (e.g. "version", "timestamp").
		err = s.skipValue()
		if err != nil {
			return nil, fmt.Errorf("decoding varnishstat v7 output: %w", err)
		}

		s.skipWhitespace()
		if s.pos >= len(s.data) {
			return nil, errV7UnterminatedObject
		}
		if s.data[s.pos] == '}' {
			return nil, errV7NoCountersKey
		}
		if s.data[s.pos] == ',' {
			s.pos++

			continue
		}

		return nil, fmt.Errorf("decoding varnishstat v7 output: expected ',' or '}' at position %d", s.pos) //nolint:err113 // position-specific parse error
	}
}

// parseVarnishstatV6 parses the Varnish 6.x flat JSON format where counters
// sit at the top level alongside non-counter keys like "timestamp":
//
//	{"timestamp": "...", "MAIN.cache_hit": {...}, ...}
func parseVarnishstatV6(data string) (map[string]varnishstatCounter, error) {
	s := &jsonScanner{data: data}

	// V6 keys without a dot (like "timestamp") are metadata, not counters.
	skipNonCounter := func(key string) bool {
		return !strings.Contains(key, ".")
	}

	result, err := s.parseCounterMap(skipNonCounter)
	if err != nil {
		return nil, fmt.Errorf("decoding varnishstat v6 output: %w", err)
	}

	return result, nil
}
