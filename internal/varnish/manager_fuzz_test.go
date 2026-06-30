package varnish

import (
	"bytes"
	"testing"
)

// FuzzPrefixWriter exercises the hand-rolled line buffering in prefixWriter.Write
// with arbitrary byte streams delivered in arbitrary-sized chunks (as the
// varnishncsa subprocess would). It must never panic, must always report the
// full input byte count, and - for inputs below the overlong-line bound - every
// emitted line carries exactly one prefix, so stripping one prefix per line and
// re-appending the still-buffered partial reconstructs the original bytes.
func FuzzPrefixWriter(f *testing.F) {
	f.Add([]byte("a\nb\n"), uint8(0))
	f.Add([]byte("no newline at all"), uint8(3))
	f.Add([]byte("\n\n"), uint8(0))
	f.Add([]byte(""), uint8(0))
	f.Add([]byte("line1\nline2\nline3"), uint8(255))
	f.Add([]byte("P: already prefixed\n"), uint8(2))

	f.Fuzz(func(t *testing.T, data []byte, chunk uint8) {
		const prefix = "P: "
		var out bytes.Buffer
		w := newPrefixWriter(&out, prefix)

		size := int(chunk) + 1
		for i := 0; i < len(data); i += size {
			slice := data[i:min(i+size, len(data))]
			n, err := w.Write(slice)
			if err != nil {
				t.Fatalf("Write(%q) error: %v", slice, err)
			}
			if n != len(slice) {
				t.Fatalf("Write(%q) returned %d, want %d", slice, n, len(slice))
			}
		}

		// The overlong-line guard re-prefixes mid-line, so the clean round-trip
		// only holds below that bound. Fuzz inputs are tiny, so this is the
		// normal case.
		if len(data) >= maxBufferedLine {
			return
		}

		recovered := stripPrefixPerLine(out.Bytes(), []byte(prefix))
		recovered = append(recovered, w.buf...)
		if !bytes.Equal(recovered, data) {
			t.Fatalf("round-trip mismatch:\n in =%q\n out=%q\n rec=%q\n buf=%q",
				data, out.Bytes(), recovered, w.buf)
		}
	})
}

// stripPrefixPerLine removes exactly one leading prefix from each
// newline-terminated segment of b and concatenates the remainders. Every line
// prefixWriter emits begins with the prefix, so this inverts the prefixing.
func stripPrefixPerLine(b, prefix []byte) []byte {
	var rec []byte
	for len(b) > 0 {
		line := b
		if nl := bytes.IndexByte(b, '\n'); nl >= 0 {
			line, b = b[:nl+1], b[nl+1:]
		} else {
			b = nil
		}
		rec = append(rec, bytes.TrimPrefix(line, prefix)...)
	}

	return rec
}
