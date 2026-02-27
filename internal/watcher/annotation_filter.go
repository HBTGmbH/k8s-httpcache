package watcher

import "strings"

// DefaultExcludeAnnotations is the built-in exclusion list applied unless
// overridden. Users can add more via --exclude-annotations.
var DefaultExcludeAnnotations = []string{
	"kubectl.kubernetes.io/last-applied-configuration",
}

// BuildAnnotationFilter compiles a list of exclusion patterns into a filter
// function. Each pattern is either an exact key or a prefix (trailing "*").
// Returns nil when patterns is empty.
func BuildAnnotationFilter(patterns []string) func(string) bool {
	if len(patterns) == 0 {
		return nil
	}

	exact := make(map[string]bool)
	var prefixes []string

	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			prefixes = append(prefixes, prefix)
		} else {
			exact[p] = true
		}
	}

	return func(key string) bool {
		if exact[key] {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				return true
			}
		}

		return false
	}
}

// filterAnnotations returns a filtered copy of annotations with excluded keys
// removed. Returns the original map unchanged if exclude is nil.
func filterAnnotations(annotations map[string]string, exclude func(string) bool) map[string]string {
	if exclude == nil {
		return annotations
	}
	if annotations == nil {
		return nil
	}

	filtered := make(map[string]string, len(annotations))
	for k, v := range annotations {
		if !exclude(k) {
			filtered[k] = v
		}
	}

	return filtered
}
