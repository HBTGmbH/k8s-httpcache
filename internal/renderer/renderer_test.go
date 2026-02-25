package renderer

import (
	"os"
	"strings"
	"testing"

	"k8s-httpcache/internal/watcher"
)

func writeTempTemplate(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.vcl.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(content)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	return f.Name()
}

func TestNew_InvalidPath(t *testing.T) {
	t.Parallel()
	_, err := New("/nonexistent/path.tmpl", "<<", ">>")
	if err == nil {
		t.Fatal("expected error for nonexistent template path")
	}
}

func TestNew_InvalidTemplate(t *testing.T) {
	t.Parallel()
	path := writeTempTemplate(t, `<< if >>`)
	_, err := New(path, "<<", ">>")
	if err == nil {
		t.Fatal("expected error for invalid template syntax")
	}
}

func TestNew_CustomDelimiters(t *testing.T) {
	t.Parallel()
	// Ensure {{ }} is treated as literal text, not Go template syntax.
	path := writeTempTemplate(t, `{{ .Helm }} << .Frontends >>`)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "{{ .Helm }}") {
		t.Errorf("expected {{ .Helm }} preserved as literal, got: %s", out)
	}
}

func TestNew_ConfigurableDelimiters(t *testing.T) {
	t.Parallel()
	// Use {{ }} delimiters instead of << >>.
	path := writeTempTemplate(t, `hello {{ .Frontends }} world`)
	r, err := New(path, "{{", "}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "hello [] world") {
		t.Errorf("expected template to use {{ }} delimiters, got: %s", out)
	}
}

func TestNew_ConfigurableDelimitersLiteralPassthrough(t *testing.T) {
	t.Parallel()
	// When using {{ }} delimiters, << >> should pass through as literal text.
	path := writeTempTemplate(t, `<< literal >> {{ len .Frontends }}`)
	r, err := New(path, "{{", "}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "<< literal >>") {
		t.Errorf("expected << literal >> preserved as literal text, got: %s", out)
	}
	if !strings.Contains(out, "0") {
		t.Errorf("expected len .Frontends = 0, got: %s", out)
	}
}

func TestNew_ConfigurableDelimitersWithFrontends(t *testing.T) {
	t.Parallel()
	tmpl := `{{ range .Frontends }}backend {{ .Name }} { .host = "{{ .IP }}"; .port = "{{ .Port }}"; }
{{ end }}`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "{{", "}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	frontends := []watcher.Frontend{
		{IP: "10.0.0.1", Port: 8080, Name: "pod-a"},
		{IP: "10.0.0.2", Port: 8080, Name: "pod-b"},
	}

	out, err := r.Render(frontends, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, `backend pod-a`) {
		t.Errorf("expected backend pod-a, got: %s", out)
	}
	if !strings.Contains(out, `.host = "10.0.0.2"`) {
		t.Errorf("expected host 10.0.0.2, got: %s", out)
	}
}

func TestNew_ConfigurableDelimitersWithBackends(t *testing.T) {
	t.Parallel()
	tmpl := `{{ range $name, $eps := .Backends }}{{ range $eps }}{{ .Name }}_{{ $name }}={{ .IP }}:{{ .Port }} {{ end }}{{ end }}`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "{{", "}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	backends := map[string][]watcher.Endpoint{
		"api": {
			{IP: "10.1.0.1", Port: 3000, Name: "api-pod-0"},
		},
	}

	out, err := r.Render(nil, backends, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "api-pod-0_api=10.1.0.1:3000") {
		t.Errorf("expected backend endpoint data, got: %s", out)
	}
}

func TestNew_ConfigurableDelimitersWithValues(t *testing.T) {
	t.Parallel()
	tmpl := `ttl={{ index .Values.config "ttl" }}`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "{{", "}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	values := map[string]map[string]any{
		"config": {"ttl": "300"},
	}

	out, err := r.Render(nil, nil, values, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "ttl=300") {
		t.Errorf("expected ttl=300, got: %s", out)
	}
}

func TestNew_ConfigurableDelimitersSprigFunctions(t *testing.T) {
	t.Parallel()
	tmpl := `{{ "hello world" | upper | replace " " "_" }}`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "{{", "}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if out != "HELLO_WORLD" {
		t.Errorf("expected HELLO_WORLD, got: %s", out)
	}
}

func TestNew_ConfigurableDelimitersRenderToFile(t *testing.T) {
	t.Parallel()
	tmpl := `vcl 4.1; {{ range .Frontends }}{{ .IP }} {{ end }}`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "{{", "}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	frontends := []watcher.Frontend{
		{IP: "10.0.0.5", Port: 80, Name: "x"},
	}

	outPath, err := r.RenderToFile(frontends, nil, nil, nil)
	if err != nil {
		t.Fatalf("RenderToFile error: %v", err)
	}
	defer func() { _ = os.Remove(outPath) }()

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if !strings.Contains(string(data), "10.0.0.5") {
		t.Errorf("expected IP in file, got: %s", data)
	}
}

func TestNew_ConfigurableDelimitersReload(t *testing.T) {
	t.Parallel()
	path := writeTempTemplate(t, `BEFORE {{ .Frontends }}`)
	r, err := New(path, "{{", "}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out, _ := r.Render(nil, nil, nil, nil)
	if !strings.Contains(out, "BEFORE") {
		t.Fatalf("expected BEFORE, got: %s", out)
	}

	// Update template and reload — delimiters should be preserved.
	err = os.WriteFile(path, []byte(`AFTER {{ .Frontends }}`), 0o644)
	if err != nil {
		t.Fatalf("writing updated template: %v", err)
	}
	err = r.Reload()
	if err != nil {
		t.Fatalf("reload error: %v", err)
	}
	out, _ = r.Render(nil, nil, nil, nil)
	if !strings.Contains(out, "AFTER") {
		t.Errorf("expected AFTER after reload, got: %s", out)
	}
}

func TestNew_ConfigurableDelimitersRollback(t *testing.T) {
	t.Parallel()
	path := writeTempTemplate(t, `OLD {{ len .Frontends }}`)
	r, err := New(path, "{{", "}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Update and reload.
	err = os.WriteFile(path, []byte(`NEW {{ len .Frontends }}`), 0o644)
	if err != nil {
		t.Fatalf("writing updated template: %v", err)
	}
	err = r.Reload()
	if err != nil {
		t.Fatalf("reload error: %v", err)
	}
	out, _ := r.Render(nil, nil, nil, nil)
	if !strings.Contains(out, "NEW") {
		t.Fatalf("expected NEW after reload, got: %s", out)
	}

	// Rollback should restore the old template.
	r.Rollback()
	out, _ = r.Render(nil, nil, nil, nil)
	if !strings.Contains(out, "OLD") {
		t.Errorf("expected OLD after rollback, got: %s", out)
	}
}

func TestNew_ConfigurableDelimitersInvalidTemplate(t *testing.T) {
	t.Parallel()
	path := writeTempTemplate(t, `{{ if }}`)
	_, err := New(path, "{{", "}}")
	if err == nil {
		t.Fatal("expected error for invalid template syntax with custom delimiters")
	}
}

func TestNew_ConfigurableDelimitersDrainVCL(t *testing.T) {
	t.Parallel()
	tmpl := `vcl 4.1;

backend origin {
  .host = "127.0.0.1";
  .port = "8080";
}

sub vcl_deliver {
  set resp.http.X-Test = "1";
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "{{", "}}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetDrainBackend("drain_flag")

	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	if !strings.Contains(out, `backend drain_flag {`) {
		t.Error("expected drain_flag backend declaration")
	}
	if !strings.Contains(out, `std.healthy(drain_flag)`) {
		t.Error("expected std.healthy(drain_flag) in drain check")
	}
	if !strings.Contains(out, "import std;") {
		t.Error("expected import std to be injected")
	}
}

func TestNew_UnusualDelimiters(t *testing.T) {
	t.Parallel()
	// Verify that arbitrary multi-character delimiters work.
	tmpl := `result=[% len .Frontends %]`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "[%", "%]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if out != "result=0" {
		t.Errorf("expected result=0, got: %s", out)
	}
}

func TestRender_EmptyFrontends(t *testing.T) {
	t.Parallel()
	tmpl := `vcl 4.1;
<< if .Frontends >>HAS_BACKENDS<< else >>NO_BACKENDS<< end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "NO_BACKENDS") {
		t.Errorf("expected NO_BACKENDS branch, got: %s", out)
	}
	if strings.Contains(out, "HAS_BACKENDS") {
		t.Errorf("unexpected HAS_BACKENDS branch in output: %s", out)
	}
}

func TestRender_WithFrontends(t *testing.T) {
	t.Parallel()
	tmpl := `<< range .Frontends >>backend << .Name >> { .host = "<< .IP >>"; .port = "<< .Port >>"; }
<< end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	frontends := []watcher.Frontend{
		{IP: "10.0.0.1", Port: 8080, Name: "pod-a"},
		{IP: "10.0.0.2", Port: 8080, Name: "pod-b"},
	}

	out, err := r.Render(frontends, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	for _, fe := range frontends {
		if !strings.Contains(out, fe.IP) {
			t.Errorf("expected IP %s in output", fe.IP)
		}
		if !strings.Contains(out, fe.Name) {
			t.Errorf("expected Name %s in output", fe.Name)
		}
	}
	if !strings.Contains(out, "8080") {
		t.Error("expected port 8080 in output")
	}
}

func TestRender_SprigFunctions(t *testing.T) {
	t.Parallel()
	frontends := []watcher.Frontend{
		{IP: "10.0.0.1", Port: 80, Name: "my-cool-pod"},
	}

	tests := []struct {
		name     string
		tmpl     string
		expected string
	}{
		{
			name:     "replace",
			tmpl:     `<< range .Frontends >><< replace "-" "_" .Name >><< end >>`,
			expected: "my_cool_pod",
		},
		{
			name:     "upper",
			tmpl:     `<< range .Frontends >><< upper .Name >><< end >>`,
			expected: "MY-COOL-POD",
		},
		{
			name:     "lower",
			tmpl:     `<< range .Frontends >><< lower .Name >><< end >>`,
			expected: "my-cool-pod",
		},
		{
			name:     "title",
			tmpl:     `<< range .Frontends >><< title .Name >><< end >>`,
			expected: "My-Cool-Pod",
		},
		{
			name:     "contains",
			tmpl:     `<< range .Frontends >><< if contains "cool" .Name >>yes<< end >><< end >>`,
			expected: "yes",
		},
		{
			name:     "hasPrefix",
			tmpl:     `<< range .Frontends >><< if hasPrefix "my-" .Name >>yes<< end >><< end >>`,
			expected: "yes",
		},
		{
			name:     "hasSuffix",
			tmpl:     `<< range .Frontends >><< if hasSuffix "-pod" .Name >>yes<< end >><< end >>`,
			expected: "yes",
		},
		{
			name:     "trimPrefix",
			tmpl:     `<< range .Frontends >><< trimPrefix "my-" .Name >><< end >>`,
			expected: "cool-pod",
		},
		{
			name:     "trimSuffix",
			tmpl:     `<< range .Frontends >><< trimSuffix "-pod" .Name >><< end >>`,
			expected: "my-cool",
		},
		{
			name:     "trim",
			tmpl:     `<< "  hello  " | trim >>`,
			expected: "hello",
		},
		{
			name:     "default",
			tmpl:     `<< "" | default "fallback" >>`,
			expected: "fallback",
		},
		{
			name:     "default_nonempty",
			tmpl:     `<< range .Frontends >><< .Name | default "fallback" >><< end >>`,
			expected: "my-cool-pod",
		},
		{
			name:     "quote",
			tmpl:     `<< range .Frontends >><< .IP | quote >><< end >>`,
			expected: `"10.0.0.1"`,
		},
		{
			name:     "squote",
			tmpl:     `<< range .Frontends >><< .IP | squote >><< end >>`,
			expected: `'10.0.0.1'`,
		},
		{
			name:     "ternary",
			tmpl:     `<< range .Frontends >><< ternary "found" "missing" (contains "cool" .Name) >><< end >>`,
			expected: "found",
		},
		{
			name:     "add",
			tmpl:     `<< range .Frontends >><< add .Port 1000 >><< end >>`,
			expected: "1080",
		},
		{
			name:     "mul",
			tmpl:     `<< range .Frontends >><< mul .Port 2 >><< end >>`,
			expected: "160",
		},
		{
			name:     "len",
			tmpl:     `<< len .Frontends >>`,
			expected: "1",
		},
		{
			name:     "substr",
			tmpl:     `<< range .Frontends >><< substr 0 7 .Name >><< end >>`,
			expected: "my-cool",
		},
		{
			name:     "repeat",
			tmpl:     `<< "ab" | repeat 3 >>`,
			expected: "ababab",
		},
		{
			name:     "nospace",
			tmpl:     `<< "a b c" | nospace >>`,
			expected: "abc",
		},
		{
			name:     "pipeline",
			tmpl:     `<< range .Frontends >><< .Name | trimPrefix "my-" | upper >><< end >>`,
			expected: "COOL-POD",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := writeTempTemplate(t, tt.tmpl)
			r, err := New(path, "<<", ">>")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			out, err := r.Render(frontends, nil, nil, nil)
			if err != nil {
				t.Fatalf("render error: %v", err)
			}
			if !strings.Contains(out, tt.expected) {
				t.Errorf("expected %q in output, got: %s", tt.expected, out)
			}
		})
	}
}

func TestReload(t *testing.T) {
	t.Parallel()
	path := writeTempTemplate(t, `BEFORE`)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out, _ := r.Render(nil, nil, nil, nil)
	if out != "BEFORE" {
		t.Fatalf("expected BEFORE, got: %s", out)
	}

	// Mutate the template file on disk.
	err = os.WriteFile(path, []byte(`AFTER`), 0o644)
	if err != nil {
		t.Fatalf("writing updated template: %v", err)
	}

	err = r.Reload()
	if err != nil {
		t.Fatalf("reload error: %v", err)
	}

	out, _ = r.Render(nil, nil, nil, nil)
	if out != "AFTER" {
		t.Errorf("expected AFTER after reload, got: %s", out)
	}
}

func TestRollback(t *testing.T) {
	t.Parallel()
	path := writeTempTemplate(t, `OLD`)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Update template on disk and reload.
	err = os.WriteFile(path, []byte(`NEW`), 0o644)
	if err != nil {
		t.Fatalf("writing updated template: %v", err)
	}
	err = r.Reload()
	if err != nil {
		t.Fatalf("reload error: %v", err)
	}

	out, _ := r.Render(nil, nil, nil, nil)
	if out != "NEW" {
		t.Fatalf("expected NEW after reload, got: %s", out)
	}

	// Rollback should restore the old template.
	r.Rollback()

	out, _ = r.Render(nil, nil, nil, nil)
	if out != "OLD" {
		t.Errorf("expected OLD after rollback, got: %s", out)
	}
}

func TestReload_InvalidTemplate(t *testing.T) {
	t.Parallel()
	path := writeTempTemplate(t, `VALID`)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Write broken syntax to the file.
	err = os.WriteFile(path, []byte(`<< if >>`), 0o644)
	if err != nil {
		t.Fatalf("writing broken template: %v", err)
	}

	err = r.Reload()
	if err == nil {
		t.Fatal("expected error for invalid template syntax")
	}

	// Old template should still work.
	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render should still work with old template: %v", err)
	}
	if out != "VALID" {
		t.Errorf("expected old template output VALID, got: %s", out)
	}
}

func TestRender_ExampleTemplate(t *testing.T) {
	t.Parallel()
	// Test against a realistic VCL template to catch regressions.
	const exampleVCL = `vcl 4.1;

import directors;
import std;

backend origin {
    .host = "127.0.0.1";
    .port = "8080";
}

<<- if .Frontends >>
<< range .Frontends >>
backend << .Name >> {
    .host = "<< .IP >>";
    .port = "<< .Port >>";
}
<< end >>
<<- end >>

backend drain_flag {
  .host = "127.0.0.1";
  .port = "9";
}

sub vcl_init {
<<- if .Frontends >>
    new cluster = directors.shard();
    << range .Frontends ->>
    cluster.add_backend(<< .Name >>);
    << end >>
    cluster.reconfigure();
<<- end >>
<<- range $name, $eps := .Backends >>
    new backend_<< $name >> = directors.round_robin();
    <<- range $eps >>
    backend_<< $name >>.add_backend(<< .Name >>_<< $name >>);
    <<- end >>
<<- end >>
}

sub vcl_recv {
    if (req.http.X-Shard-Routed) {
        unset req.http.X-Shard-Routed;
        set req.backend_hint = origin;
        return (hash);
    }

<<- range $name, $_ := .Backends >>
    if (req.url ~ "^/<< $name >>/") {
        set req.backend_hint = backend_<< $name >>.backend();
        return (pass);
    }
<<- end >>

<<- if .Frontends >>
    set req.backend_hint = cluster.backend();
    set req.http.X-Shard-Routed = "true";
<<- else >>
    set req.backend_hint = origin;
<<- end >>
}

<<- if .Backends >>
<<- range $name, $eps := .Backends >>
<< range $eps >>
backend << .Name >>_<< $name >> {
    .host = "<< .IP >>";
    .port = "<< .Port >>";
}
<< end >>
<<- end >>
<<- end >>

sub vcl_backend_response {
    set beresp.ttl = 120s;
    set beresp.grace = 60s;
}`
	path := writeTempTemplate(t, exampleVCL)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("failed to load example template: %v", err)
	}

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		out, err := r.Render(nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("render error: %v", err)
		}
		if !strings.Contains(out, "backend_hint = origin") {
			t.Error("expected direct-to-origin fallback for empty frontends")
		}
		if strings.Contains(out, "cluster.backend()") {
			t.Error("should not reference cluster.backend() with no frontends")
		}
		if strings.Contains(out, "directors.shard") {
			t.Error("should not create shard director with no frontends")
		}
	})

	t.Run("with_frontends", func(t *testing.T) {
		t.Parallel()
		frontends := []watcher.Frontend{
			{IP: "10.0.0.1", Port: 8080, Name: "web-pod-0"},
			{IP: "10.0.0.2", Port: 8080, Name: "web-pod-1"},
		}
		out, err := r.Render(frontends, nil, nil, nil)
		if err != nil {
			t.Fatalf("render error: %v", err)
		}
		// Peer backends declared.
		if !strings.Contains(out, `backend web-pod-0`) {
			t.Error("expected backend web-pod-0")
		}
		if !strings.Contains(out, `backend web-pod-1`) {
			t.Error("expected backend web-pod-1")
		}
		if !strings.Contains(out, `.host = "10.0.0.1"`) {
			t.Error("expected host 10.0.0.1")
		}
		if !strings.Contains(out, `.port = "8080"`) {
			t.Error("expected port 8080")
		}
		// Shard director with consistent hashing.
		if !strings.Contains(out, "directors.shard()") {
			t.Error("expected shard director")
		}
		if !strings.Contains(out, "cluster.add_backend(web-pod-0)") {
			t.Error("expected cluster.add_backend with pod name")
		}
		if !strings.Contains(out, "cluster.reconfigure()") {
			t.Error("expected cluster.reconfigure() after adding backends")
		}
		// Self-routing via X-Shard-Routed header.
		if !strings.Contains(out, "req.backend_hint = cluster.backend()") {
			t.Error("expected backend_hint set to shard cluster")
		}
		if !strings.Contains(out, "X-Shard-Routed") {
			t.Error("expected X-Shard-Routed header for self-routing")
		}
		if !strings.Contains(out, "backend_hint = origin") {
			t.Error("expected origin fallback on second hop")
		}
	})

	t.Run("with_backends", func(t *testing.T) {
		t.Parallel()
		backends := map[string][]watcher.Endpoint{
			"api": {
				{IP: "10.1.0.1", Port: 3000, Name: "api-pod-0"},
				{IP: "10.1.0.2", Port: 3000, Name: "api-pod-1"},
			},
		}
		out, err := r.Render(nil, backends, nil, nil)
		if err != nil {
			t.Fatalf("render error: %v", err)
		}
		// Backend declarations.
		if !strings.Contains(out, "backend api-pod-0_api") {
			t.Error("expected backend api-pod-0_api")
		}
		if !strings.Contains(out, "backend api-pod-1_api") {
			t.Error("expected backend api-pod-1_api")
		}
		if !strings.Contains(out, `.host = "10.1.0.1"`) {
			t.Error("expected host 10.1.0.1")
		}
		if !strings.Contains(out, `.port = "3000"`) {
			t.Error("expected port 3000")
		}
		// Round-robin director.
		if !strings.Contains(out, "directors.round_robin()") {
			t.Error("expected round_robin director for api backend")
		}
		if !strings.Contains(out, "backend_api.add_backend(api-pod-0_api)") {
			t.Error("expected backend_api.add_backend(api-pod-0_api)")
		}
		if !strings.Contains(out, "backend_api.add_backend(api-pod-1_api)") {
			t.Error("expected backend_api.add_backend(api-pod-1_api)")
		}
		// URL routing.
		if !strings.Contains(out, `req.url ~ "^/api/"`) {
			t.Error("expected URL routing for /api/")
		}
		if !strings.Contains(out, "backend_api.backend()") {
			t.Error("expected backend_api.backend() hint")
		}
	})
}

func TestRenderToFile(t *testing.T) {
	t.Parallel()
	tmpl := `vcl 4.1; << range .Frontends >><< .IP >> << end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	frontends := []watcher.Frontend{
		{IP: "10.0.0.5", Port: 80, Name: "x"},
	}

	outPath, err := r.RenderToFile(frontends, nil, nil, nil)
	if err != nil {
		t.Fatalf("RenderToFile error: %v", err)
	}
	defer func() { _ = os.Remove(outPath) }()

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if !strings.Contains(string(data), "10.0.0.5") {
		t.Errorf("expected IP in file, got: %s", data)
	}
}

func TestRender_WithBackends(t *testing.T) {
	t.Parallel()
	tmpl := `<< range $name, $eps := .Backends >>` +
		`<< range $eps >>backend << .Name >>_<< $name >> { .host = "<< .IP >>"; .port = "<< .Port >>"; }
<< end >><< end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	backends := map[string][]watcher.Endpoint{
		"api": {
			{IP: "10.1.0.1", Port: 3000, Name: "api-pod-0"},
			{IP: "10.1.0.2", Port: 3000, Name: "api-pod-1"},
		},
		"auth": {
			{IP: "10.2.0.1", Port: 8080, Name: "auth-pod-0"},
		},
	}

	out, err := r.Render(nil, backends, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	if !strings.Contains(out, "api-pod-0_api") {
		t.Errorf("expected api-pod-0_api in output, got: %s", out)
	}
	if !strings.Contains(out, "api-pod-1_api") {
		t.Errorf("expected api-pod-1_api in output, got: %s", out)
	}
	if !strings.Contains(out, "auth-pod-0_auth") {
		t.Errorf("expected auth-pod-0_auth in output, got: %s", out)
	}
	if !strings.Contains(out, "10.1.0.1") {
		t.Errorf("expected IP 10.1.0.1 in output")
	}
	if !strings.Contains(out, "3000") {
		t.Errorf("expected port 3000 in output")
	}
}

func TestRenderToFile_RenderError(t *testing.T) {
	t.Parallel()
	// Template that will fail during execution (call undefined method).
	tmpl := `<< range .Frontends >><< .Nonexistent >><< end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	frontends := []watcher.Frontend{
		{IP: "10.0.0.1", Port: 80, Name: "a"},
	}

	_, err = r.RenderToFile(frontends, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error from RenderToFile with broken template execution")
	}
}

func TestReload_FileRemoved(t *testing.T) {
	t.Parallel()
	path := writeTempTemplate(t, `VALID`)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Remove the template file.
	err = os.Remove(path)
	if err != nil {
		t.Fatalf("removing template: %v", err)
	}

	err = r.Reload()
	if err == nil {
		t.Fatal("expected error when template file is missing")
	}

	// Old template should still work.
	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render should still work: %v", err)
	}
	if out != "VALID" {
		t.Errorf("expected VALID, got: %s", out)
	}
}

func TestRollback_NoOp(t *testing.T) {
	t.Parallel()
	path := writeTempTemplate(t, `ONLY`)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Rollback without prior Reload should be a no-op.
	r.Rollback()

	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if out != "ONLY" {
		t.Errorf("expected ONLY, got: %s", out)
	}
}

func TestRender_FrontendsAndBackendsTogether(t *testing.T) {
	t.Parallel()
	tmpl := `frontends:<< range .Frontends >> << .IP >><< end >>` +
		` backends:<< range $name, $eps := .Backends >><< range $eps >> << .IP >>/<< $name >><< end >><< end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	frontends := []watcher.Frontend{
		{IP: "10.0.0.1", Port: 8080, Name: "web-0"},
	}
	backends := map[string][]watcher.Endpoint{
		"api": {
			{IP: "10.1.0.1", Port: 3000, Name: "api-0"},
		},
	}

	out, err := r.Render(frontends, backends, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	if !strings.Contains(out, "frontends: 10.0.0.1") {
		t.Errorf("expected frontends with 10.0.0.1, got: %s", out)
	}
	if !strings.Contains(out, "10.1.0.1/api") {
		t.Errorf("expected backend 10.1.0.1/api, got: %s", out)
	}
}

func TestRender_WithValues(t *testing.T) {
	t.Parallel()
	tmpl := `greeting=<< index .Values.tuning "greeting" >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	values := map[string]map[string]any{
		"tuning": {"greeting": "hello-world"},
	}

	out, err := r.Render(nil, nil, values, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "greeting=hello-world") {
		t.Errorf("expected greeting=hello-world, got: %s", out)
	}
}

func TestRender_EmptyValues(t *testing.T) {
	t.Parallel()
	tmpl := `<< if .Values >>HAS_VALUES<< else >>NO_VALUES<< end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// nil values should be normalised to empty map — template should still work.
	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	// Empty map is falsy in Go templates.
	if !strings.Contains(out, "NO_VALUES") {
		t.Errorf("expected NO_VALUES for nil values, got: %s", out)
	}
}

func TestRender_ValuesWithFrontendsAndBackends(t *testing.T) {
	t.Parallel()
	tmpl := `ttl=<< index .Values.config "ttl" >> frontends=<< len .Frontends >> backends=<< len .Backends >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	frontends := []watcher.Frontend{{IP: "10.0.0.1", Port: 80, Name: "pod-1"}}
	backends := map[string][]watcher.Endpoint{
		"api": {{IP: "10.1.0.1", Port: 3000, Name: "api-0"}},
	}
	values := map[string]map[string]any{
		"config": {"ttl": "120"},
	}

	out, err := r.Render(frontends, backends, values, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "ttl=120") {
		t.Errorf("expected ttl=120, got: %s", out)
	}
	if !strings.Contains(out, "frontends=1") {
		t.Errorf("expected frontends=1, got: %s", out)
	}
	if !strings.Contains(out, "backends=1") {
		t.Errorf("expected backends=1, got: %s", out)
	}
}

func TestRender_DrainVCLInjection(t *testing.T) {
	t.Parallel()
	tmpl := `vcl 4.1;

sub vcl_recv {
  set req.backend_hint = origin;
}

sub vcl_deliver {
  set resp.http.X-Test = "1";
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetDrainBackend("drain_flag")

	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	// Backend declaration present.
	if !strings.Contains(out, `backend drain_flag {`) {
		t.Error("expected drain_flag backend declaration")
	}
	// Drain vcl_deliver present.
	if !strings.Contains(out, `std.healthy(drain_flag)`) {
		t.Error("expected std.healthy(drain_flag) in drain check")
	}
	if !strings.Contains(out, `resp.http.Connection = "close"`) {
		t.Error("expected Connection: close in drain vcl_deliver")
	}
	// import std injected.
	if !strings.Contains(out, "import std;") {
		t.Error("expected import std to be injected")
	}
	// No readiness vcl_recv injected.
	if strings.Contains(out, "synth(200)") || strings.Contains(out, "synth(503)") {
		t.Error("expected no readiness health check to be injected")
	}
}

func TestRender_DrainVCLImportStdDedup(t *testing.T) {
	t.Parallel()
	tmpl := `vcl 4.1;

import std;

sub vcl_recv {
  set req.backend_hint = origin;
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetDrainBackend("drain_flag")

	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	// "import std" should appear exactly once (user's copy stripped, ours injected).
	count := strings.Count(out, "import std")
	if count != 1 {
		t.Errorf("expected import std exactly once, got %d occurrences", count)
	}
}

func TestRender_NoDrainVCLWhenDisabled(t *testing.T) {
	t.Parallel()
	tmpl := `vcl 4.1;

sub vcl_recv {
  set req.backend_hint = origin;
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No SetDrainBackend call — drain is disabled.

	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	if strings.Contains(out, "drain_flag") {
		t.Error("expected no drain_flag when drain is disabled")
	}
	if strings.Contains(out, "std.healthy") {
		t.Error("expected no std.healthy when drain is disabled")
	}
}

func TestRender_DrainVCLCustomBackendName(t *testing.T) {
	t.Parallel()
	tmpl := `vcl 4.1;

sub vcl_recv {
  set req.backend_hint = origin;
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetDrainBackend("my_drain")

	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	if !strings.Contains(out, `backend my_drain {`) {
		t.Error("expected custom backend name my_drain")
	}
	if !strings.Contains(out, `std.healthy(my_drain)`) {
		t.Error("expected std.healthy(my_drain)")
	}
	// The default name should not appear.
	if strings.Contains(out, "drain_flag") {
		t.Error("unexpected drain_flag when custom name is used")
	}
}

func TestRender_DrainVCLOrdering(t *testing.T) {
	t.Parallel()
	tmpl := `vcl 4.1;

backend origin {
  # USER_BACKEND
  .host = "127.0.0.1";
  .port = "8080";
}

sub vcl_deliver {
  # USER_DELIVER
  set resp.http.X-Test = "1";
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetDrainBackend("drain_flag")

	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	// Drain vcl_deliver (with Connection: close) should appear before USER_DELIVER.
	drainPos := strings.Index(out, `resp.http.Connection = "close"`)
	userDeliverPos := strings.Index(out, `# USER_DELIVER`)
	if drainPos < 0 || userDeliverPos < 0 {
		t.Fatal("could not find drain or user deliver markers")
	}
	if drainPos >= userDeliverPos {
		t.Error("drain vcl_deliver should appear before user vcl_deliver")
	}

	// Drain backend should appear after user backend (so user's first
	// backend remains the Varnish default).
	userBackendPos := strings.Index(out, `# USER_BACKEND`)
	drainBackendPos := strings.Index(out, `backend drain_flag {`)
	if userBackendPos < 0 || drainBackendPos < 0 {
		t.Fatal("could not find user backend or drain backend markers")
	}
	if drainBackendPos <= userBackendPos {
		t.Error("drain backend should appear after user-defined backends")
	}
}

func TestRender_DrainVCLBackendWithProbe(t *testing.T) {
	t.Parallel()
	tmpl := `vcl 4.1;

backend origin {
  .host = "127.0.0.1";
  .port = "8080";
  .probe = {
    .url = "/healthz";
    .interval = 5s;
  }
}

sub vcl_deliver {
  # USER_DELIVER
  set resp.http.X-Test = "1";
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetDrainBackend("drain_flag")

	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	// Drain backend should appear after the user backend (with nested probe braces).
	originPos := strings.Index(out, `backend origin {`)
	drainBackendPos := strings.Index(out, `backend drain_flag {`)
	if originPos < 0 || drainBackendPos < 0 {
		t.Fatalf("could not find backend markers\noutput:\n%s", out)
	}
	if drainBackendPos <= originPos {
		t.Error("drain backend should appear after user backend with probe")
	}

	// Drain sub should appear before user vcl_deliver.
	drainPos := strings.Index(out, `resp.http.Connection = "close"`)
	userDeliverPos := strings.Index(out, `# USER_DELIVER`)
	if drainPos < 0 || userDeliverPos < 0 {
		t.Fatalf("could not find drain or user deliver markers\noutput:\n%s", out)
	}
	if drainPos >= userDeliverPos {
		t.Error("drain vcl_deliver should appear before user vcl_deliver")
	}
}

func TestRender_DrainVCLNoBackends(t *testing.T) {
	t.Parallel()
	tmpl := `vcl 4.1;

sub vcl_recv {
  set req.backend_hint = origin;
}

sub vcl_deliver {
  # USER_DELIVER
  set resp.http.X-Test = "1";
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetDrainBackend("drain_flag")

	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	// Drain backend + sub should be present.
	if !strings.Contains(out, `backend drain_flag {`) {
		t.Errorf("expected drain_flag backend\noutput:\n%s", out)
	}
	if !strings.Contains(out, `std.healthy(drain_flag)`) {
		t.Errorf("expected std.healthy check\noutput:\n%s", out)
	}

	// Both should appear after import std.
	importPos := strings.Index(out, "import std;")
	drainBackendPos := strings.Index(out, `backend drain_flag {`)
	drainSubPos := strings.Index(out, `std.healthy(drain_flag)`)
	if importPos < 0 || drainBackendPos < 0 || drainSubPos < 0 {
		t.Fatalf("missing markers\noutput:\n%s", out)
	}
	if drainBackendPos <= importPos {
		t.Error("drain backend should appear after import std")
	}
	if drainSubPos <= drainBackendPos {
		t.Error("drain sub should appear after drain backend")
	}

	// Drain sub should appear before user vcl_deliver.
	userDeliverPos := strings.Index(out, `# USER_DELIVER`)
	if userDeliverPos < 0 {
		t.Fatalf("could not find user deliver marker\noutput:\n%s", out)
	}
	if drainSubPos >= userDeliverPos {
		t.Error("drain vcl_deliver should appear before user vcl_deliver")
	}
}

func TestVclVersionEnd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		vcl  string
		want int
	}{
		{
			name: "standard version line",
			vcl:  "vcl 4.1;\nbackend default { }\n",
			want: len("vcl 4.1;\n"),
		},
		{
			name: "no version line",
			vcl:  "backend default {\n  .host = \"127.0.0.1\";\n}\n",
			want: 0,
		},
		{
			name: "indented version line",
			vcl:  "  vcl 4.1;\nbackend default { }\n",
			want: len("  vcl 4.1;\n"),
		},
		{
			name: "version without minor",
			vcl:  "vcl 4;\nbackend default { }\n",
			want: len("vcl 4;\n"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := vclVersionEnd(tt.vcl)
			if got != tt.want {
				t.Errorf("vclVersionEnd() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBackendBlocksEnd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		vcl  string
		want int // expected byte offset (0 = not found)
	}{
		{
			name: "no_backends",
			vcl:  "vcl 4.1;\n\nsub vcl_recv {\n}\n",
			want: 0,
		},
		{
			name: "single_backend",
			vcl: "vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  .port = \"8080\";\n}\n" +
				"sub vcl_recv {\n}\n",
			want: len("vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  .port = \"8080\";\n}\n"),
		},
		{
			name: "multiple_backends",
			vcl: "vcl 4.1;\n" +
				"backend first {\n  .host = \"10.0.0.1\";\n  .port = \"80\";\n}\n" +
				"backend second {\n  .host = \"10.0.0.2\";\n  .port = \"80\";\n}\n" +
				"sub vcl_recv {\n}\n",
			want: len("vcl 4.1;\n" +
				"backend first {\n  .host = \"10.0.0.1\";\n  .port = \"80\";\n}\n" +
				"backend second {\n  .host = \"10.0.0.2\";\n  .port = \"80\";\n}\n"),
		},
		{
			name: "backend_with_probe",
			vcl: "vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  .probe = {\n    .url = \"/\";\n  }\n}\n" +
				"sub vcl_recv {\n}\n",
			want: len("vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  .probe = {\n    .url = \"/\";\n  }\n}\n"),
		},
		{
			name: "hyphenated_name",
			vcl: "vcl 4.1;\n" +
				"backend web-pod-0 {\n  .host = \"10.0.0.1\";\n  .port = \"8080\";\n}\n" +
				"sub vcl_recv {\n}\n",
			want: len("vcl 4.1;\n" +
				"backend web-pod-0 {\n  .host = \"10.0.0.1\";\n  .port = \"8080\";\n}\n"),
		},
		{
			name: "underscore_name",
			vcl: "vcl 4.1;\n" +
				"backend api_pod_0_api {\n  .host = \"10.0.0.1\";\n  .port = \"3000\";\n}\n",
			want: len("vcl 4.1;\n" +
				"backend api_pod_0_api {\n  .host = \"10.0.0.1\";\n  .port = \"3000\";\n}\n"),
		},
		{
			name: "indented_backend",
			vcl: "vcl 4.1;\n" +
				"  backend origin {\n    .host = \"127.0.0.1\";\n  }\n" +
				"sub vcl_recv {\n}\n",
			want: len("vcl 4.1;\n" +
				"  backend origin {\n    .host = \"127.0.0.1\";\n  }\n"),
		},
		{
			name: "backends_before_and_after_subs",
			vcl: "vcl 4.1;\n" +
				"backend first {\n  .host = \"10.0.0.1\";\n}\n" +
				"sub vcl_init {\n}\n" +
				"backend last {\n  .host = \"10.0.0.2\";\n}\n" +
				"sub vcl_recv {\n}\n",
			want: len("vcl 4.1;\n" +
				"backend first {\n  .host = \"10.0.0.1\";\n}\n" +
				"sub vcl_init {\n}\n" +
				"backend last {\n  .host = \"10.0.0.2\";\n}\n"),
		},
		{
			name: "no_trailing_newline",
			vcl:  "vcl 4.1;\nbackend origin {\n  .host = \"127.0.0.1\";\n}",
			want: len("vcl 4.1;\nbackend origin {\n  .host = \"127.0.0.1\";\n}"),
		},
		{
			name: "hash_comment",
			vcl: "vcl 4.1;\n" +
				"# backend commented_out {\n" +
				"#   .host = \"127.0.0.1\";\n" +
				"# }\n" +
				"backend real {\n  .host = \"10.0.0.1\";\n}\n",
			want: len("vcl 4.1;\n" +
				"# backend commented_out {\n" +
				"#   .host = \"127.0.0.1\";\n" +
				"# }\n" +
				"backend real {\n  .host = \"10.0.0.1\";\n}\n"),
		},
		{
			name: "slash_comment",
			vcl: "vcl 4.1;\n" +
				"// backend commented_out {\n" +
				"//   .host = \"127.0.0.1\";\n" +
				"// }\n" +
				"backend real {\n  .host = \"10.0.0.1\";\n}\n",
			want: len("vcl 4.1;\n" +
				"// backend commented_out {\n" +
				"//   .host = \"127.0.0.1\";\n" +
				"// }\n" +
				"backend real {\n  .host = \"10.0.0.1\";\n}\n"),
		},
		{
			name: "block_comment_only_backend",
			vcl: "vcl 4.1;\n" +
				"/*\nbackend commented_out {\n  .host = \"127.0.0.1\";\n}\n*/\n" +
				"sub vcl_recv {\n}\n",
			want: 0, // commented-out backend should be ignored
		},
		{
			name: "block_comment_before_real_backend",
			vcl: "vcl 4.1;\n" +
				"/*\nbackend commented_out {\n  .host = \"127.0.0.1\";\n}\n*/\n" +
				"backend real {\n  .host = \"10.0.0.1\";\n}\n",
			want: len("vcl 4.1;\n" +
				"/*\nbackend commented_out {\n  .host = \"127.0.0.1\";\n}\n*/\n" +
				"backend real {\n  .host = \"10.0.0.1\";\n}\n"),
		},
		{
			name: "block_comment_after_real_backend",
			vcl: "vcl 4.1;\n" +
				"backend real {\n  .host = \"10.0.0.1\";\n}\n" +
				"/*\nbackend commented_out {\n  .host = \"127.0.0.2\";\n}\n*/\n",
			want: len("vcl 4.1;\n" +
				"backend real {\n  .host = \"10.0.0.1\";\n}\n"),
		},
		{
			name: "block_comment_open_brace_inside_backend",
			vcl: "vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  /* { */\n  .port = \"8080\";\n}\n" +
				"sub vcl_recv {\n}\n",
			want: len("vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  /* { */\n  .port = \"8080\";\n}\n"),
		},
		{
			name: "block_comment_close_brace_inside_backend",
			vcl: "vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  /* } */\n  .port = \"8080\";\n}\n" +
				"sub vcl_recv {\n}\n",
			want: len("vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  /* } */\n  .port = \"8080\";\n}\n"),
		},
		{
			name: "line_comment_brace_inside_backend",
			vcl: "vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  // }\n  .port = \"8080\";\n}\n",
			want: len("vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  // }\n  .port = \"8080\";\n}\n"),
		},
		{
			name: "hash_comment_brace_inside_backend",
			vcl: "vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  # }\n  .port = \"8080\";\n}\n",
			want: len("vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  # }\n  .port = \"8080\";\n}\n"),
		},
		{
			name: "unclosed_block_comment_inside_backend",
			vcl: "vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  /* unclosed comment\n",
			// The unclosed block comment causes the scanner to break;
			// end stays at openBrace+1 since the closing brace is never found.
			want: len("vcl 4.1;\nbackend origin {"),
		},
		{
			name: "line_comment_at_eof_no_newline_inside_backend",
			vcl: "vcl 4.1;\n" +
				"backend origin {\n  .host = \"127.0.0.1\";\n  // trailing comment no newline",
			// The line comment has no trailing newline, so the scanner breaks;
			// end stays at openBrace+1 since the closing brace is never found.
			want: len("vcl 4.1;\nbackend origin {"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := backendBlocksEnd(tt.vcl)
			if got != tt.want {
				t.Errorf("backendBlocksEnd() = %d, want %d\nvcl:\n%s", got, tt.want, tt.vcl)
			}
		})
	}
}

func TestRender_DrainVCLMultipleBackends(t *testing.T) {
	t.Parallel()
	tmpl := `vcl 4.1;

backend origin {
  .host = "127.0.0.1";
  .port = "8080";
}

<< range .Frontends >>
backend << .Name >> {
  .host = "<< .IP >>";
  .port = "<< .Port >>";
}
<< end >>

sub vcl_deliver {
  # USER_DELIVER
  set resp.http.X-Test = "1";
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetDrainBackend("drain_flag")

	frontends := []watcher.Frontend{
		{IP: "10.0.0.1", Port: 8080, Name: "web-pod-0"},
		{IP: "10.0.0.2", Port: 8080, Name: "web-pod-1"},
	}

	out, err := r.Render(frontends, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	// All user backends should appear before drain backend.
	drainBackendPos := strings.Index(out, `backend drain_flag {`)
	if drainBackendPos < 0 {
		t.Fatalf("drain backend not found\noutput:\n%s", out)
	}
	for _, name := range []string{"origin", "web-pod-0", "web-pod-1"} {
		pos := strings.Index(out, "backend "+name+" {")
		if pos < 0 {
			t.Errorf("backend %s not found\noutput:\n%s", name, out)
		} else if pos >= drainBackendPos {
			t.Errorf("backend %s (pos %d) should appear before drain backend (pos %d)", name, pos, drainBackendPos)
		}
	}

	// Drain sub should appear before user vcl_deliver.
	drainSubPos := strings.Index(out, `resp.http.Connection = "close"`)
	userDeliverPos := strings.Index(out, `# USER_DELIVER`)
	if drainSubPos < 0 || userDeliverPos < 0 {
		t.Fatalf("missing markers\noutput:\n%s", out)
	}
	if drainSubPos >= userDeliverPos {
		t.Error("drain vcl_deliver should appear before user vcl_deliver")
	}

	// Drain backend should appear exactly once.
	count := strings.Count(out, "backend drain_flag {")
	if count != 1 {
		t.Errorf("expected drain backend exactly once, got %d\noutput:\n%s", count, out)
	}
}

func TestRender_DrainVCLImportStdInComment(t *testing.T) {
	t.Parallel()
	tmpl := `vcl 4.1;

/*
import std;
*/

sub vcl_recv {
  set req.backend_hint = origin;
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetDrainBackend("drain_flag")

	out, err := r.Render(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}

	// The "import std;" inside the block comment must be preserved.
	if !strings.Contains(out, "/*\nimport std;\n*/") {
		t.Error("import std inside block comment was incorrectly stripped")
	}
}

func TestRender_DrainVCLImportStdWhitespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		line string
	}{
		{"tabs", "\timport std;"},
		{"multiple_spaces", "    import std;"},
		{"tab_and_spaces", "\t  import std;"},
		{"extra_space_before_semi", "import std ;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tmpl := "vcl 4.1;\n" + tt.line + "\n\nsub vcl_recv {\n  set req.backend_hint = origin;\n}\n"
			path := writeTempTemplate(t, tmpl)
			r, err := New(path, "<<", ">>")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			r.SetDrainBackend("drain_flag")

			out, err := r.Render(nil, nil, nil, nil)
			if err != nil {
				t.Fatalf("render error: %v", err)
			}

			// User's import std should be stripped; only ours should remain.
			count := strings.Count(out, "import std")
			if count != 1 {
				t.Errorf("expected import std exactly once, got %d occurrences\noutput:\n%s", count, out)
			}
		})
	}
}

func TestRender_NestedValues(t *testing.T) {
	t.Parallel()
	tmpl := `host=<< index .Values.server "host" >> port=<< index .Values.server "port" >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	values := map[string]map[string]any{
		"server": {
			"host": "example.com",
			"port": float64(8080),
		},
	}

	out, err := r.Render(nil, nil, values, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "host=example.com") {
		t.Errorf("expected host=example.com, got: %s", out)
	}
	if !strings.Contains(out, "port=8080") {
		t.Errorf("expected port=8080, got: %s", out)
	}
}

func TestNew_Secrets(t *testing.T) {
	t.Parallel()
	path := writeTempTemplate(t, `apikey=<< .Secrets.myapp.apikey >>`)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	secrets := map[string]map[string]any{
		"myapp": {
			"apikey": "s3cr3t",
		},
	}

	out, err := r.Render(nil, nil, nil, secrets)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "apikey=s3cr3t") {
		t.Errorf("expected apikey=s3cr3t, got: %s", out)
	}
}

func TestNew_LocalZoneAndEndpointZone(t *testing.T) {
	t.Parallel()
	tmpl := `zone=<< .LocalZone >>
<<- range $name, $eps := .Backends >>
<<- range $eps >>
<< .Name >>:zone=<< .Zone >>,node=<< .NodeName >>,forZones=<< join "," .ForZones >>
<<- end >>
<<- end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetLocalZone("europe-west3-a")

	backends := map[string][]watcher.Endpoint{
		"api": {
			{IP: "10.0.0.1", Port: 8080, Name: "pod-a", Zone: "europe-west3-a", NodeName: "node-1", ForZones: []string{"europe-west3-a", "europe-west3-b"}},
			{IP: "10.0.0.2", Port: 8080, Name: "pod-b", Zone: "europe-west3-b", NodeName: "node-2"},
		},
	}

	out, err := r.Render(nil, backends, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "zone=europe-west3-a") {
		t.Errorf("expected LocalZone=europe-west3-a, got: %s", out)
	}
	if !strings.Contains(out, "pod-a:zone=europe-west3-a,node=node-1,forZones=europe-west3-a,europe-west3-b") {
		t.Errorf("expected pod-a topology fields, got: %s", out)
	}
	if !strings.Contains(out, "pod-b:zone=europe-west3-b,node=node-2,forZones=") {
		t.Errorf("expected pod-b topology fields, got: %s", out)
	}
}

func TestNew_LocalZoneWeightedDirector(t *testing.T) {
	t.Parallel()
	// Simulate a weighted director pattern that gives higher weight to same-zone backends.
	tmpl := `<<- range $name, $eps := .Backends >><<- range $eps >>weight=<< if eq .Zone $.LocalZone >>10<< else >>1<< end >>
<< end >><<- end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetLocalZone("europe-west3-a")

	backends := map[string][]watcher.Endpoint{
		"api": {
			{IP: "10.0.0.1", Port: 8080, Name: "pod-a", Zone: "europe-west3-a"},
			{IP: "10.0.0.2", Port: 8080, Name: "pod-b", Zone: "europe-west3-b"},
		},
	}

	out, err := r.Render(nil, backends, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "weight=10") {
		t.Errorf("expected weight=10 for same-zone backend, got: %s", out)
	}
	if !strings.Contains(out, "weight=1") {
		t.Errorf("expected weight=1 for different-zone backend, got: %s", out)
	}
}

func TestSplitBackendsByZone(t *testing.T) {
	t.Parallel()

	// Exhaustive Zone × ForZones matrix (localZone = "europe-west3-a"):
	//
	//   Zone         | ForZones                  | Expected | Name
	//   -------------|---------------------------|----------|-----
	//   matches      | nil                       | local    | zone-match-no-hints
	//   matches      | contains local            | local    | zone-match-hints-contain-local
	//   matches      | doesn't contain local     | local    | zone-match-hints-other
	//   different    | nil                       | remote   | zone-diff-no-hints
	//   different    | contains local (only)     | local    | zone-diff-hints-local-only
	//   different    | contains local + others   | local    | zone-diff-hints-local-among-others
	//   different    | doesn't contain local     | remote   | zone-diff-hints-other
	//   empty        | nil                       | remote   | zone-empty-no-hints
	//   empty        | contains local + others   | local    | zone-empty-hints-local-among-others
	//   empty        | doesn't contain local     | remote   | zone-empty-hints-other

	backends := map[string][]watcher.Endpoint{
		"api": {
			// Zone matches localZone
			{Name: "zone-match-no-hints", Zone: "europe-west3-a"},
			{Name: "zone-match-hints-contain-local", Zone: "europe-west3-a", ForZones: []string{"europe-west3-a", "europe-west3-b"}},
			{Name: "zone-match-hints-other", Zone: "europe-west3-a", ForZones: []string{"europe-west3-c"}},
			// Zone is different
			{Name: "zone-diff-no-hints", Zone: "europe-west3-b"},
			{Name: "zone-diff-hints-local-only", Zone: "europe-west3-b", ForZones: []string{"europe-west3-a"}},
			{Name: "zone-diff-hints-local-among-others", Zone: "europe-west3-b", ForZones: []string{"europe-west3-c", "europe-west3-a"}},
			{Name: "zone-diff-hints-other", Zone: "europe-west3-b", ForZones: []string{"europe-west3-c"}},
			// Zone is empty
			{Name: "zone-empty-no-hints"},
			{Name: "zone-empty-hints-local-among-others", ForZones: []string{"europe-west3-a", "europe-west3-b"}},
			{Name: "zone-empty-hints-other", ForZones: []string{"europe-west3-c"}},
		},
	}

	local, remote := splitBackendsByZone(backends, "europe-west3-a")

	wantLocal := []string{
		"zone-match-no-hints",
		"zone-match-hints-contain-local",
		"zone-match-hints-other",
		"zone-diff-hints-local-only",
		"zone-diff-hints-local-among-others",
		"zone-empty-hints-local-among-others",
	}
	if len(local["api"]) != len(wantLocal) {
		t.Fatalf("expected %d local api endpoints, got %d: %v", len(wantLocal), len(local["api"]), local["api"])
	}
	for i, name := range wantLocal {
		if local["api"][i].Name != name {
			t.Errorf("local[api][%d]: expected %s, got %s", i, name, local["api"][i].Name)
		}
	}

	wantRemote := []string{
		"zone-diff-no-hints",
		"zone-diff-hints-other",
		"zone-empty-no-hints",
		"zone-empty-hints-other",
	}
	if len(remote["api"]) != len(wantRemote) {
		t.Fatalf("expected %d remote api endpoints, got %d: %v", len(wantRemote), len(remote["api"]), remote["api"])
	}
	for i, name := range wantRemote {
		if remote["api"][i].Name != name {
			t.Errorf("remote[api][%d]: expected %s, got %s", i, name, remote["api"][i].Name)
		}
	}
}

func TestSplitBackendsByZone_MultipleGroups(t *testing.T) {
	t.Parallel()

	backends := map[string][]watcher.Endpoint{
		"all-local": {
			{Name: "a", Zone: "europe-west3-a"},
			{Name: "b", Zone: "europe-west3-a"},
		},
		"all-remote": {
			{Name: "c", Zone: "europe-west3-b"},
			{Name: "d", Zone: "europe-west3-c"},
		},
		"mixed": {
			{Name: "e", Zone: "europe-west3-a"},
			{Name: "f", Zone: "europe-west3-b"},
		},
	}

	local, remote := splitBackendsByZone(backends, "europe-west3-a")

	if len(local["all-local"]) != 2 {
		t.Errorf("expected 2 local all-local endpoints, got %d", len(local["all-local"]))
	}
	if len(remote["all-local"]) != 0 {
		t.Errorf("expected 0 remote all-local endpoints, got %d", len(remote["all-local"]))
	}
	if len(local["all-remote"]) != 0 {
		t.Errorf("expected 0 local all-remote endpoints, got %d", len(local["all-remote"]))
	}
	if len(remote["all-remote"]) != 2 {
		t.Errorf("expected 2 remote all-remote endpoints, got %d", len(remote["all-remote"]))
	}
	if len(local["mixed"]) != 1 || local["mixed"][0].Name != "e" {
		t.Errorf("expected [e] in local mixed, got %v", local["mixed"])
	}
	if len(remote["mixed"]) != 1 || remote["mixed"][0].Name != "f" {
		t.Errorf("expected [f] in remote mixed, got %v", remote["mixed"])
	}
}

func TestSplitBackendsByZone_EmptyBackends(t *testing.T) {
	t.Parallel()

	local, remote := splitBackendsByZone(map[string][]watcher.Endpoint{}, "europe-west3-a")

	if len(local) != 0 {
		t.Errorf("expected empty local map, got %v", local)
	}
	if len(remote) != 0 {
		t.Errorf("expected empty remote map, got %v", remote)
	}
}

func TestSplitBackendsByZone_NilBackends(t *testing.T) {
	t.Parallel()

	local, remote := splitBackendsByZone(nil, "europe-west3-a")

	if len(local) != 0 {
		t.Errorf("expected empty local map, got %v", local)
	}
	if len(remote) != 0 {
		t.Errorf("expected empty remote map, got %v", remote)
	}
}

func TestRender_LocalRemoteBackends_WithZone(t *testing.T) {
	t.Parallel()

	tmpl := `local:<<- range $name, $eps := .LocalBackends >><<- range $eps >> << .Name >><< end >><<- end >>
remote:<<- range $name, $eps := .RemoteBackends >><<- range $eps >> << .Name >><< end >><<- end >>`

	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetLocalZone("europe-west3-a")

	backends := map[string][]watcher.Endpoint{
		"api": {
			{IP: "10.0.0.1", Port: 8080, Name: "pod-a", Zone: "europe-west3-a"},
			{IP: "10.0.0.2", Port: 8080, Name: "pod-b", Zone: "europe-west3-b"},
		},
	}

	out, err := r.Render(nil, backends, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "local: pod-a") {
		t.Errorf("expected pod-a in local, got: %s", out)
	}
	if !strings.Contains(out, "remote: pod-b") {
		t.Errorf("expected pod-b in remote, got: %s", out)
	}
}

func TestRender_LocalRemoteBackends_EmptyLocalZone(t *testing.T) {
	t.Parallel()

	tmpl := `local=<< len .LocalBackends >> remote=<< len .RemoteBackends >>`

	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// localZone not set — both maps should be empty.

	backends := map[string][]watcher.Endpoint{
		"api": {
			{IP: "10.0.0.1", Port: 8080, Name: "pod-a", Zone: "europe-west3-a"},
		},
	}

	out, err := r.Render(nil, backends, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "local=0") {
		t.Errorf("expected empty LocalBackends when localZone is empty, got: %s", out)
	}
	if !strings.Contains(out, "remote=0") {
		t.Errorf("expected empty RemoteBackends when localZone is empty, got: %s", out)
	}
}

func TestRender_LocalRemoteBackends_ByName(t *testing.T) {
	t.Parallel()

	// Access .LocalBackends and .RemoteBackends by backend name directly.
	tmpl := `local_api:<<- range .LocalBackends.api >> << .Name >><< end >>
remote_api:<<- range .RemoteBackends.api >> << .Name >><< end >>`

	path := writeTempTemplate(t, tmpl)
	r, err := New(path, "<<", ">>")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetLocalZone("europe-west3-a")

	backends := map[string][]watcher.Endpoint{
		"api": {
			{IP: "10.0.0.1", Port: 8080, Name: "pod-a", Zone: "europe-west3-a"},
			{IP: "10.0.0.2", Port: 8080, Name: "pod-b", Zone: "europe-west3-b"},
			{IP: "10.0.0.3", Port: 8080, Name: "pod-c"}, // empty zone → remote
		},
	}

	out, err := r.Render(nil, backends, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "local_api: pod-a") {
		t.Errorf("expected pod-a in local_api, got: %s", out)
	}
	if !strings.Contains(out, "remote_api: pod-b pod-c") {
		t.Errorf("expected pod-b and pod-c in remote_api, got: %s", out)
	}
}

func TestRender_LocalRemoteBackends_FallbackDirectorPattern(t *testing.T) {
	t.Parallel()

	// Mirrors the multi-backend fallback director pattern from the README:
	// uses if $.LocalZone to guard the fallback pattern and falls back to
	// a plain round-robin when zone info is unavailable.
	tmpl := `<<- range $name, $eps := .Backends >>
<<- range $eps >>
backend << .Name >>_<< $name >> { .host = "<< .IP >>"; }
<<- end >>
<<- end >>
sub vcl_init {
<<- range $name, $eps := .Backends >>
<<- if $.LocalZone >>
  new local_<< $name >> = directors.round_robin();
  <<- range index $.LocalBackends $name >>
  local_<< $name >>.add_backend(<< .Name >>_<< $name >>);
  <<- end >>
  new remote_<< $name >> = directors.round_robin();
  <<- range index $.RemoteBackends $name >>
  remote_<< $name >>.add_backend(<< .Name >>_<< $name >>);
  <<- end >>
  new backend_<< $name >> = directors.fallback();
  backend_<< $name >>.add_backend(local_<< $name >>.backend());
  backend_<< $name >>.add_backend(remote_<< $name >>.backend());
<<- else >>
  new backend_<< $name >> = directors.round_robin();
  <<- range $eps >>
  backend_<< $name >>.add_backend(<< .Name >>_<< $name >>);
  <<- end >>
<<- end >>
<<- end >>
}`

	backends := map[string][]watcher.Endpoint{
		"api": {
			{IP: "10.0.0.1", Port: 8080, Name: "pod-a", Zone: "europe-west3-a"},
			{IP: "10.0.0.2", Port: 8080, Name: "pod-b", Zone: "europe-west3-b"},
		},
		"web": {
			{IP: "10.0.1.1", Port: 80, Name: "web-a", Zone: "europe-west3-b"},
			{IP: "10.0.1.2", Port: 80, Name: "web-b", Zone: "europe-west3-a"},
		},
	}

	t.Run("with_zone", func(t *testing.T) {
		t.Parallel()
		path := writeTempTemplate(t, tmpl)
		r, err := New(path, "<<", ">>")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		r.SetLocalZone("europe-west3-a")

		out, err := r.Render(nil, backends, nil, nil)
		if err != nil {
			t.Fatalf("render error: %v", err)
		}

		// All backends are declared (from .Backends).
		for _, name := range []string{"pod-a_api", "pod-b_api", "web-a_web", "web-b_web"} {
			if !strings.Contains(out, "backend "+name) {
				t.Errorf("expected backend declaration for %s, got:\n%s", name, out)
			}
		}

		// Local directors get same-zone backends only.
		if !strings.Contains(out, "local_api.add_backend(pod-a_api)") {
			t.Errorf("expected pod-a in local_api director, got:\n%s", out)
		}
		if strings.Contains(out, "local_api.add_backend(pod-b_api)") {
			t.Errorf("pod-b should not be in local_api director, got:\n%s", out)
		}

		// Remote directors get other-zone backends.
		if !strings.Contains(out, "remote_api.add_backend(pod-b_api)") {
			t.Errorf("expected pod-b in remote_api director, got:\n%s", out)
		}

		// web group: web-b is local, web-a is remote.
		if !strings.Contains(out, "local_web.add_backend(web-b_web)") {
			t.Errorf("expected web-b in local_web director, got:\n%s", out)
		}
		if !strings.Contains(out, "remote_web.add_backend(web-a_web)") {
			t.Errorf("expected web-a in remote_web director, got:\n%s", out)
		}

		// Fallback directors are created for both groups.
		if !strings.Contains(out, "new backend_api = directors.fallback()") {
			t.Errorf("expected fallback director for api, got:\n%s", out)
		}
		if !strings.Contains(out, "new backend_web = directors.fallback()") {
			t.Errorf("expected fallback director for web, got:\n%s", out)
		}

		// No plain round-robin fallback should appear.
		if strings.Contains(out, "new backend_api = directors.round_robin()") {
			t.Errorf("should use fallback director when zone is set, got:\n%s", out)
		}
	})

	t.Run("without_zone", func(t *testing.T) {
		t.Parallel()
		path := writeTempTemplate(t, tmpl)
		r, err := New(path, "<<", ">>")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// localZone not set — should fall back to plain round-robin.

		out, err := r.Render(nil, backends, nil, nil)
		if err != nil {
			t.Fatalf("render error: %v", err)
		}

		// All backends are still declared.
		for _, name := range []string{"pod-a_api", "pod-b_api", "web-a_web", "web-b_web"} {
			if !strings.Contains(out, "backend "+name) {
				t.Errorf("expected backend declaration for %s, got:\n%s", name, out)
			}
		}

		// Plain round-robin directors are used instead of fallback.
		if !strings.Contains(out, "new backend_api = directors.round_robin()") {
			t.Errorf("expected round-robin director for api when no zone, got:\n%s", out)
		}
		if !strings.Contains(out, "new backend_web = directors.round_robin()") {
			t.Errorf("expected round-robin director for web when no zone, got:\n%s", out)
		}

		// All backends are added to the round-robin directors.
		if !strings.Contains(out, "backend_api.add_backend(pod-a_api)") {
			t.Errorf("expected pod-a in backend_api, got:\n%s", out)
		}
		if !strings.Contains(out, "backend_api.add_backend(pod-b_api)") {
			t.Errorf("expected pod-b in backend_api, got:\n%s", out)
		}

		// No local/remote directors should appear.
		if strings.Contains(out, "local_") {
			t.Errorf("should not have local directors when zone is empty, got:\n%s", out)
		}
		if strings.Contains(out, "remote_") {
			t.Errorf("should not have remote directors when zone is empty, got:\n%s", out)
		}

		// No fallback directors.
		if strings.Contains(out, "directors.fallback()") {
			t.Errorf("should not have fallback directors when zone is empty, got:\n%s", out)
		}
	})

	t.Run("all_local", func(t *testing.T) {
		t.Parallel()
		path := writeTempTemplate(t, tmpl)
		r, err := New(path, "<<", ">>")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		r.SetLocalZone("europe-west3-a")

		allLocal := map[string][]watcher.Endpoint{
			"api": {
				{IP: "10.0.0.1", Port: 8080, Name: "pod-a", Zone: "europe-west3-a"},
				{IP: "10.0.0.2", Port: 8080, Name: "pod-b", Zone: "europe-west3-a"},
			},
		}

		out, err := r.Render(nil, allLocal, nil, nil)
		if err != nil {
			t.Fatalf("render error: %v", err)
		}

		// Both endpoints go to local director.
		if !strings.Contains(out, "local_api.add_backend(pod-a_api)") {
			t.Errorf("expected pod-a in local_api, got:\n%s", out)
		}
		if !strings.Contains(out, "local_api.add_backend(pod-b_api)") {
			t.Errorf("expected pod-b in local_api, got:\n%s", out)
		}

		// Remote director is created but empty (no add_backend calls).
		if !strings.Contains(out, "new remote_api = directors.round_robin()") {
			t.Errorf("expected remote_api director to be created, got:\n%s", out)
		}
		if strings.Contains(out, "remote_api.add_backend(") {
			t.Errorf("remote_api should have no backends, got:\n%s", out)
		}

		// Fallback director still wraps both.
		if !strings.Contains(out, "new backend_api = directors.fallback()") {
			t.Errorf("expected fallback director, got:\n%s", out)
		}
	})

	t.Run("all_remote", func(t *testing.T) {
		t.Parallel()
		path := writeTempTemplate(t, tmpl)
		r, err := New(path, "<<", ">>")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		r.SetLocalZone("europe-west3-a")

		allRemote := map[string][]watcher.Endpoint{
			"api": {
				{IP: "10.0.0.1", Port: 8080, Name: "pod-a", Zone: "europe-west3-b"},
				{IP: "10.0.0.2", Port: 8080, Name: "pod-b", Zone: "europe-west3-c"},
			},
		}

		out, err := r.Render(nil, allRemote, nil, nil)
		if err != nil {
			t.Fatalf("render error: %v", err)
		}

		// Local director is created but empty.
		if !strings.Contains(out, "new local_api = directors.round_robin()") {
			t.Errorf("expected local_api director to be created, got:\n%s", out)
		}
		if strings.Contains(out, "local_api.add_backend(") {
			t.Errorf("local_api should have no backends, got:\n%s", out)
		}

		// Both endpoints go to remote director.
		if !strings.Contains(out, "remote_api.add_backend(pod-a_api)") {
			t.Errorf("expected pod-a in remote_api, got:\n%s", out)
		}
		if !strings.Contains(out, "remote_api.add_backend(pod-b_api)") {
			t.Errorf("expected pod-b in remote_api, got:\n%s", out)
		}

		// Fallback director still wraps both.
		if !strings.Contains(out, "new backend_api = directors.fallback()") {
			t.Errorf("expected fallback director, got:\n%s", out)
		}
	})

	t.Run("forZones_hint", func(t *testing.T) {
		t.Parallel()
		path := writeTempTemplate(t, tmpl)
		r, err := New(path, "<<", ">>")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		r.SetLocalZone("europe-west3-a")

		hinted := map[string][]watcher.Endpoint{
			"api": {
				{IP: "10.0.0.1", Port: 8080, Name: "pod-a", Zone: "europe-west3-b", ForZones: []string{"europe-west3-a"}}, // remote zone, but hinted for local → local
				{IP: "10.0.0.2", Port: 8080, Name: "pod-b", Zone: "europe-west3-b"},                                       // remote zone, no hint → remote
				{IP: "10.0.0.3", Port: 8080, Name: "pod-c", Zone: "europe-west3-b", ForZones: []string{"europe-west3-c"}}, // remote zone, hint for other → remote
				{IP: "10.0.0.4", Port: 8080, Name: "pod-d", ForZones: []string{"europe-west3-a", "europe-west3-b"}},       // empty zone, hint contains local → local
				{IP: "10.0.0.5", Port: 8080, Name: "pod-e", Zone: "europe-west3-a", ForZones: []string{"europe-west3-c"}}, // local zone, hint for other → local (zone match wins)
			},
		}

		out, err := r.Render(nil, hinted, nil, nil)
		if err != nil {
			t.Fatalf("render error: %v", err)
		}

		// Local director: pod-a (ForZones match), pod-d (ForZones match), pod-e (Zone match).
		for _, name := range []string{"pod-a", "pod-d", "pod-e"} {
			if !strings.Contains(out, "local_api.add_backend("+name+"_api)") {
				t.Errorf("expected %s in local_api, got:\n%s", name, out)
			}
		}

		// Remote director: pod-b (no hint), pod-c (hint for other zone).
		for _, name := range []string{"pod-b", "pod-c"} {
			if !strings.Contains(out, "remote_api.add_backend("+name+"_api)") {
				t.Errorf("expected %s in remote_api, got:\n%s", name, out)
			}
		}

		// pod-a and pod-d should NOT be in remote.
		for _, name := range []string{"pod-a", "pod-d", "pod-e"} {
			if strings.Contains(out, "remote_api.add_backend("+name+"_api)") {
				t.Errorf("%s should not be in remote_api, got:\n%s", name, out)
			}
		}
	})

	t.Run("mixed_groups", func(t *testing.T) {
		t.Parallel()
		path := writeTempTemplate(t, tmpl)
		r, err := New(path, "<<", ">>")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		r.SetLocalZone("europe-west3-a")

		// One group is all-local, another is all-remote.
		mixed := map[string][]watcher.Endpoint{
			"api": {
				{IP: "10.0.0.1", Port: 8080, Name: "pod-a", Zone: "europe-west3-a"},
			},
			"web": {
				{IP: "10.0.1.1", Port: 80, Name: "web-a", Zone: "europe-west3-b"},
			},
		}

		out, err := r.Render(nil, mixed, nil, nil)
		if err != nil {
			t.Fatalf("render error: %v", err)
		}

		// api: local has pod-a, remote is empty.
		if !strings.Contains(out, "local_api.add_backend(pod-a_api)") {
			t.Errorf("expected pod-a in local_api, got:\n%s", out)
		}
		if strings.Contains(out, "remote_api.add_backend(") {
			t.Errorf("remote_api should be empty, got:\n%s", out)
		}

		// web: remote has web-a, local is empty.
		if !strings.Contains(out, "remote_web.add_backend(web-a_web)") {
			t.Errorf("expected web-a in remote_web, got:\n%s", out)
		}
		if strings.Contains(out, "local_web.add_backend(") {
			t.Errorf("local_web should be empty, got:\n%s", out)
		}
	})
}
