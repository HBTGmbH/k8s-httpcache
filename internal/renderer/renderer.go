// Package renderer renders VCL templates with endpoint data.
package renderer

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/HBTGmbH/k8s-httpcache/internal/watcher"

	"github.com/Masterminds/sprig/v3"
	"github.com/go-sprout/sprout"
	"github.com/go-sprout/sprout/group/all"
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
//
// Renderer is NOT safe for concurrent use: Reload/Rollback swap tmpl/prev and
// Render writes drainPlacementWarned without synchronisation. All calls must
// come from a single goroutine - in this application the main event loop
// (plus the initial Render before the loop starts). Add a mutex before
// calling it from anywhere else (e.g. a preview endpoint).
type Renderer struct {
	tmpl         *template.Template
	prev         *template.Template
	templatePath string
	funcMap      template.FuncMap
	drainBackend string
	delimLeft    string
	delimRight   string
	localZone    string
	log          *slog.Logger // nil falls back to slog.Default(); injectable for tests

	// drainPlacementWarned records that the once-per-template warning about an
	// un-prependable drain sub vcl_deliver has been logged, so a frequently
	// re-rendering loop does not repeat it. Reset on Reload.
	drainPlacementWarned bool
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
//
// Templates should only use DETERMINISTIC functions. The full sprig/sprout
// function map is available, which includes nondeterministic funcs (now,
// date, uuidv4, randAlphaNum, ...): using one makes every render produce
// different output, permanently defeating the rendered-VCL hash dedup - each
// redundant watcher event then triggers a full varnishd VCL compile instead
// of being skipped as unchanged.
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
	// A freshly loaded template may have a different drain-placement layout;
	// allow the once-per-template warning to fire again for it.
	r.drainPlacementWarned = false

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
		var couldNotPrepend bool
		vcl, couldNotPrepend = injectDrainVCL(vcl, r.drainBackend)
		if couldNotPrepend && !r.drainPlacementWarned {
			r.drainPlacementWarned = true
			r.logger().Warn("drain sub vcl_deliver cannot be placed before your vcl_deliver because no backend is declared before it; "+
				"graceful draining may not set \"Connection: close\" if your vcl_deliver returns early - declare at least one backend before your vcl_deliver",
				"drain_backend", r.drainBackend)
		}
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
		_ = os.Remove(f.Name())

		return "", fmt.Errorf("writing temp file: %w", err)
	}

	err = f.Close()
	if err != nil {
		_ = os.Remove(f.Name())

		return "", fmt.Errorf("closing temp file: %w", err)
	}

	return f.Name(), nil
}

// logger returns r.log, or the process default logger when unset.
func (r *Renderer) logger() *slog.Logger {
	if r.log != nil {
		return r.log
	}

	return slog.Default()
}

// vclVersionRe matches a VCL version line like "vcl 4.1;" (with optional
// leading whitespace and trailing newline).
var vclVersionRe = regexp.MustCompile(`(?m)^[\t ]*vcl\s+[\d.]+\s*;\s*\n?`)

// vclVersionEnd returns the byte offset just past the first "vcl X.Y;" line
// that is not inside a /* */ block comment. If no such line is found it
// returns 0. The comment filter matters: a commented-out old version line
// (e.g. a template header carrying "/* vcl 4.0; */") would otherwise be
// matched first, and both drain-injection paths would splice "import std;"
// inside the comment - leaving no active import for the drain sub's
// std.healthy call.
func vclVersionEnd(vcl string) int {
	for _, loc := range vclVersionRe.FindAllStringIndex(vcl, -1) {
		if inCommentOrLongString(vcl, loc[0]) {
			continue
		}

		return loc[1]
	}

	return 0
}

// importStdRe matches an "import std;" line (with optional leading whitespace
// and trailing newline) so it can be identified in user VCL.
var importStdRe = regexp.MustCompile(`(?m)^[\t ]*import\s+std\s*;\s*\n?`)

// inCommentOrLongString reports whether byte offset pos in vcl lies inside a
// /* */ block comment OR a {"..."} long string - both contexts where a
// line-anchored token match (import std, backend, sub vcl_deliver, vcl X.Y;)
// is literal text, not structure: a long string (e.g. a vcl_synth body) can
// contain lines that begin with any of those tokens, and treating one as
// structure would suppress the injected import std or splice the drain sub
// INTO the string literal. The single left-to-right scan also recognises //
// and # line comments and "..." string literals, so a "/*" or "*/" appearing
// inside them does not corrupt the result; a naive delimiter tally would
// miscount those and wrongly treat real code as commented out.
//
// Callers pass pos = the start offset of a regex match, which is always at a
// line boundary (the patterns are anchored with (?m)^), so pos never falls in
// the middle of a token or string.
func inCommentOrLongString(vcl string, pos int) bool {
	const (
		stNormal     = iota
		stBlock      // inside /* */
		stLine       // inside // or # up to newline
		stString     // inside "..."
		stLongString // inside {"..."}
	)
	state := stNormal
	limit := min(pos, len(vcl))
	for i := 0; i < limit; i++ {
		switch state {
		case stNormal:
			switch {
			case vcl[i] == '/' && i+1 < len(vcl) && vcl[i+1] == '*':
				state = stBlock
				i++
			case vcl[i] == '/' && i+1 < len(vcl) && vcl[i+1] == '/':
				state = stLine
				i++
			case vcl[i] == '#':
				state = stLine
			case vcl[i] == '{' && i+1 < len(vcl) && vcl[i+1] == '"':
				state = stLongString
				i++
			case vcl[i] == '"':
				state = stString
			}
		case stBlock:
			if vcl[i] == '*' && i+1 < len(vcl) && vcl[i+1] == '/' {
				state = stNormal
				i++
			}
		case stLine:
			if vcl[i] == '\n' {
				state = stNormal
			}
		case stString:
			if vcl[i] == '"' {
				state = stNormal
			}
		case stLongString:
			if vcl[i] == '"' && i+1 < len(vcl) && vcl[i+1] == '}' {
				state = stNormal
				i++
			}
		}
	}

	return state == stBlock || state == stLongString
}

// importStdPositions returns the [start, end) byte offsets of all top-level
// "import std;" occurrences in vcl (skipping those inside /* */ block comments).
func importStdPositions(vcl string) [][2]int {
	locs := importStdRe.FindAllStringIndex(vcl, -1)
	var result [][2]int
	for _, loc := range locs {
		if inCommentOrLongString(vcl, loc[0]) {
			continue // inside a block comment - keep it
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
			continue // before threshold - leave it
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

// backendBlockEnds returns, in source order, the byte offset just past the
// closing "}" (and any trailing newline) of every top-level backend block in
// vcl. It correctly handles nested brace blocks such as ".probe = { ... }" and
// skips matches inside /* */ block comments. For a malformed (unclosed) block
// the offset just past its opening brace is recorded, matching the historical
// behaviour of backendBlocksEnd.
func backendBlockEnds(vcl string) []int {
	locs := backendBlockRe.FindAllStringIndex(vcl, -1)
	var ends []int
	for _, loc := range locs {
		// Skip matches inside /* */ block comments.
		if inCommentOrLongString(vcl, loc[0]) {
			continue
		}

		// The regex ends with \{, so loc[1]-1 is the opening brace.
		openBrace := loc[1] - 1
		depth := 1
		end := openBrace + 1
		for i := openBrace + 1; i < len(vcl); i++ {
			// Skip {"..."} long strings: braces, quotes, and comment markers
			// inside them are literal text, not structure.
			if vcl[i] == '{' && i+1 < len(vcl) && vcl[i+1] == '"' {
				rel := strings.Index(vcl[i+2:], `"}`)
				if rel < 0 {
					break // unclosed long string, stop scanning
				}
				i = i + 2 + rel + 1 // skip past `"}`

				continue
			}
			// Skip "..." string literals: a brace inside a string (e.g. a
			// probe .url = "/a}b") must not change the depth, and a # or //
			// inside a string must not swallow the rest of the line as a
			// comment - either would end the block at the wrong offset and
			// splice the drain VCL inside the user's backend block.
			if vcl[i] == '"' {
				rel := strings.IndexByte(vcl[i+1:], '"')
				if rel < 0 {
					break // unclosed string, stop scanning
				}
				i = i + 1 + rel // advance to the closing quote

				continue
			}
			// Skip /* */ block comments.
			if i+1 < len(vcl) && vcl[i] == '/' && vcl[i+1] == '*' {
				rel := strings.Index(vcl[i+2:], "*/")
				if rel < 0 {
					break // unclosed comment, stop scanning
				}
				i = i + 2 + rel + 1 // skip past "*/"

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
		ends = append(ends, end)
	}

	return ends
}

// backendBlocksEnd returns the byte offset just past the last backend block in
// vcl, or 0 if there are none.
func backendBlocksEnd(vcl string) int {
	ends := backendBlockEnds(vcl)
	last := 0
	for _, end := range ends {
		if end > last {
			last = end
		}
	}

	return last
}

// firstBackendBlockEnd returns the byte offset just past the first backend
// block in vcl, or 0 if there are none.
func firstBackendBlockEnd(vcl string) int {
	ends := backendBlockEnds(vcl)
	if len(ends) == 0 {
		return 0
	}

	return ends[0]
}

// subVCLDeliverRe matches the start of a top-level "sub vcl_deliver {"
// declaration (with optional leading whitespace).
var subVCLDeliverRe = regexp.MustCompile(`(?m)^[\t ]*sub\s+vcl_deliver\s*\{`)

// firstSubVCLDeliverStart returns the start byte offset of the first top-level
// "sub vcl_deliver" declaration in vcl (skipping those inside /* */ block
// comments), or -1 if there is none.
func firstSubVCLDeliverStart(vcl string) int {
	for _, loc := range subVCLDeliverRe.FindAllStringIndex(vcl, -1) {
		if inCommentOrLongString(vcl, loc[0]) {
			continue // inside a block comment
		}

		return loc[0]
	}

	return -1
}

// injectDrainVCL injects drain VCL into the user template output.
//
// drainImportStd is the injected "import std;" block (std.healthy backs the
// drain check). It is added only when the user VCL has no import std before the
// drain sub's insertion point.
const drainImportStd = "\n// Begin k8s-httpcache connection draining.\nimport std;\n// End k8s-httpcache connection draining.\n"

// drainBackendBlock renders the drain backend declaration (a black-hole backend
// that is marked sick during draining).
func drainBackendBlock(backendName string) string {
	return fmt.Sprintf("\n// Begin k8s-httpcache connection draining.\nbackend %s {\n  .host = \"127.0.0.1\";\n  .port = \"9\";\n}\n// End k8s-httpcache connection draining.\n", backendName)
}

// drainDeliverSub renders the drain sub vcl_deliver that sets Connection: close
// while the drain backend is sick.
func drainDeliverSub(backendName string) string {
	return fmt.Sprintf(`
// Begin k8s-httpcache connection draining.
sub vcl_deliver {
  if (!std.healthy(%s)) {
    set resp.http.Connection = "close";
  }
}
// End k8s-httpcache connection draining.
`, backendName)
}

// injectDrainVCL injects the drain backend and drain sub vcl_deliver into the
// rendered VCL. It returns the augmented VCL and a bool that is true only when
// the drain sub could not be placed before a user vcl_deliver that precedes
// every backend - in that case the historical placement is kept and the caller
// should warn that draining may not set Connection: close if the user's
// vcl_deliver returns early.
//
// Varnish concatenates same-named subs in source order and a return(...) exits
// the whole subroutine, so the drain sub must run before the user's vcl_deliver
// to guarantee Connection: close is set during draining. The common layout
// (vcl_deliver after the backends) already satisfies this with the historical
// end-of-backends placement; only a vcl_deliver declared among/before the
// backends needs the prepend path.
func injectDrainVCL(vcl, backendName string) (string, bool) {
	backendsEnd := backendBlocksEnd(vcl)
	deliverStart := firstSubVCLDeliverStart(vcl)

	// User declares sub vcl_deliver before the last backend: the historical
	// placement would land the drain sub after it. Prepend the drain sub before
	// the user's vcl_deliver, anchoring the drain backend after the first user
	// backend (so it stays non-default and is declared before the sub).
	if deliverStart >= 0 && deliverStart < backendsEnd {
		firstBackendEnd := firstBackendBlockEnd(vcl)
		if firstBackendEnd > 0 && firstBackendEnd <= deliverStart {
			return injectDrainPrepended(vcl, backendName, firstBackendEnd, deliverStart), false
		}

		// The user's vcl_deliver precedes every backend, so there is no backend
		// to declare the drain backend after while keeping it before the sub.
		// Keep the historical placement and signal the caller to warn.
		return injectDrainVCLLegacy(vcl, backendName), true
	}

	return injectDrainVCLLegacy(vcl, backendName), false
}

// injectDrainPrepended inserts the drain sub vcl_deliver immediately before the
// user's first vcl_deliver and the drain backend immediately after the first
// user backend. firstBackendEnd must be <= deliverStart so the backend is
// declared before the sub.
func injectDrainPrepended(vcl, backendName string, firstBackendEnd, deliverStart int) string {
	// import std must precede the drain sub: reuse a user import that already
	// appears before it, otherwise inject one after the version line. Either
	// way, user "import std;" lines at or after the drain sub would duplicate
	// the surviving import ("Module std already imported" compile error), so
	// comment them out - mirroring injectDrainVCLLegacy. Commenting out only
	// rewrites text at or after deliverStart, so the insertion offsets
	// (firstBackendEnd <= deliverStart, versionEnd < deliverStart) stay valid.
	hasEarlyImport := false
	for _, pos := range importStdPositions(vcl) {
		if pos[0] < deliverStart {
			hasEarlyImport = true

			break
		}
	}
	vcl = commentOutImportStdFrom(vcl, deliverStart)

	// Insert from the highest offset downward so lower offsets stay valid.
	out := vcl[:deliverStart] + drainDeliverSub(backendName) + vcl[deliverStart:]
	out = out[:firstBackendEnd] + drainBackendBlock(backendName) + out[firstBackendEnd:]

	if hasEarlyImport {
		return out
	}
	versionEnd := vclVersionEnd(vcl)
	if versionEnd > deliverStart {
		// The matched "vcl X.Y;" token sits after the drain sub (e.g. inside
		// a trailing comment of a template with no top version line);
		// splicing the import there would place it after the sub that uses
		// std. Inject at the top instead.
		versionEnd = 0
	}

	return out[:versionEnd] + drainImportStd + out[versionEnd:]
}

// injectDrainVCLLegacy injects the drain backend declaration and drain sub
// vcl_deliver right after the last user-declared backend block, so the drain
// backend is never the first (default) backend and is declared before the sub
// that references it (avoiding forward-reference errors on Varnish 6). If no
// user backends are found, the drain VCL is inserted right after the user's
// "import std;" line (if present) or after the version line.
//
// "import std;" is only injected (at the top) when the user VCL does not
// already provide one before the drain insertion point. User "import std;"
// lines that would end up after the injected vcl_deliver are commented out.
func injectDrainVCLLegacy(vcl, backendName string) string {
	versionEnd := vclVersionEnd(vcl)
	backendsEnd := backendBlocksEnd(vcl)
	imports := importStdPositions(vcl)

	drainVCL := drainBackendBlock(backendName) + drainDeliverSub(backendName)

	if backendsEnd > 0 {
		// Has user backends - drain VCL goes after the last backend.
		hasEarlyImport := false
		for _, pos := range imports {
			if pos[0] < backendsEnd {
				hasEarlyImport = true

				break
			}
		}

		// Comment out any "import std;" at or after backendsEnd - those
		// would appear after our vcl_deliver.
		vcl = commentOutImportStdFrom(vcl, backendsEnd)

		if hasEarlyImport {
			// User already has import std before the drain VCL.
			return vcl[:backendsEnd] + drainVCL + vcl[backendsEnd:]
		}

		// No early import - inject ours right after the version line.
		importStd := drainImportStd
		result := vcl[:versionEnd] + importStd + vcl[versionEnd:]
		// backendsEnd shifted by the inserted text.
		return result[:backendsEnd+len(importStd)] + drainVCL + result[backendsEnd+len(importStd):]
	}

	// No user backends. Look for the first user "import std;" after the
	// version line - if present, insert drain VCL right after it so the
	// user's import is preserved and appears before our vcl_deliver.
	for _, pos := range imports {
		if pos[0] >= versionEnd {
			// Comment out any later duplicates.
			vcl = commentOutImportStdFrom(vcl, pos[1])

			return vcl[:pos[1]] + drainVCL + vcl[pos[1]:]
		}
	}

	// No user import std at all - inject ours + drain VCL after the
	// version line.
	importStd := "\n// Begin k8s-httpcache connection draining.\nimport std;\n// End k8s-httpcache connection draining.\n"
	result := vcl[:versionEnd] + importStd + vcl[versionEnd:]
	insertPos := versionEnd + len(importStd)

	return result[:insertPos] + drainVCL + result[insertPos:]
}
