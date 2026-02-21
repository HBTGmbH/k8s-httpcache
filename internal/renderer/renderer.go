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

// injectDrainVCL injects drain VCL around the user template output.
//
// The drain vcl_deliver is prepended (right after the version line) so it
// always runs, even if a user-defined vcl_deliver returns early.
// "import std;" is always prepended (and stripped from the user VCL to avoid
// duplicates) because it must appear before the drain subroutine.
//
// The drain backend declaration is appended after all user content so it
// never becomes Varnish's default backend (Varnish uses the first declared
// backend as the implicit default).
func injectDrainVCL(vcl, backendName string) string {
	// Strip any existing "import std;" from user VCL — we re-inject it
	// at the top so it appears before the drain subroutine.
	vcl = stripImportStd(vcl)

	pos := vclVersionEnd(vcl)

	// Drain preamble: import std + drain subroutine.
	var prepend strings.Builder
	_, _ = fmt.Fprintf(&prepend, `
import std;

sub vcl_deliver {
  if (!std.healthy(%s)) {
    set resp.http.Connection = "close";
  }
}
`, backendName)

	// Drain-flag backend declaration — appended after all user backends
	// so it does not become the default backend.
	var appendVCL strings.Builder
	_, _ = fmt.Fprintf(&appendVCL, "\nbackend %s {\n  .host = \"127.0.0.1\";\n  .port = \"9\";\n}\n", backendName)

	return vcl[:pos] + prepend.String() + vcl[pos:] + appendVCL.String()
}
