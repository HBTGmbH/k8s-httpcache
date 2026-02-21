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
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return f.Name()
}

func TestNew_InvalidPath(t *testing.T) {
	_, err := New("/nonexistent/path.tmpl")
	if err == nil {
		t.Fatal("expected error for nonexistent template path")
	}
}

func TestNew_InvalidTemplate(t *testing.T) {
	path := writeTempTemplate(t, `<< if >>`)
	_, err := New(path)
	if err == nil {
		t.Fatal("expected error for invalid template syntax")
	}
}

func TestNew_CustomDelimiters(t *testing.T) {
	// Ensure {{ }} is treated as literal text, not Go template syntax.
	path := writeTempTemplate(t, `{{ .Helm }} << .Frontends >>`)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, err := r.Render(nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "{{ .Helm }}") {
		t.Errorf("expected {{ .Helm }} preserved as literal, got: %s", out)
	}
}

func TestRender_EmptyFrontends(t *testing.T) {
	tmpl := `vcl 4.1;
<< if .Frontends >>HAS_BACKENDS<< else >>NO_BACKENDS<< end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out, err := r.Render(nil, nil, nil)
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
	tmpl := `<< range .Frontends >>backend << .Name >> { .host = "<< .IP >>"; .port = "<< .Port >>"; }
<< end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	frontends := []watcher.Frontend{
		{IP: "10.0.0.1", Port: 8080, Name: "pod-a"},
		{IP: "10.0.0.2", Port: 8080, Name: "pod-b"},
	}

	out, err := r.Render(frontends, nil, nil)
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
			path := writeTempTemplate(t, tt.tmpl)
			r, err := New(path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			out, err := r.Render(frontends, nil, nil)
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
	path := writeTempTemplate(t, `BEFORE`)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out, _ := r.Render(nil, nil, nil)
	if out != "BEFORE" {
		t.Fatalf("expected BEFORE, got: %s", out)
	}

	// Mutate the template file on disk.
	if err := os.WriteFile(path, []byte(`AFTER`), 0644); err != nil {
		t.Fatalf("writing updated template: %v", err)
	}

	if err := r.Reload(); err != nil {
		t.Fatalf("reload error: %v", err)
	}

	out, _ = r.Render(nil, nil, nil)
	if out != "AFTER" {
		t.Errorf("expected AFTER after reload, got: %s", out)
	}
}

func TestRollback(t *testing.T) {
	path := writeTempTemplate(t, `OLD`)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Update template on disk and reload.
	if err := os.WriteFile(path, []byte(`NEW`), 0644); err != nil {
		t.Fatalf("writing updated template: %v", err)
	}
	if err := r.Reload(); err != nil {
		t.Fatalf("reload error: %v", err)
	}

	out, _ := r.Render(nil, nil, nil)
	if out != "NEW" {
		t.Fatalf("expected NEW after reload, got: %s", out)
	}

	// Rollback should restore the old template.
	r.Rollback()

	out, _ = r.Render(nil, nil, nil)
	if out != "OLD" {
		t.Errorf("expected OLD after rollback, got: %s", out)
	}
}

func TestReload_InvalidTemplate(t *testing.T) {
	path := writeTempTemplate(t, `VALID`)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Write broken syntax to the file.
	if err := os.WriteFile(path, []byte(`<< if >>`), 0644); err != nil {
		t.Fatalf("writing broken template: %v", err)
	}

	if err := r.Reload(); err == nil {
		t.Fatal("expected error for invalid template syntax")
	}

	// Old template should still work.
	out, err := r.Render(nil, nil, nil)
	if err != nil {
		t.Fatalf("render should still work with old template: %v", err)
	}
	if out != "VALID" {
		t.Errorf("expected old template output VALID, got: %s", out)
	}
}

func TestRender_ExampleTemplate(t *testing.T) {
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
	r, err := New(path)
	if err != nil {
		t.Fatalf("failed to load example template: %v", err)
	}

	t.Run("empty", func(t *testing.T) {
		out, err := r.Render(nil, nil, nil)
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
		frontends := []watcher.Frontend{
			{IP: "10.0.0.1", Port: 8080, Name: "web-pod-0"},
			{IP: "10.0.0.2", Port: 8080, Name: "web-pod-1"},
		}
		out, err := r.Render(frontends, nil, nil)
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
		backends := map[string][]watcher.Endpoint{
			"api": {
				{IP: "10.1.0.1", Port: 3000, Name: "api-pod-0"},
				{IP: "10.1.0.2", Port: 3000, Name: "api-pod-1"},
			},
		}
		out, err := r.Render(nil, backends, nil)
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
	tmpl := `vcl 4.1; << range .Frontends >><< .IP >> << end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	frontends := []watcher.Frontend{
		{IP: "10.0.0.5", Port: 80, Name: "x"},
	}

	outPath, err := r.RenderToFile(frontends, nil, nil)
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
	tmpl := `<< range $name, $eps := .Backends >>` +
		`<< range $eps >>backend << .Name >>_<< $name >> { .host = "<< .IP >>"; .port = "<< .Port >>"; }
<< end >><< end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
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

	out, err := r.Render(nil, backends, nil)
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
	// Template that will fail during execution (call undefined method).
	tmpl := `<< range .Frontends >><< .Nonexistent >><< end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	frontends := []watcher.Frontend{
		{IP: "10.0.0.1", Port: 80, Name: "a"},
	}

	_, err = r.RenderToFile(frontends, nil, nil)
	if err == nil {
		t.Fatal("expected error from RenderToFile with broken template execution")
	}
}

func TestReload_FileRemoved(t *testing.T) {
	path := writeTempTemplate(t, `VALID`)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Remove the template file.
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing template: %v", err)
	}

	if err := r.Reload(); err == nil {
		t.Fatal("expected error when template file is missing")
	}

	// Old template should still work.
	out, err := r.Render(nil, nil, nil)
	if err != nil {
		t.Fatalf("render should still work: %v", err)
	}
	if out != "VALID" {
		t.Errorf("expected VALID, got: %s", out)
	}
}

func TestRollback_NoOp(t *testing.T) {
	path := writeTempTemplate(t, `ONLY`)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Rollback without prior Reload should be a no-op.
	r.Rollback()

	out, err := r.Render(nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if out != "ONLY" {
		t.Errorf("expected ONLY, got: %s", out)
	}
}

func TestRender_FrontendsAndBackendsTogether(t *testing.T) {
	tmpl := `frontends:<< range .Frontends >> << .IP >><< end >>` +
		` backends:<< range $name, $eps := .Backends >><< range $eps >> << .IP >>/<< $name >><< end >><< end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
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

	out, err := r.Render(frontends, backends, nil)
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
	tmpl := `greeting=<< index .Values.tuning "greeting" >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	values := map[string]map[string]any{
		"tuning": {"greeting": "hello-world"},
	}

	out, err := r.Render(nil, nil, values)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(out, "greeting=hello-world") {
		t.Errorf("expected greeting=hello-world, got: %s", out)
	}
}

func TestRender_EmptyValues(t *testing.T) {
	tmpl := `<< if .Values >>HAS_VALUES<< else >>NO_VALUES<< end >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// nil values should be normalised to empty map — template should still work.
	out, err := r.Render(nil, nil, nil)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	// Empty map is falsy in Go templates.
	if !strings.Contains(out, "NO_VALUES") {
		t.Errorf("expected NO_VALUES for nil values, got: %s", out)
	}
}

func TestRender_ValuesWithFrontendsAndBackends(t *testing.T) {
	tmpl := `ttl=<< index .Values.config "ttl" >> frontends=<< len .Frontends >> backends=<< len .Backends >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
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

	out, err := r.Render(frontends, backends, values)
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
	tmpl := `vcl 4.1;

sub vcl_recv {
  set req.backend_hint = origin;
}

sub vcl_deliver {
  set resp.http.X-Test = "1";
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetDrainBackend("drain_flag")

	out, err := r.Render(nil, nil, nil)
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
	tmpl := `vcl 4.1;

import std;

sub vcl_recv {
  set req.backend_hint = origin;
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetDrainBackend("drain_flag")

	out, err := r.Render(nil, nil, nil)
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
	tmpl := `vcl 4.1;

sub vcl_recv {
  set req.backend_hint = origin;
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No SetDrainBackend call — drain is disabled.

	out, err := r.Render(nil, nil, nil)
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
	tmpl := `vcl 4.1;

sub vcl_recv {
  set req.backend_hint = origin;
}
`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetDrainBackend("my_drain")

	out, err := r.Render(nil, nil, nil)
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
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r.SetDrainBackend("drain_flag")

	out, err := r.Render(nil, nil, nil)
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

func TestRender_NestedValues(t *testing.T) {
	tmpl := `host=<< index .Values.server "host" >> port=<< index .Values.server "port" >>`
	path := writeTempTemplate(t, tmpl)
	r, err := New(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	values := map[string]map[string]any{
		"server": {
			"host": "example.com",
			"port": float64(8080),
		},
	}

	out, err := r.Render(nil, nil, values)
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
