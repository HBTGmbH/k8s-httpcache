package renderer

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"text/template"

	"k8s-httpcache/internal/watcher"
)

type templateData struct {
	Frontends []watcher.Frontend
	Backends  map[string][]watcher.Endpoint
}

// Renderer renders VCL from a Go template and a list of frontends.
type Renderer struct {
	tmpl         *template.Template
	prev         *template.Template
	templatePath string
	funcMap      template.FuncMap
}

// New parses the template file and returns a Renderer.
func New(templatePath string) (*Renderer, error) {
	funcMap := template.FuncMap{
		"replace": strings.ReplaceAll,
	}

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

// Render executes the template with the given frontends and backends and returns the VCL string.
func (r *Renderer) Render(frontends []watcher.Frontend, backends map[string][]watcher.Endpoint) (string, error) {
	var buf bytes.Buffer
	if err := r.tmpl.Execute(&buf, templateData{Frontends: frontends, Backends: backends}); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	return buf.String(), nil
}

// RenderToFile renders the template to a temporary file and returns its path.
func (r *Renderer) RenderToFile(frontends []watcher.Frontend, backends map[string][]watcher.Endpoint) (string, error) {
	vcl, err := r.Render(frontends, backends)
	if err != nil {
		return "", err
	}

	f, err := os.CreateTemp("", "k8s-httpcache-*.vcl")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}

	if _, err := f.WriteString(vcl); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("writing temp file: %w", err)
	}

	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("closing temp file: %w", err)
	}

	return f.Name(), nil
}
