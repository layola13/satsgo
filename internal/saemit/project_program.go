// Program scaffolding: linked multi-file projects + incremental cache.
package saemit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ProgramScaffoldOptions controls linked project generation.
type ProgramScaffoldOptions struct {
	// ModuleName is the sa.mod package name.
	ModuleName string
	// NpmDeps surfaces package.json runtime deps as commented require
	// lines in sa.mod plus a subset-report section (see npm_deps.go).
	NpmDeps []NpmDep
	// Entry is the program entry path key (e.g. "main.ts").
	Entry string
	// Files maps a slash path to TS source text.
	Files map[string]string
	// UseCache enables the content-hash incremental cache.
	UseCache bool
}

// programCache is the .tsgo-sa-cache.json shape: reachable-file hashes plus
// the last linked result (merged + per-file .sai, refusal flag).
type programCache struct {
	Hashes  map[string]string `json:"hashes"`
	SAI     string            `json:"sai"`
	PerFile map[string]string `json:"perFile"`
	Refused bool              `json:"refused"`
	Report  string            `json:"report"`
}

// ScaffoldProgram lowers a linked program and writes the sci/sa project:
// per-file src/<path>.{ts,sai} (debug), merged src/main.sai (link artifact),
// sa.mod, subset-report.txt (per-file + link section), build.sh, README.md.
func ScaffoldProgram(outDir string, opts ProgramScaffoldOptions) (ProgramResult, error) {
	if opts.ModuleName == "" {
		opts.ModuleName = "tsgo_sa_app"
	}
	if err := os.MkdirAll(filepath.Join(outDir, "src"), 0o755); err != nil {
		return ProgramResult{}, err
	}
	res := LowerProgram(opts.Entry, opts.Files)
	// Incremental cache: reachable-file hashes decide reuse.
	cachePath := filepath.Join(outDir, ".tsgo-sa-cache.json")
	hashes := map[string]string{}
	reach := append([]string{}, res.Files...)
	sort.Strings(reach)
	for _, p := range reach {
		h := sha256.Sum256([]byte(opts.Files[p]))
		hashes[p] = hex.EncodeToString(h[:])
	}
	if opts.UseCache && !res.Refused {
		if data, err := os.ReadFile(cachePath); err == nil {
			var c programCache
			if json.Unmarshal(data, &c) == nil && equalHashes(c.Hashes, hashes) {
				// Cache hit: restore linked artifacts without relowering.
				res.SAI = c.SAI
				for p, sai := range c.PerFile {
					if fr, ok := res.PerFile[p]; ok {
						fr.SAI = sai
						res.PerFile[p] = fr
					}
				}
				writeProgramArtifacts(outDir, opts, res, c.Report)
				return res, nil
			}
		}
	}
	report := programReport(opts, res)
	if opts.UseCache && !res.Refused {
		per := map[string]string{}
		for p, fr := range res.PerFile {
			per[p] = fr.SAI
		}
		c := programCache{Hashes: hashes, SAI: res.SAI, PerFile: per, Refused: res.Refused, Report: report}
		if data, err := json.Marshal(c); err == nil {
			_ = os.WriteFile(cachePath, data, 0o644)
		}
	}
	writeProgramArtifacts(outDir, opts, res, report)
	return res, nil
}

func equalHashes(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func programReport(opts ProgramScaffoldOptions, res ProgramResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "== program %s: refused=%v ==\n", opts.Entry, res.Refused)
	fmt.Fprintf(&b, "files: %s\n", strings.Join(res.Files, ", "))
	for _, d := range res.Diagnostics {
		fmt.Fprintf(&b, "%s\n", d)
	}
	// Per-package aggregation: third-party specifiers are Phase-3
	// candidates (whitelist or SA-native rewrite), not file-level noise.
	if len(res.Unresolved) > 0 {
		fmt.Fprintf(&b, "== unresolved third-party deps (%d) ==\n", len(res.Unresolved))
		for _, u := range res.Unresolved {
			fmt.Fprintf(&b, "package %s: no SA backend yet (see todo/03_npm.md)\n", u)
		}
	}
	// package.json runtime deps (imported or not; versions verbatim).
	if len(opts.NpmDeps) > 0 {
		fmt.Fprintf(&b, "== package.json dependencies (%d) ==\n", len(opts.NpmDeps))
		for _, d := range opts.NpmDeps {
			fmt.Fprintf(&b, "npm %s@%s: record in sa.mod require after sa pkg resolution\n", d.Name, d.Version)
		}
	}
	return b.String()
}

func writeProgramArtifacts(outDir string, opts ProgramScaffoldOptions, res ProgramResult, report string) {
	for p, text := range opts.Files {
		// Sources land under src/ preserving the tree (a leading src/
		// is not duplicated).
		rel := strings.TrimPrefix(filepath.FromSlash(p), "src"+string(filepath.Separator))
		dst := filepath.Join(outDir, "src", rel)
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		_ = os.WriteFile(dst, []byte(text), 0o644)
	}
	for p, fr := range res.PerFile {
		rel := strings.TrimPrefix(filepath.FromSlash(p), "src"+string(filepath.Separator))
		dst := filepath.Join(outDir, "src", rel+".sai")
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		_ = os.WriteFile(dst, []byte(fr.SAI), 0o644)
	}
	_ = os.WriteFile(filepath.Join(outDir, "src", "main.sai"), []byte(res.SAI), 0o644)
	_ = os.WriteFile(filepath.Join(outDir, "subset-report.txt"), []byte(report), 0o644)
	files := map[string]string{
		"sa.mod":    fmt.Sprintf("package \"%s\"\n", opts.ModuleName) + npmRequireComments(opts.NpmDeps),
		"README.md": readmeText(opts.ModuleName, "src/main.sai"),
		"build.sh":  buildScriptText("src/main.sai"),
	}
	for rel, content := range files {
		_ = os.WriteFile(filepath.Join(outDir, rel), []byte(content), 0o644)
	}
}
