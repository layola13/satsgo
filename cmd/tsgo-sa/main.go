// Command tsgo-sa lowers TypeScript to sci/sa projects (tsgo → ts → sa).
//
// Usage:
//
//	tsgo-sa [--out <dir>] [--mod <name>] <file.ts> [...]
//
// Each input is parsed with the production tsgo frontend, gated through the
// SA-lowerable subset, lowered to SA-ASM, and scaffolded as a sci/sa project
// (sa.mod + src/*.ts + src/*.sai + subset-report.txt + build.sh).
//
// Exit status: 0 when every file lowered without refusal; 1 when any file
// refused (the project is still written, but partial .sai files must NOT be
// assembled — see subset-report.txt).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/typescript-go/internal/saemit"
)

func main() {
	out := flag.String("out", "", "output project directory (default: <first-input-base>_sa)")
	mod := flag.String("mod", "", "sa.mod package name (default: derived from output dir)")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: tsgo-sa [--out <dir>] [--mod <name>] <file.ts> [...]")
		os.Exit(2)
	}
	first := flag.Arg(0)
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
	for _, f := range flag.Args() {
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
