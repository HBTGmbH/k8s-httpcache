// Package renderer renders VCL templates with endpoint data.
package renderer

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"

	"k8s-httpcache/internal/watcher"
)

type templateData struct {
	Frontends []watcher.Frontend
	Backends  map[string][]watcher.Endpoint
	Values    map[string]map[string]any
}

// Renderer renders VCL from a Go template and a list of frontends.
type Renderer struct {
	tmpl         *template.Template
	prev         *template.Template
	templatePath string
	funcMap      template.FuncMap
	drainBackend string
}

// SetDrainBackend configures the renderer to auto-inject drain VCL using the
// given backend name. An empty name disables injection.
func (r *Renderer) SetDrainBackend(name string) {
	r.drainBackend = name
}

// New parses the template file and returns a Renderer.
func New(templatePath string) (*Renderer, error) {
	funcMap := sprig.TxtFuncMap()

	raw, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("reading template %s: %w", templatePath, err)
	}

	tmpl, err := template.New("vcl").Delims("<<", ">>").Funcs(funcMap).Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parsing template %s: %w", templatePath, err)
	}

	return &Renderer{tmpl: tmpl, templatePath: templatePath, funcMap: funcMap}, nil
}

// Reload re-reads and re-parses the template file from disk.
// On success the internal template is replaced and the previous template is
// kept so that Rollback can restore it. On parse failure the active template
// is not changed.
func (r *Renderer) Reload() error {
	raw, err := os.ReadFile(r.templatePath)
	if err != nil {
		return fmt.Errorf("reading template %s: %w", r.templatePath, err)
	}

	tmpl, err := template.New("vcl").Delims("<<", ">>").Funcs(r.funcMap).Parse(string(raw))
	if err != nil {
		return fmt.Errorf("parsing template %s: %w", r.templatePath, err)
	}

	r.prev = r.tmpl
	r.tmpl = tmpl
	return nil
}

// Rollback reverts to the template that was active before the last successful
// Reload. It is a no-op if there is no previous template.
func (r *Renderer) Rollback() {
	if r.prev != nil {
		r.tmpl = r.prev
		r.prev = nil
	}
}

// Render executes the template with the given frontends, backends, and values and returns the VCL string.
func (r *Renderer) Render(frontends []watcher.Frontend, backends map[string][]watcher.Endpoint, values map[string]map[string]any) (string, error) {
	if values == nil {
		values = make(map[string]map[string]any)
	}
	var buf bytes.Buffer
	if err := r.tmpl.Execute(&buf, templateData{Frontends: frontends, Backends: backends, Values: values}); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	vcl := buf.String()
	if r.drainBackend != "" {
		vcl = injectDrainVCL(vcl, r.drainBackend)
	}
	return vcl, nil
}

// RenderToFile renders the template to a temporary file and returns its path.
func (r *Renderer) RenderToFile(frontends []watcher.Frontend, backends map[string][]watcher.Endpoint, values map[string]map[string]any) (string, error) {
	vcl, err := r.Render(frontends, backends, values)
	if err != nil {
		return "", err
	}

	f, err := os.CreateTemp("", "k8s-httpcache-*.vcl")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}

	if _, err := f.WriteString(vcl); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("writing temp file: %w", err)
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("closing temp file: %w", err)
	}

	return f.Name(), nil
}

// vclVersionRe matches a VCL version line like "vcl 4.1;" (with optional
// leading whitespace and trailing newline).
var vclVersionRe = regexp.MustCompile(`(?m)^[\t ]*vcl\s+[\d.]+\s*;\s*\n?`)

// vclVersionEnd returns the byte offset just past the "vcl X.Y;" line.
// If no version line is found it returns 0.
func vclVersionEnd(vcl string) int {
	loc := vclVersionRe.FindStringIndex(vcl)
	if loc == nil {
		return 0
	}
	return loc[1]
}

// importStdRe matches an "import std;" line (with optional leading whitespace
// and trailing newline) so it can be stripped from user VCL before re-injection.
var importStdRe = regexp.MustCompile(`(?m)^[\t ]*import\s+std\s*;\s*\n?`)

// stripImportStd removes top-level "import std;" lines from VCL while
// preserving any occurrences inside /* */ block comments.
func stripImportStd(vcl string) string {
	locs := importStdRe.FindAllStringIndex(vcl, -1)
	if len(locs) == 0 {
		return vcl
	}

	var b strings.Builder
	prev := 0
	for _, loc := range locs {
		before := vcl[:loc[0]]
		if strings.Count(before, "/*")-strings.Count(before, "*/") > 0 {
			continue // inside a block comment — keep it
		}
		b.WriteString(vcl[prev:loc[0]])
		prev = loc[1]
	}
	b.WriteString(vcl[prev:])
	return b.String()
}

// backendBlockRe matches the start of a VCL backend declaration, e.g.
// "backend myname {". VCL identifiers may contain letters, digits,
// underscores, and hyphens.
var backendBlockRe = regexp.MustCompile(`(?m)^[\t ]*backend\s+[\w-]+\s*\{`)

// backendBlocksEnd returns the byte offset just past the closing "}" (and any
// trailing newline) of the last backend block in vcl. It correctly handles
// nested brace blocks such as ".probe = { ... }". Returns 0 if no backend
// blocks are found.
func backendBlocksEnd(vcl string) int {
	locs := backendBlockRe.FindAllStringIndex(vcl, -1)
	if len(locs) == 0 {
		return 0
	}

	lastEnd := 0
	for _, loc := range locs {
		// Skip matches inside /* */ block comments.
		before := vcl[:loc[0]]
		if strings.Count(before, "/*")-strings.Count(before, "*/") > 0 {
			continue
		}

		// The regex ends with \{, so loc[1]-1 is the opening brace.
		openBrace := loc[1] - 1
		depth := 1
		end := openBrace + 1
		for i := openBrace + 1; i < len(vcl); i++ {
			// Skip /* */ block comments.
			if i+1 < len(vcl) && vcl[i] == '/' && vcl[i+1] == '*' {
				end := strings.Index(vcl[i+2:], "*/")
				if end >= 0 {
					i = i + 2 + end + 1 // skip past "*/"
				} else {
					break // unclosed comment, stop scanning
				}
				continue
			}
			// Skip // and # line comments.
			if vcl[i] == '#' || (i+1 < len(vcl) && vcl[i] == '/' && vcl[i+1] == '/') {
				nl := strings.IndexByte(vcl[i:], '\n')
				if nl >= 0 {
					i += nl // advance to newline
				} else {
					break // no newline, rest is comment
				}
				continue
			}
			switch vcl[i] {
			case '{':
				depth++
			case '}':
				depth--
			}
			if depth == 0 {
				end = i + 1
				// Skip a single trailing newline.
				if end < len(vcl) && vcl[end] == '\n' {
					end++
				}
				break
			}
		}
		if end > lastEnd {
			lastEnd = end
		}
	}
	return lastEnd
}

// injectDrainVCL injects drain VCL into the user template output.
//
// "import std;" is injected right after the version line (and stripped from the
// user VCL to avoid duplicates) because it must appear before any subroutine
// that uses it.
//
// The drain backend declaration and drain sub vcl_deliver are injected right
// after the last user-declared backend block, so the drain backend is never
// the first (default) backend and is declared before the sub that references
// it (avoiding forward-reference errors on Varnish 6). If no user backends
// are found, the drain VCL is inserted right after the import std line.
func injectDrainVCL(vcl, backendName string) string {
	// Strip any existing "import std;" from user VCL — we re-inject it
	// at the top so it appears before the drain subroutine.
	vcl = stripImportStd(vcl)

	versionEnd := vclVersionEnd(vcl)

	importStd := "\nimport std;\n"

	drainVCL := fmt.Sprintf("\nbackend %s {\n  .host = \"127.0.0.1\";\n  .port = \"9\";\n}\n", backendName)
	drainVCL += fmt.Sprintf(`
sub vcl_deliver {
  if (!std.healthy(%s)) {
    set resp.http.Connection = "close";
  }
}
`, backendName)

	// Insert import std right after the version line.
	result := vcl[:versionEnd] + importStd + vcl[versionEnd:]

	// Find insertion point for drain backend + sub: after the last user
	// backend block, or right after import std if no backends exist.
	// The offset is computed on the result string (which now includes
	// the injected import std).
	backendsEnd := backendBlocksEnd(result)
	if backendsEnd == 0 {
		// No user backends — insert right after import std.
		insertPos := versionEnd + len(importStd)
		result = result[:insertPos] + drainVCL + result[insertPos:]
	} else {
		result = result[:backendsEnd] + drainVCL + result[backendsEnd:]
	}

	return result
}
