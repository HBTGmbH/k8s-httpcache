// Package renderer renders VCL templates with endpoint data.
package renderer

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/go-sprout/sprout"
	"github.com/go-sprout/sprout/group/all"

	"k8s-httpcache/internal/watcher"
)

// BackendGroup holds all data for a single backend group, exposed to VCL templates.
type BackendGroup struct {
	Endpoints       []watcher.Endpoint // all endpoints
	Labels          map[string]string  // Service labels
	Annotations     map[string]string  // Service annotations (filtered)
	LocalEndpoints  []watcher.Endpoint // same-zone endpoints
	RemoteEndpoints []watcher.Endpoint // other-zone endpoints
}

type templateData struct {
	Frontends []watcher.Frontend
	Backends  map[string]any
	Values    map[string]any
	Secrets   map[string]any
	LocalZone string // zone of the Varnish pod (empty if unknown)
}

// templateBackendGroup mirrors BackendGroup with map[string]any fields so that
// Sprig dict functions (keys, hasKey, …) work without custom overrides.
type templateBackendGroup struct {
	Endpoints       []watcher.Endpoint
	Labels          map[string]any
	Annotations     map[string]any
	LocalEndpoints  []watcher.Endpoint
	RemoteEndpoints []watcher.Endpoint
}

// toAnyMap converts a typed map[string]V to map[string]any.
func toAnyMap[V any](m map[string]V) map[string]any {
	r := make(map[string]any, len(m))
	for k, v := range m {
		r[k] = v
	}

	return r
}

// splitEndpointsByZone partitions endpoints into local (same-zone) and remote
// (different-zone) slices. An endpoint is considered local if its Zone matches
// the given zone, or if its ForZones hints include the zone (Kubernetes
// Topology Aware Routing). Endpoints with an empty Zone and no matching
// ForZones hint are placed into remote. The zone parameter must be non-empty.
func splitEndpointsByZone(eps []watcher.Endpoint, zone string) ([]watcher.Endpoint, []watcher.Endpoint) {
	var local, remote []watcher.Endpoint
	for _, ep := range eps {
		if ep.Zone == zone || slices.Contains(ep.ForZones, zone) {
			local = append(local, ep)
		} else {
			remote = append(remote, ep)
		}
	}

	return local, remote
}

// Renderer renders VCL from a Go template and a list of frontends.
type Renderer struct {
	tmpl         *template.Template
	prev         *template.Template
	templatePath string
	funcMap      template.FuncMap
	drainBackend string
	delimLeft    string
	delimRight   string
	localZone    string
}

// SetDrainBackend configures the renderer to auto-inject drain VCL using the
// given backend name. An empty name disables injection.
func (r *Renderer) SetDrainBackend(name string) {
	r.drainBackend = name
}

// SetLocalZone sets the topology zone of the local Varnish pod, made available
// as .LocalZone in templates for zone-aware routing decisions.
func (r *Renderer) SetLocalZone(zone string) {
	r.localZone = zone
}

// buildFuncMap returns the template function map for the given library name.
func buildFuncMap(templateFuncs string) template.FuncMap {
	switch templateFuncs {
	case "sprout":
		handler := sprout.New(sprout.WithGroups(all.RegistryGroup()))

		return handler.Build()
	default:
		return sprig.TxtFuncMap()
	}
}

// New parses the template file and returns a Renderer.
func New(templatePath, delimLeft, delimRight, templateFuncs string) (*Renderer, error) {
	funcMap := buildFuncMap(templateFuncs)

	raw, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("reading template %s: %w", templatePath, err)
	}

	tmpl, err := template.New("vcl").Delims(delimLeft, delimRight).Funcs(funcMap).Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parsing template %s: %w", templatePath, err)
	}

	return &Renderer{tmpl: tmpl, templatePath: templatePath, funcMap: funcMap, delimLeft: delimLeft, delimRight: delimRight}, nil
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

	tmpl, err := template.New("vcl").Delims(r.delimLeft, r.delimRight).Funcs(r.funcMap).Parse(string(raw))
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

// Render executes the template with the given frontends, backends, values, and secrets and returns the VCL string.
func (r *Renderer) Render(frontends []watcher.Frontend, backends map[string]BackendGroup, values, secrets map[string]map[string]any) (string, error) {
	if values == nil {
		values = make(map[string]map[string]any)
	}
	if secrets == nil {
		secrets = make(map[string]map[string]any)
	}

	// Convert typed maps to map[string]any so Sprig dict functions (keys,
	// hasKey, get, values, pick, omit) work without custom overrides.
	backendsAny := make(map[string]any, len(backends))
	for name, bg := range backends {
		labels := bg.Labels
		if labels == nil {
			labels = make(map[string]string)
		}
		annotations := bg.Annotations
		if annotations == nil {
			annotations = make(map[string]string)
		}
		var local, remote []watcher.Endpoint
		if r.localZone != "" {
			local, remote = splitEndpointsByZone(bg.Endpoints, r.localZone)
		}
		backendsAny[name] = templateBackendGroup{
			Endpoints:       bg.Endpoints,
			Labels:          toAnyMap(labels),
			Annotations:     toAnyMap(annotations),
			LocalEndpoints:  local,
			RemoteEndpoints: remote,
		}
	}

	var buf bytes.Buffer

	err := r.tmpl.Execute(&buf, templateData{
		Frontends: frontends,
		Backends:  backendsAny,
		Values:    toAnyMap(values),
		Secrets:   toAnyMap(secrets),
		LocalZone: r.localZone,
	})
	if err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	vcl := buf.String()
	if r.drainBackend != "" {
		vcl = injectDrainVCL(vcl, r.drainBackend)
	}

	return vcl, nil
}

// RenderToFile renders the template to a temporary file and returns its path.
func (r *Renderer) RenderToFile(frontends []watcher.Frontend, backends map[string]BackendGroup, values, secrets map[string]map[string]any) (string, error) {
	vcl, err := r.Render(frontends, backends, values, secrets)
	if err != nil {
		return "", err
	}

	f, err := os.CreateTemp("", "k8s-httpcache-*.vcl")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}

	_, err = f.WriteString(vcl)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name()) //nolint:gosec // G703: path from os.CreateTemp, not user input

		return "", fmt.Errorf("writing temp file: %w", err)
	}

	err = f.Close()
	if err != nil {
		_ = os.Remove(f.Name()) //nolint:gosec // G703: path from os.CreateTemp, not user input

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
// and trailing newline) so it can be identified in user VCL.
var importStdRe = regexp.MustCompile(`(?m)^[\t ]*import\s+std\s*;\s*\n?`)

// importStdPositions returns the [start, end) byte offsets of all top-level
// "import std;" occurrences in vcl (skipping those inside /* */ block comments).
func importStdPositions(vcl string) [][2]int {
	locs := importStdRe.FindAllStringIndex(vcl, -1)
	var result [][2]int
	for _, loc := range locs {
		before := vcl[:loc[0]]
		if strings.Count(before, "/*")-strings.Count(before, "*/") > 0 {
			continue // inside a block comment — keep it
		}
		result = append(result, [2]int{loc[0], loc[1]})
	}

	return result
}

// commentOutImportStdFrom comments out top-level "import std;" lines that
// start at or after byte offset from, preserving those inside /* */ block
// comments and those before from.
func commentOutImportStdFrom(vcl string, from int) string {
	positions := importStdPositions(vcl)

	var b strings.Builder
	prev := 0
	for _, pos := range positions {
		if pos[0] < from {
			continue // before threshold — leave it
		}
		_, _ = b.WriteString(vcl[prev:pos[0]])
		matched := vcl[pos[0]:pos[1]]
		_, _ = b.WriteString("// Commented out by k8s-httpcache; moved to the top of the VCL.\n")
		_, _ = b.WriteString("// " + strings.TrimRight(matched, "\n") + "\n")
		prev = pos[1]
	}
	if prev == 0 {
		return vcl // nothing was commented out
	}
	_, _ = b.WriteString(vcl[prev:])

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
				if end < 0 {
					break // unclosed comment, stop scanning
				}
				i = i + 2 + end + 1 // skip past "*/"

				continue
			}
			// Skip // and # line comments.
			if vcl[i] == '#' || (i+1 < len(vcl) && vcl[i] == '/' && vcl[i+1] == '/') {
				nl := strings.IndexByte(vcl[i:], '\n')
				if nl < 0 {
					break // no newline, rest is comment
				}
				i += nl // advance to newline

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
// The drain backend declaration and drain sub vcl_deliver are injected right
// after the last user-declared backend block, so the drain backend is never
// the first (default) backend and is declared before the sub that references
// it (avoiding forward-reference errors on Varnish 6). If no user backends
// are found, the drain VCL is inserted right after the user's "import std;"
// line (if present) or after the version line.
//
// "import std;" is only injected (at the top) when the user VCL does not
// already provide one before the drain insertion point. User "import std;"
// lines that would end up after the injected vcl_deliver are commented out.
func injectDrainVCL(vcl, backendName string) string {
	versionEnd := vclVersionEnd(vcl)
	backendsEnd := backendBlocksEnd(vcl)
	imports := importStdPositions(vcl)

	drainVCL := fmt.Sprintf("\n// Begin k8s-httpcache connection draining.\nbackend %s {\n  .host = \"127.0.0.1\";\n  .port = \"9\";\n}\n// End k8s-httpcache connection draining.\n", backendName)
	drainVCL += fmt.Sprintf(`
// Begin k8s-httpcache connection draining.
sub vcl_deliver {
  if (!std.healthy(%s)) {
    set resp.http.Connection = "close";
  }
}
// End k8s-httpcache connection draining.
`, backendName)

	if backendsEnd > 0 {
		// Has user backends — drain VCL goes after the last backend.
		hasEarlyImport := false
		for _, pos := range imports {
			if pos[0] < backendsEnd {
				hasEarlyImport = true

				break
			}
		}

		// Comment out any "import std;" at or after backendsEnd — those
		// would appear after our vcl_deliver.
		vcl = commentOutImportStdFrom(vcl, backendsEnd)

		if hasEarlyImport {
			// User already has import std before the drain VCL.
			return vcl[:backendsEnd] + drainVCL + vcl[backendsEnd:]
		}

		// No early import — inject ours right after the version line.
		importStd := "\n// Begin k8s-httpcache connection draining.\nimport std;\n// End k8s-httpcache connection draining.\n"
		result := vcl[:versionEnd] + importStd + vcl[versionEnd:]
		// backendsEnd shifted by the inserted text.
		return result[:backendsEnd+len(importStd)] + drainVCL + result[backendsEnd+len(importStd):]
	}

	// No user backends. Look for the first user "import std;" after the
	// version line — if present, insert drain VCL right after it so the
	// user's import is preserved and appears before our vcl_deliver.
	for _, pos := range imports {
		if pos[0] >= versionEnd {
			// Comment out any later duplicates.
			vcl = commentOutImportStdFrom(vcl, pos[1])

			return vcl[:pos[1]] + drainVCL + vcl[pos[1]:]
		}
	}

	// No user import std at all — inject ours + drain VCL after the
	// version line.
	importStd := "\n// Begin k8s-httpcache connection draining.\nimport std;\n// End k8s-httpcache connection draining.\n"
	result := vcl[:versionEnd] + importStd + vcl[versionEnd:]
	insertPos := versionEnd + len(importStd)

	return result[:insertPos] + drainVCL + result[insertPos:]
}
