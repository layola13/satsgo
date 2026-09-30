// package.json dependency mapping: npm names/versions surface in the
// scaffolded sa.mod (commented require lines) and the subset report
// (Unresolved-style section). sa.mod require entries mandate a sha256
// that npm packages do not carry, so nothing uncommented is emitted:
// resolution via `sa pkg` stays a deliberate manual step (loud, visible).
package saemit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// NpmDep is one package.json runtime dependency (devDependencies never
// ship, so they are excluded; versions stay verbatim, no semver solving).
type NpmDep struct {
	Name    string
	Version string
}

// ReadNpmDeps returns the sorted runtime dependencies of dir/package.json
// (nil when absent or unparsable; never refuses).
func ReadNpmDeps(dir string) []NpmDep {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil
	}
	var pkg map[string]any
	if json.Unmarshal(data, &pkg) == nil {
		deps, _ := pkg["dependencies"].(map[string]any)
		if len(deps) == 0 {
			return nil
		}
		out := make([]NpmDep, 0, len(deps))
		for name, v := range deps {
			ver, _ := v.(string)
			out = append(out, NpmDep{Name: name, Version: ver})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	return nil
}

// npmRequireComments renders the sa.mod comment block for npm deps.
func npmRequireComments(deps []NpmDep) string {
	if len(deps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# npm dependencies (no sa hash yet; resolve via sa pkg before uncommenting):\n")
	for _, d := range deps {
		b.WriteString("# require npm:" + d.Name + "@" + d.Version + " <sha256 pending>\n")
	}
	return b.String()
}
