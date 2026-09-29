// Command tsgo-sa lowers TypeScript to sci/sa projects (tsgo → ts → sa).
//
// Usage:
//
//	tsgo-sa [--out <dir>] [--mod <name>] <file.ts> [...]
//	tsgo-sa build <dir> [--out <dir>] [--entry <path>] [--mod <name>] [--no-cache]
//
// Single-file mode parses each input, gates it through the SA-lowerable
// subset, lowers to SA-ASM, and scaffolds a sci/sa project.
// Build mode links a directory program: entry plus reachable relative
// imports lower with per-file symbol prefixes and merge into src/main.sai.
//
// Exit status: 0 when every file lowered without refusal; 1 when any file
// refused (the project is still written, but partial .sai files must NOT be
// assembled — see subset-report.txt).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/typescript-go/internal/saemit"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "build" {
		runBuild(os.Args[2:])
		return
	}
	runFiles(os.Args[1:])
}

func runFiles(args []string) {
	fs := flag.NewFlagSet("tsgo-sa", flag.ExitOnError)
	out := fs.String("out", "", "output project directory (default: <first-input-base>_sa)")
	mod := fs.String("mod", "", "sa.mod package name (default: derived from output dir)")
	_ = fs.Parse(args)
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: tsgo-sa [--out <dir>] [--mod <name>] <file.ts> [...]")
		os.Exit(2)
	}
	first := fs.Arg(0)
	outDir := *out
	if outDir == "" {
		base := strings.TrimSuffix(filepath.Base(first), filepath.Ext(first))
		outDir = base + "_sa"
	}
	modName := *mod
	if modName == "" {
		modName = strings.ReplaceAll(filepath.Base(outDir), "-", "_")
	}
	sources := map[string]string{}
	for _, f := range fs.Args() {
		text, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: read %s: %v\n", f, err)
			os.Exit(2)
		}
		name := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
		sources[name] = string(text)
	}
	results, err := saemit.Scaffold(outDir, saemit.ScaffoldOptions{ModuleName: modName, Sources: sources})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: scaffold: %v\n", err)
		os.Exit(2)
	}
	refused := false
	for name, res := range results {
		for _, d := range res.Diagnostics {
			fmt.Fprintf(os.Stderr, "%s: %s\n", name, d.Error())
		}
		if res.Refused {
			refused = true
		}
	}
	fmt.Printf("wrote SA project to %s\n", outDir)
	if refused {
		fmt.Fprintln(os.Stderr, "refused: resolve subset-report.txt before running sh build.sh")
		os.Exit(1)
	}
}

// runBuild links a directory program: tsgo-sa build <dir> [--out ...].
// Flags may appear before or after <dir> (pre-scanned: the std flag set
// stops at the first positional).
func runBuild(args []string) {
	rest, outVal, entryVal, modVal, noCacheDef := prescanBuildFlags(args)
	fs := flag.NewFlagSet("tsgo-sa build", flag.ExitOnError)
	out := fs.String("out", outVal, "output project directory (default: <dir>)")
	entry := fs.String("entry", entryVal, "entry path relative to <dir> (default: auto-detect)")
	mod := fs.String("mod", modVal, "sa.mod package name (default: package.json name or dir base)")
	noCacheF := fs.Bool("no-cache", noCacheDef, "disable the content-hash incremental cache")
	_ = fs.Parse(rest)
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: tsgo-sa build <dir> [--out <dir>] [--entry <path>] [--mod <name>] [--no-cache]")
		os.Exit(2)
	}
	dir := fs.Arg(0)
	outDir := *out
	if outDir == "" {
		outDir = dir
	}
	// Collect .ts sources (skip node_modules and previous outputs).
	files := map[string]string{}
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := filepath.Base(p)
			if base == "node_modules" || base == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".ts") || strings.HasSuffix(p, ".d.ts") {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		// Skip generated outputs and caches.
		if strings.HasSuffix(rel, ".sai") || filepath.Base(rel) == ".tsgo-sa-cache.json" {
			return nil
		}
		text, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		files[filepath.ToSlash(rel)] = string(text)
		return nil
	})
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "error: no .ts sources under %s\n", dir)
		os.Exit(2)
	}
	entryRel := *entry
	if entryRel == "" {
		entryRel = detectEntry(dir, files)
	}
	if _, ok := files[entryRel]; !ok {
		fmt.Fprintf(os.Stderr, "error: entry %s not found under %s\n", entryRel, dir)
		os.Exit(2)
	}
	modName := *mod
	if modName == "" {
		modName = detectModName(dir)
	}
	res, err := saemit.ScaffoldProgram(outDir, saemit.ProgramScaffoldOptions{
		ModuleName: modName,
		Entry:      entryRel,
		Files:      files,
		UseCache:   !*noCacheF,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: scaffold: %v\n", err)
		os.Exit(2)
	}
	for _, d := range res.Diagnostics {
		fmt.Fprintln(os.Stderr, "diag:", d)
	}
	fmt.Printf("wrote linked SA project to %s (entry %s, %d files)\n", outDir, entryRel, len(res.Files))
	if res.Refused {
		fmt.Fprintln(os.Stderr, "refused: resolve subset-report.txt before running sh build.sh")
		os.Exit(1)
	}
}

// prescanBuildFlags hoists --out/--entry/--mod/--no-cache wherever they
// appear so positional <dir> never cuts std flag parsing short.
func prescanBuildFlags(args []string) (rest []string, out, entry, mod string, noCache bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		take := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--out" && i+1 < len(args):
			out = take()
		case strings.HasPrefix(a, "--out="):
			out = strings.TrimPrefix(a, "--out=")
		case a == "--entry" && i+1 < len(args):
			entry = take()
		case strings.HasPrefix(a, "--entry="):
			entry = strings.TrimPrefix(a, "--entry=")
		case a == "--mod" && i+1 < len(args):
			mod = take()
		case strings.HasPrefix(a, "--mod="):
			mod = strings.TrimPrefix(a, "--mod=")
		case a == "--no-cache":
			noCache = true
		default:
			rest = append(rest, a)
		}
	}
	return rest, out, entry, mod, noCache
}

// detectEntry resolves the program entry: explicit package.json "saEntry",
// tsconfig "files"[0], then src/main.ts, main.ts, src/index.ts, index.ts.
func detectEntry(dir string, files map[string]string) string {
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var pkg map[string]any
		if json.Unmarshal(data, &pkg) == nil {
			if se, ok := pkg["saEntry"].(string); ok {
				if _, ok := files[se]; ok {
					return se
				}
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "tsconfig.json")); err == nil {
		var ts map[string]any
		if json.Unmarshal(data, &ts) == nil {
			if fl, ok := ts["files"].([]any); ok && len(fl) > 0 {
				if f0, ok := fl[0].(string); ok {
					if _, ok := files[f0]; ok {
						return f0
					}
				}
			}
		}
	}
	for _, c := range []string{"src/main.ts", "main.ts", "src/index.ts", "index.ts"} {
		if _, ok := files[c]; ok {
			return c
		}
	}
	// Fallback: lexicographically first file (deterministic).
	names := []string{}
	for n := range files {
		names = append(names, n)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return names[0]
}

// detectModName derives the package name from package.json or the dir base.
func detectModName(dir string) string {
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var pkg map[string]any
		if json.Unmarshal(data, &pkg) == nil {
			if name, ok := pkg["name"].(string); ok && name != "" {
				return strings.ReplaceAll(name, "-", "_")
			}
		}
	}
	return strings.ReplaceAll(filepath.Base(dir), "-", "_")
}
