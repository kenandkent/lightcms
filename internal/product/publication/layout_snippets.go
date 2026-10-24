package publication

import (
	"fmt"
	"strings"
)

// ExpandLayoutSnippets expands template source, not rendered user values. The
// caller then parses/executes the complete layout exactly once. Frozen callers
// resolve exclusively from their snapshot; previews may resolve from the DB.
func ExpandLayoutSnippets(layout string, resolve func(string) (string, error)) (string, error) {
	cache := map[string]string{}
	var expand func(string, int, map[string]bool) (string, error)
	expand = func(source string, depth int, visiting map[string]bool) (string, error) {
		var first error
		out := snippetIncludeREFrozen.ReplaceAllStringFunc(source, func(marker string) string {
			if first != nil {
				return marker
			}
			name := snippetIncludeREFrozen.FindStringSubmatch(marker)[1]
			if visiting[name] || depth >= 3 {
				first = fmt.Errorf("layout snippet %q is cyclic or exceeds nesting limit", name)
				return marker
			}
			body, ok := cache[name]
			if !ok {
				var e error
				body, e = resolve(name)
				if e != nil {
					first = fmt.Errorf("layout snippet %q: %w", name, e)
					return marker
				}
				cache[name] = body
			}
			visiting[name] = true
			rendered, e := expand(body, depth+1, visiting)
			delete(visiting, name)
			if e != nil {
				first = e
				return marker
			}
			return rendered
		})
		return out, first
	}
	if !strings.Contains(layout, "[[include:") {
		return layout, nil
	}
	return expand(layout, 0, map[string]bool{})
}
