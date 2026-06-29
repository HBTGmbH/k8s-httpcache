package renderer

import (
	"testing"
	"text/template"
)

// FuzzInjectDrainVCL ensures the drain-injection string surgery never panics on
// arbitrary VCL. injectDrainVCL runs inside Render on the event-loop goroutine,
// where a panic is unrecovered and would crash the controller; the VCL it
// operates on is influenced by the operator's template and by endpoint data, so
// it must tolerate any input (unclosed braces, comments, missing version line,
// backends declared before the version, etc.).
func FuzzInjectDrainVCL(f *testing.F) {
	seeds := []string{
		"vcl 4.1;\nbackend default { .host = \"1.2.3.4\"; }\n",
		"vcl 4.1;\n",
		"vcl 4.1;\nimport std;\nbackend a { .host=\"x\"; .probe = { .url=\"/\"; } }\n",
		"backend early { }\nvcl 4.1;\n",
		"vcl 4.1;\nbackend unclosed {  ",
		"/* vcl 4.1; */ backend x {}",
		"",
		"{",
		"}",
		"vcl 4.1;\nbackend a {} backend b {}\n",
		"vcl 4.1;\nbackend a { // }\n}\n",
		"vcl 4.1;\nbackend a { # }\n}\n",
		"vcl 4.1;\nbackend a { /* } */ }\n",
		"vcl 4.1;\nimport std;\nimport std;\nbackend a {}\n",
		"   vcl   4.1   ;   \n",
		"import std;",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(_ *testing.T, vcl string) {
		// Must never panic on any input.
		_, _ = injectDrainVCL(vcl, "k8s_httpcache_drain")
	})
}

// FuzzRender feeds arbitrary template text through the same parse+execute path
// as production rendering (buildFuncMap + sprig, << >> delimiters), executed
// against representative Values/Secrets. Render runs on the controller's
// event-loop goroutine where a panic is unrecovered, so any template that parses
// must execute without panicking. Parse errors are expected (rejected at load
// time in production) and ignored.
func FuzzRender(f *testing.F) {
	funcMap := buildFuncMap("")
	values := map[string]map[string]any{
		"cfg": {"name": "web", "replicas": 3, "list": []any{"a", "b"}},
	}
	secrets := map[string]map[string]any{"creds": {"token": "s3cr3t-value"}}

	seeds := []string{
		"<< .Values | toJson >>",
		"<< range $k, $v := .Values.cfg >><< $k >>=<< $v >>\n<< end >>",
		"<< .Secrets.creds.token >>",
		"<< .Values.cfg.replicas | int >>",
		"plain text, no actions",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(_ *testing.T, tmplText string) {
		tmpl, err := template.New("vcl").Delims("<<", ">>").Funcs(funcMap).Parse(tmplText)
		if err != nil {
			return
		}

		r := &Renderer{tmpl: tmpl}
		_, _ = r.Render(nil, nil, values, secrets)
	})
}
