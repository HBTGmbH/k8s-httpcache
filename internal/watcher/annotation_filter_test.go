package watcher

import (
	"maps"
	"testing"
)

func TestBuildAnnotationFilter_ExactMatch(t *testing.T) {
	t.Parallel()
	f := BuildAnnotationFilter([]string{"kubectl.kubernetes.io/last-applied-configuration"})
	if f == nil {
		t.Fatal("expected non-nil filter")
	}
	if !f("kubectl.kubernetes.io/last-applied-configuration") {
		t.Error("expected exact match to be excluded")
	}
	if f("kubectl.kubernetes.io/other") {
		t.Error("did not expect non-matching key to be excluded")
	}
}

func TestBuildAnnotationFilter_PrefixMatch(t *testing.T) {
	t.Parallel()
	f := BuildAnnotationFilter([]string{"kubectl.kubernetes.io/*"})
	if f == nil {
		t.Fatal("expected non-nil filter")
	}
	if !f("kubectl.kubernetes.io/last-applied-configuration") {
		t.Error("expected prefix match to be excluded")
	}
	if !f("kubectl.kubernetes.io/anything") {
		t.Error("expected prefix match to be excluded")
	}
	if f("example.com/something") {
		t.Error("did not expect non-matching key to be excluded")
	}
}

func TestBuildAnnotationFilter_Mixed(t *testing.T) {
	t.Parallel()
	f := BuildAnnotationFilter([]string{
		"kubectl.kubernetes.io/last-applied-configuration",
		"internal.example.com/*",
	})
	if f == nil {
		t.Fatal("expected non-nil filter")
	}
	if !f("kubectl.kubernetes.io/last-applied-configuration") {
		t.Error("expected exact match to be excluded")
	}
	if !f("internal.example.com/foo") {
		t.Error("expected prefix match to be excluded")
	}
	if f("example.com/version") {
		t.Error("did not expect non-matching key to be excluded")
	}
}

func TestBuildAnnotationFilter_NoMatch(t *testing.T) {
	t.Parallel()
	f := BuildAnnotationFilter([]string{"specific-key"})
	if f("other-key") {
		t.Error("did not expect non-matching key to be excluded")
	}
}

func TestBuildAnnotationFilter_EmptyPatterns(t *testing.T) {
	t.Parallel()
	f := BuildAnnotationFilter(nil)
	if f != nil {
		t.Error("expected nil filter for nil patterns")
	}

	f = BuildAnnotationFilter([]string{})
	if f != nil {
		t.Error("expected nil filter for empty patterns")
	}
}

func TestFilterAnnotations(t *testing.T) {
	t.Parallel()

	exclude := BuildAnnotationFilter([]string{
		"kubectl.kubernetes.io/last-applied-configuration",
		"internal.example.com/*",
	})

	input := map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": `{"big":"json"}`,
		"internal.example.com/debug":                       "true",
		"example.com/version":                              "v1",
		"example.com/tier":                                 "backend",
	}

	got := filterAnnotations(input, exclude)

	want := map[string]string{
		"example.com/version": "v1",
		"example.com/tier":    "backend",
	}
	if !maps.Equal(got, want) {
		t.Errorf("filterAnnotations() = %v, want %v", got, want)
	}
}

func TestFilterAnnotations_NilExclude(t *testing.T) {
	t.Parallel()

	input := map[string]string{"foo": "bar"}
	got := filterAnnotations(input, nil)

	// Should return the original map unchanged.
	if !maps.Equal(got, input) {
		t.Errorf("filterAnnotations(nil exclude) = %v, want %v", got, input)
	}
}

func TestFilterAnnotations_NilAnnotations(t *testing.T) {
	t.Parallel()

	exclude := BuildAnnotationFilter([]string{"foo"})
	got := filterAnnotations(nil, exclude)

	if got != nil {
		t.Errorf("filterAnnotations(nil annotations) = %v, want nil", got)
	}
}

// TestBuildAnnotationFilter_CombinedDefaultAndUser simulates the pattern used
// in main.go: [slices.Concat](DefaultExcludeAnnotations, userPatterns).
func TestBuildAnnotationFilter_CombinedDefaultAndUser(t *testing.T) {
	t.Parallel()

	// Simulate: defaults (exact) + user patterns (prefix).
	patterns := append([]string{}, DefaultExcludeAnnotations...)
	patterns = append(patterns, "internal.example.com/*", "custom-key")

	f := BuildAnnotationFilter(patterns)
	if f == nil {
		t.Fatal("expected non-nil filter")
	}

	// Default exact match should work.
	if !f("kubectl.kubernetes.io/last-applied-configuration") {
		t.Error("expected default exact match to be excluded")
	}
	// User prefix match should work.
	if !f("internal.example.com/debug") {
		t.Error("expected user prefix match to be excluded")
	}
	if !f("internal.example.com/trace-id") {
		t.Error("expected user prefix match to be excluded")
	}
	// User exact match should work.
	if !f("custom-key") {
		t.Error("expected user exact match to be excluded")
	}
	// Non-matching key should not be excluded.
	if f("example.com/version") {
		t.Error("did not expect non-matching key to be excluded")
	}
}

func TestFilterAnnotations_EmptyMap(t *testing.T) {
	t.Parallel()

	exclude := BuildAnnotationFilter([]string{"foo"})
	got := filterAnnotations(map[string]string{}, exclude)

	if got == nil {
		t.Fatal("expected non-nil empty map")
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}
