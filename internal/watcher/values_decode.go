package watcher

import (
	"encoding/json"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// useNumber makes the decoder hand back [json.Number] instead of float64 for
// every numeric scalar, so integers survive decoding with full precision and
// can be re-typed exactly (see normalizeNumbers).
func useNumber(d *json.Decoder) *json.Decoder {
	d.UseNumber()

	return d
}

// decodeValue parses one ConfigMap / Secret / --values-dir entry into the
// arbitrary structure templates consume. On a parse error the raw text is
// returned unchanged, so an unparseable value still reaches the template as a
// plain string.
//
// Integers are decoded as int64 rather than float64. sigs.k8s.io/yaml decodes
// an untyped scalar into float64, and [text/template] prints a float64 with
// %g-shortest - which switches to scientific notation at exactly 1e6. A
// ConfigMap saying `max_object_size: 1048576` therefore rendered
// `1.048576e+06` into the VCL, which varnishd rejects at vcl.load; since the
// template and the values are both stable, the reload-recovery backoff then
// retried the identical broken VCL forever. Integers above 2^53 also lost
// precision. Keeping them as int64 renders them verbatim and, as a bonus,
// makes `eq .Values.x 42` work (comparing a float64 against an untyped int
// constant is an error in text/template).
func decodeValue(raw []byte) any {
	var val any
	err := yaml.Unmarshal(raw, &val, useNumber)
	if err != nil {
		return string(raw)
	}
	val = normalizeNumbers(val)

	// A scalar that YAML 1.1 resolution rewrote is kept as its original text.
	// Nothing here is a parse error, so the fallback above never fires: `0755`
	// resolves as OCTAL to 493 (a file mode renders as 493), `1.10` loses its
	// trailing zero (mangling a version pin), and an empty value becomes nil and
	// renders as "<no value>". Round-tripping the decoded value and comparing it
	// with the input detects exactly those cases while leaving genuine numbers,
	// booleans and strings untouched.
	if text, ok := lossyScalar(raw, val); ok {
		return text
	}

	return val
}

// lossyScalar reports whether raw is a scalar whose decoded form no longer
// renders back to the original text, returning that text.
func lossyScalar(raw []byte, val any) (string, bool) {
	text := strings.TrimRight(string(raw), "\n")
	if strings.ContainsAny(text, "\n:-[]{}") && !isSignedNumber(text) {
		// Structured YAML (or anything multi-line): not a plain scalar.
		return "", false
	}

	switch v := val.(type) {
	case nil:
		// An empty value is an empty string, not a missing one.
		return text, text == ""
	case int64:
		return text, strconv.FormatInt(v, 10) != text
	case float64:
		return text, strconv.FormatFloat(v, 'g', -1, 64) != text
	default:
		return "", false
	}
}

// isSignedNumber reports whether text is a plain (possibly signed) decimal, so
// a leading '-' is not mistaken for YAML structure.
func isSignedNumber(text string) bool {
	if text == "" {
		return false
	}
	body := strings.TrimPrefix(text, "-")
	if body == "" {
		return false
	}
	for _, r := range body {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}

	return true
}

// normalizeNumbers walks the decoded structure and replaces every
// [json.Number] with int64 when it is an exact integer and float64 otherwise,
// so templates see real numeric types (arithmetic and comparison helpers keep
// working) instead of a string-backed [json.Number].
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		i, intErr := t.Int64()
		if intErr == nil {
			return i
		}
		f, floatErr := t.Float64()
		if floatErr == nil {
			return f
		}

		// Neither representable: keep the literal text rather than losing it.
		return t.String()
	case map[string]any:
		for k, e := range t {
			t[k] = normalizeNumbers(e)
		}

		return t
	case []any:
		for i, e := range t {
			t[i] = normalizeNumbers(e)
		}

		return t
	default:
		return v
	}
}
