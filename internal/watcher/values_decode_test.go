package watcher

import (
	"strings"
	"testing"
	"text/template"
)

// renderValue renders v the way a VCL template does, so the assertions below
// pin what actually lands in the generated VCL rather than the Go type.
func renderValue(t *testing.T, v any) string {
	t.Helper()
	tmpl := template.Must(template.New("t").Parse("{{ . }}"))
	var sb strings.Builder
	err := tmpl.Execute(&sb, v)
	if err != nil {
		t.Fatalf("render %v: %v", v, err)
	}

	return sb.String()
}

// TestDecodeValueKeepsIntegersExact pins that numeric values from a ConfigMap,
// Secret or --values-dir file reach templates as integers, not float64.
//
// sigs.k8s.io/yaml decodes an untyped scalar into float64, and text/template
// prints a float64 with %g-shortest - which switches to scientific notation at
// exactly 1e6. A ConfigMap saying `max_object_size: 1048576` therefore rendered
// `1.048576e+06` into the VCL, which varnishd rejects at vcl.load. Because the
// template and the values are both stable, the reload-recovery backoff then
// retried the identical broken VCL forever and never converged; hit at startup,
// varnishd never launched at all. Integers above 2^53 additionally lost
// precision before anything could observe them.
func TestDecodeValueKeepsIntegersExact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "below the scientific-notation threshold", in: "999999", want: "999999"},
		{name: "at the threshold", in: "1000000", want: "1000000"},
		{name: "one MiB", in: "1048576", want: "1048576"},
		{name: "large byte count", in: "2147483648", want: "2147483648"},
		{name: "beyond float64 integer precision", in: "1234567890123456789", want: "1234567890123456789"},
		{name: "negative", in: "-1048576", want: "-1048576"},
		{name: "small integer still an integer", in: "42", want: "42"},
		{name: "genuine float is preserved", in: "1.5", want: "1.5"},
		{name: "string stays a string", in: "hello", want: "hello"},
		{name: "bool stays a bool", in: "true", want: "true"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := renderValue(t, decodeValue([]byte(tc.in)))
			if got != tc.want {
				t.Errorf("decodeValue(%q) renders as %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDecodeValueNestedIntegers pins that the same conversion reaches integers
// nested inside maps and lists, which is how structured --values data arrives.
func TestDecodeValueNestedIntegers(t *testing.T) {
	t.Parallel()

	v := decodeValue([]byte("tuning:\n  ttl: 1048576\n  sizes: [2097152, 3]\n"))
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("decodeValue returned %T, want map[string]any", v)
	}
	tuning, ok := m["tuning"].(map[string]any)
	if !ok {
		t.Fatalf("tuning is %T, want map[string]any", m["tuning"])
	}
	if got := renderValue(t, tuning["ttl"]); got != "1048576" {
		t.Errorf("nested map value renders as %q, want %q", got, "1048576")
	}
	sizes, ok := tuning["sizes"].([]any)
	if !ok {
		t.Fatalf("sizes is %T, want []any", tuning["sizes"])
	}
	if got := renderValue(t, sizes[0]); got != "2097152" {
		t.Errorf("nested list value renders as %q, want %q", got, "2097152")
	}
}

// TestDecodeValueLosslessScalars pins that a scalar whose YAML 1.1 decoding
// would silently change it reaches templates as its original text.
//
// sigs.k8s.io/yaml applies YAML 1.1 resolution, so a zero-padded number is read
// as octal (`0755` -> 493, i.e. a file mode renders as 493) and a trailing zero
// is dropped from a decimal (`1.10` -> 1.1, mangling a version pin). An empty
// ConfigMap value became nil and rendered as `<no value>`. None of these is a
// parse error, so the existing raw-string fallback never triggered and the
// wrong text went into the VCL with no warning.
func TestDecodeValueLosslessScalars(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "zero-padded file mode stays literal", in: "0755", want: "0755"},
		{name: "zero-padded id stays literal", in: "0644", want: "0644"},
		{name: "trailing zero is preserved", in: "1.10", want: "1.10"},
		{name: "empty value stays an empty string", in: "", want: ""},
		{name: "plain integer still a number", in: "42", want: "42"},
		{name: "large integer still exact", in: "1048576", want: "1048576"},
		{name: "genuine float unchanged", in: "1.5", want: "1.5"},
		{name: "negative integer unchanged", in: "-7", want: "-7"},
		{name: "zero unchanged", in: "0", want: "0"},
		{name: "boolean still a boolean", in: "true", want: "true"},
		{name: "quoted string unchanged", in: `"hello"`, want: "hello"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := renderValue(t, decodeValue([]byte(tc.in)))
			if got != tc.want {
				t.Errorf("decodeValue(%q) renders as %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
