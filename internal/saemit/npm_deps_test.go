package saemit

// Tests for npm_deps.go (package.json dependency mapping).
import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePkg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadNpmDeps(t *testing.T) {
	dir := writePkg(t, `{"name":"x","dependencies":{"lodash-es":"^4.17.21","date-fns":"3.0.0"},"devDependencies":{"vitest":"1.0.0"}}`)
	deps := ReadNpmDeps(dir)
	if len(deps) != 2 {
		t.Fatalf("want 2 runtime deps, got %v", deps)
	}
	if deps[0].Name != "date-fns" || deps[1].Name != "lodash-es" {
		t.Errorf("want sorted names, got %v", deps)
	}
	if deps[1].Version != "^4.17.21" {
		t.Errorf("want verbatim version, got %v", deps)
	}
	if d := ReadNpmDeps(t.TempDir()); d != nil {
		t.Errorf("missing package.json must yield nil, got %v", d)
	}
	bad := writePkg(t, `not json`)
	if d := ReadNpmDeps(bad); d != nil {
		t.Errorf("invalid package.json must yield nil, got %v", d)
	}
	nodeps := writePkg(t, `{"name":"x"}`)
	if d := ReadNpmDeps(nodeps); d != nil {
		t.Errorf("no dependencies must yield nil, got %v", d)
	}
}

func TestScaffoldNpmDeps(t *testing.T) {
	files := map[string]string{
		"main.ts": "function main(): i32 { return 1; }\n",
	}
	out := t.TempDir()
	res, err := ScaffoldProgram(out, ProgramScaffoldOptions{
		ModuleName: "npmapp",
		Entry:      "main.ts",
		Files:      files,
		NpmDeps:    []NpmDep{{Name: "lodash-es", Version: "^4.17.21"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = res
	mod, err := os.ReadFile(filepath.Join(out, "sa.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mod), "# require npm:lodash-es@^4.17.21 <sha256 pending>") {
		t.Errorf("missing commented require line:\n%s", mod)
	}
	rep, err := os.ReadFile(filepath.Join(out, "subset-report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rep), "npm lodash-es@^4.17.21") {
		t.Errorf("missing report section:\n%s", rep)
	}
}
