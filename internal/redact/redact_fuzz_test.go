package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzRedactNestedSecrets feeds arbitrary JSON (decoded into the nested
// map[string]any shape that ConfigMap/Secret values take) through Update, which
// walks it via collectLeafStrings. The redactor is rebuilt from operator-
// controlled data on every change, so building it must never panic.
func FuzzRedactNestedSecrets(f *testing.F) {
	seeds := []string{
		`{"a":"hunter2pass"}`,
		`{"a":{"b":{"c":"deepsecretvalue"}}}`,
		`{"a":["onesecret","twosecret",["nested"]]}`,
		`{"a":null,"b":123,"c":true}`,
		`[1,2,3]`,
		`"justastring"`,
		`{}`,
		``,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(_ *testing.T, data []byte) {
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			return // only well-formed object payloads reach Update in production
		}

		r := NewRedactor()
		r.Update(map[string]map[string]any{"s": m})
		_ = r.Redact("nothing-to-redact-here")
	})
}

// FuzzRedact checks the property the redactor exists for: once a secret is
// registered, its verbatim value must not appear in Redact's output.
//
// To keep the assertion sound, the secret is required to share no byte with the
// placeholder text ([REDACTED]). That guarantees the inserted placeholder can
// neither contain a fragment of the secret nor bridge two original fragments
// into one - so any occurrence of the secret in the output would be a genuine
// leak, never an artifact of the replacement. (Without this guard a secret like
// "000001" "reappears" around "000[REDACTED]001", which is not a real leak.)
func FuzzRedact(f *testing.F) {
	f.Add("hunter2pass", "user=admin password=hunter2pass token=hunter2pass")
	f.Add("supersecret", "no occurrence here")
	f.Add("000001", "000000001001") // placeholder-collision case: must NOT flag
	f.Add("99999999", "x99999999x99999999")

	f.Fuzz(func(t *testing.T, secret, haystack string) {
		if len(secret) < minSecretLen || strings.ContainsAny(secret, placeholder) {
			return
		}

		r := NewRedactor()
		r.Update(map[string]map[string]any{"s": {"k": secret}})
		got := r.Redact(haystack)

		if strings.Contains(got, secret) {
			t.Fatalf("secret survived redaction:\n secret  = %q\n haystack= %q\n output  = %q",
				secret, haystack, got)
		}
	})
}
