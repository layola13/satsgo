// Program linking: multi-file tsgo → SA.
//
// LowerProgram lowers an entry file plus its reachable relative imports:
// each file lowers with a symbol prefix (entry keeps `@main`), imports bind
// to qualified callees, headers merge with dedup, and cycles refuse loudly.
// Bare third-party imports are Phase 3 (see todo/03_npm.md).
package saemit

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/parser"
	"github.com/microsoft/typescript-go/internal/tspath"
)

// fileLink is one linked file's link-time environment (see emitter.link).
type fileLink struct {
	key      string
	prefix   string
	resolved map[string]*modResolution
}

// modResolution binds one module specifier to its target file exports.
type modResolution struct {
	key     string
	prefix  string
	exports map[string]bool
	rets    map[string]saType
}

// ProgramResult is the linked program outcome.
type ProgramResult struct {
	SAI         string
	Files       []string
	PerFile     map[string]Result
	Refused     bool
	Diagnostics []string
}

// LowerProgram lowers entry plus reachable relative .ts modules.
// files maps a clean slash path (e.g. "main.ts", "lib/util.ts") to text;
// entry must be one of its keys.
func LowerProgram(entry string, files map[string]string) ProgramResult {
	res := ProgramResult{PerFile: map[string]Result{}}
	entry = path.Clean(entry)
	if _, ok := files[entry]; !ok {
		res.Refused = true
		res.Diagnostics = append(res.Diagnostics, fmt.Sprintf("entry %s not in file set", entry))
		return res
	}
	// Parse every file (fallback trees; the checker's trees win when the
	// type context builds so node identity matches type queries).
	parsed := map[string]*ast.SourceFile{}
	var parseErrs []string
	for p, text := range files {
		abs := "/" + strings.TrimPrefix(p, "/")
		opts := ast.SourceFileParseOptions{FileName: abs, Path: tspath.ToPath(abs, "/", true)}
		sf := parser.ParseSourceFile(opts, text, core.ScriptKindTS)
		parsed[path.Clean(p)] = sf
		_ = parseErrs
	}
	// Import graph over relative specifiers.
	graph := map[string][]string{}
	specOf := map[string]map[string]string{} // file -> spec -> target
	for p, sf := range parsed {
		for _, st := range sf.AsSourceFile().Statements.Nodes {
			if st.Kind != ast.KindImportDeclaration && st.Kind != ast.KindExportDeclaration {
				continue
			}
			spec := moduleSpecifierOf(st)
			if spec == "" || !strings.HasPrefix(spec, ".") {
				continue
			}
			tgt := resolveRelative(p, spec, files)
			if tgt == "" {
				continue
			}
			graph[p] = append(graph[p], tgt)
			if specOf[p] == nil {
				specOf[p] = map[string]string{}
			}
			specOf[p][spec] = tgt
		}
	}
	// Reachability from entry + cycle check (DFS).
	reachable := []string{}
	visited := map[string]bool{}
	onStack := map[string]bool{}
	var stack []string
	var cycle []string
	var dfs func(p string)
	dfs = func(p string) {
		visited[p] = true
		onStack[p] = true
		stack = append(stack, p)
		for _, q := range graph[p] {
			if cycle != nil {
				break
			}
			if onStack[q] {
				i := 0
				for i < len(stack) && stack[i] != q {
					i++
				}
				cycle = append(append([]string{}, stack[i:]...), q)
				break
			}
			if !visited[q] {
				dfs(q)
			}
		}
		stack = stack[:len(stack)-1]
		onStack[p] = false
		reachable = append(reachable, p)
	}
	dfs(entry)
	if cycle != nil {
		res.Refused = true
		res.Diagnostics = append(res.Diagnostics, fmt.Sprintf("import cycle: %s", strings.Join(cycle, " -> ")))
		return res
	}
	// Prefixes: entry keeps "", others sanitize the path.
	prefixOf := map[string]string{}
	for _, p := range reachable {
		if p == entry {
			prefixOf[p] = ""
			continue
		}
		base := strings.TrimSuffix(p, ".ts")
		var b strings.Builder
		for _, r := range base {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				b.WriteRune(r)
			} else {
				b.WriteRune('_')
			}
		}
		prefixOf[p] = b.String() + "__"
	}
	// Pre-pass: exports + shared type environment + global function table.
	type fileExports struct {
		exports map[string]bool
		rets    map[string]saType
	}
	expOf := map[string]*fileExports{}
	globalDefaults := map[string]map[string][]bool{}
	sharedLayouts := map[string]*layout{}
	sharedClassDefs := map[string]*classDef{}
	sharedEnums := map[string]map[string]int64{}
	globalRets := map[string]map[string]saType{} // file -> name -> ret
	globalArity := map[string]map[string]int{}
	globalRest := map[string]map[string]bool{}
	for _, p := range reachable {
		expOf[p] = &fileExports{exports: map[string]bool{}, rets: map[string]saType{}}
		globalRets[p] = map[string]saType{}
		globalArity[p] = map[string]int{}
		globalRest[p] = map[string]bool{}
		globalDefaults[p] = map[string][]bool{}
		text := files[p]
		scratch := &emitter{file: p, src: text, lines: lineOffsets(text)}
		for _, st := range parsed[p].AsSourceFile().Statements.Nodes {
			switch st.Kind {
			case ast.KindFunctionDeclaration:
				if st.Name() == nil || st.Name().Kind != ast.KindIdentifier {
					continue
				}
				name := st.Name().Text()
				if hasExportModifier(st) {
					expOf[p].exports[name] = true
				}
				ret := tVoid
				if fd := st.AsFunctionDeclaration(); fd.Type != nil {
					ret = annotationType(fd.Type)
					if ret == tUnknown {
						ret = tI32
					}
				}
				expOf[p].rets[name] = ret
				globalRets[p][name] = ret
				params := st.Parameters()
				globalArity[p][name] = len(params)
				defs := make([]bool, len(params))
				for i, pm := range params {
					if pd := pm.AsParameterDeclaration(); pd.Initializer != nil {
						defs[i] = true
					}
				}
				globalDefaults[p][name] = defs
				if len(params) > 0 {
					if pd := params[len(params)-1].AsParameterDeclaration(); pd.DotDotDotToken != nil {
						globalRest[p][name] = true
					}
				}
			case ast.KindVariableStatement:
				dl := st.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
				for _, d := range dl.Declarations.Nodes {
					if d.Initializer() == nil || d.Initializer().Kind != ast.KindArrowFunction {
						continue
					}
					name, ok := bindingNameText(d)
					if !ok {
						continue
					}
					if hasExportModifier(st) {
						expOf[p].exports[name] = true
					}
					expOf[p].rets[name] = tI32
					globalRets[p][name] = tI32
					globalArity[p][name] = len(d.Initializer().Parameters())
				}
			case ast.KindInterfaceDeclaration, ast.KindTypeAliasDeclaration, ast.KindEnumDeclaration:
				scratch.lowerTypeDecl(st)
			case ast.KindClassDeclaration:
				scratch.recordClass(st)
				if st.Name() != nil && st.Name().Kind == ast.KindIdentifier && hasExportModifier(st) {
					expOf[p].exports[st.Name().Text()] = true
				}
			case ast.KindExportDeclaration:
				if refused := collectExportList(st, expOf[p].exports); refused != "" {
					res.Refused = true
					res.Diagnostics = append(res.Diagnostics, fmt.Sprintf("%s: %s", p, refused))
				}
			}
		}
		for k, l := range scratch.layouts {
			sharedLayouts[k] = l
		}
		for k, c := range scratch.classDefs {
			sharedClassDefs[k] = c
		}
		for k, v := range scratch.enums {
			sharedEnums[k] = v
		}
	}
	// Pre-pass refusals (export * / default) abort before lowering.
	if res.Refused {
		return res
	}
	// Per-file link environments.
	links := map[string]*fileLink{}
	for _, p := range reachable {
		lk := &fileLink{key: p, prefix: prefixOf[p], resolved: map[string]*modResolution{}}
		for spec, tgt := range specOf[p] {
			lk.resolved[spec] = &modResolution{
				key:     tgt,
				prefix:  prefixOf[tgt],
				exports: expOf[tgt].exports,
				rets:    expOf[tgt].rets,
			}
		}
		links[p] = lk
	}
	// Lower in dependency order (leaves first; reachable is post-order).
	order := append([]string{}, reachable...)
	res.Files = order
	// Shared binder/checker over the reachable set (nil-safe fallback).
	tcx := newTypeCtx(func() map[string]string {
		m := map[string]string{}
		for _, p := range reachable {
			m[p] = files[p]
		}
		return m
	}())
	if tcx != nil {
		defer tcx.close()
		// Prefer the checker's trees for node-identity type queries.
		for _, p := range reachable {
			if psf := tcx.prog.GetSourceFile("/" + strings.TrimPrefix(p, "/")); psf != nil {
				parsed[p] = psf
			}
		}
	}
	// Every linked top-level name for the "import it first" diagnostic.
	linkExports := map[string]string{}
	for _, p := range reachable {
		for name := range globalRets[p] {
			if _, ok := linkExports[name]; !ok {
				linkExports[name] = p
			}
		}
	}
	mergedImports := []string{}
	seenImport := map[string]bool{}
	bodies := []string{}
	for _, p := range order {
		text := files[p]
		e := &emitter{file: p, src: text, lines: lineOffsets(text)}
		e.prefix = prefixOf[p]
		e.link = links[p]
		e.linkExports = linkExports
		e.tcx = tcx
		e.layouts = sharedLayouts
		e.classDefs = sharedClassDefs
		e.enums = sharedEnums
		e.funcSigs = map[string]saType{}
		e.funcParams = map[string]int{}
		e.funcHasRest = map[string]bool{}
		e.localDefs = map[string]bool{}
		// Seed signatures: own file unprefixed + qualified names for
		// spread-arity lookups of imported callees.
		for name, ret := range globalRets[p] {
			e.funcSigs[name] = ret
			e.localDefs[name] = true
		}
		for name, n := range globalArity[p] {
			e.funcParams[name] = n
		}
		for name := range globalRest[p] {
			e.funcHasRest[name] = true
		}
		if e.funcDefaults == nil {
			e.funcDefaults = map[string][]bool{}
		}
		for name, defs := range globalDefaults[p] {
			e.funcDefaults[name] = defs
		}
		for spec, r := range links[p].resolved {
			_ = spec
			for name, ret := range r.rets {
				q := r.prefix + name
				e.funcSigs[q] = ret
				if a, ok := globalArity[r.key][name]; ok {
					e.funcParams[q] = a
				}
				if globalRest[r.key][name] {
					e.funcHasRest[q] = true
				}
				if defs, ok := globalDefaults[r.key][name]; ok {
					e.funcDefaults[q] = defs
				}
			}
		}
		e.lowerSourceFile(parsed[p])
		fr := Result{SAI: e.finish(), SubsetTS: text, Diagnostics: e.diags, Refused: e.refused}
		res.PerFile[p] = fr
		if e.refused {
			res.Refused = true
		}
		for _, d := range e.diags {
			res.Diagnostics = append(res.Diagnostics, fmt.Sprintf("%s: %s", p, d.Error()))
		}
		// Split header imports from body for the merge.
		for _, line := range strings.Split(fr.SAI, "\n") {
			t := strings.TrimSpace(line)
			if strings.HasPrefix(t, "@import ") || strings.HasPrefix(t, "@extern ") {
				if !seenImport[t] {
					seenImport[t] = true
					mergedImports = append(mergedImports, line)
				}
				continue
			}
			if strings.HasPrefix(t, "@const ") {
				if !seenImport[t] {
					seenImport[t] = true
					mergedImports = append(mergedImports, line)
				}
				continue
			}
			bodies = append(bodies, line)
		}
	}
	// @const collision check: same name, different lines refuse.
	constSeen := map[string]string{}
	for _, line := range mergedImports {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "@const ") {
			continue
		}
		fields := strings.Fields(t)
		if len(fields) < 2 {
			continue
		}
		if prev, ok := constSeen[fields[1]]; ok && prev != t {
			res.Refused = true
			res.Diagnostics = append(res.Diagnostics, fmt.Sprintf("@const %s collides across files", fields[1]))
		}
		constSeen[fields[1]] = t
	}
	var out strings.Builder
	for _, line := range mergedImports {
		out.WriteString(line)
		out.WriteString("\n")
	}
	out.WriteString(strings.Join(bodies, "\n"))
	res.SAI = out.String()
	sort.Strings(res.Files)
	return res
}

// moduleSpecifierOf extracts the literal module string of an import or a
// re-export declaration ("" when absent/dynamic).
func moduleSpecifierOf(st *ast.Node) string {
	switch st.Kind {
	case ast.KindImportDeclaration:
		if s, ok := stringLiteralText(st.AsImportDeclaration().ModuleSpecifier); ok {
			return s
		}
	case ast.KindExportDeclaration:
		ed := st.AsExportDeclaration()
		if ed.ModuleSpecifier != nil {
			if s, ok := stringLiteralText(ed.ModuleSpecifier); ok {
				return s
			}
		}
	}
	return ""
}

// resolveRelative maps "./x" against the importer's dir into the file set
// (tries x.ts, x/index.ts; "" when absent).
func resolveRelative(importer, spec string, files map[string]string) string {
	dir := path.Dir(importer)
	if dir == "." {
		dir = ""
	}
	join := func(base string) string {
		if dir == "" {
			return path.Clean(base)
		}
		return path.Clean(dir + "/" + base)
	}
	cands := []string{join(spec) + ".ts", join(spec) + "/index.ts", join(spec)}
	for _, c := range cands {
		if _, ok := files[c]; ok {
			return c
		}
	}
	return ""
}

// hasExportModifier reports an `export` keyword modifier.
func hasExportModifier(st *ast.Node) bool {
	mods := st.Modifiers()
	if mods == nil {
		return false
	}
	for _, m := range mods.Nodes {
		if m.Kind == ast.KindExportKeyword {
			return true
		}
	}
	return false
}

// collectExportList records `export { a, b }` names; `export *` and
// re-exports refuse (link subset v1), returning the refusal message.
func collectExportList(st *ast.Node, exports map[string]bool) string {
	ed := st.AsExportDeclaration()
	if ed.ModuleSpecifier != nil {
		return "export * / re-exports are not lowerable (import modules directly)"
	}
	if ed.ExportClause == nil {
		return "export * / re-exports are not lowerable (import modules directly)"
	}
	clause := ed.ExportClause
	if clause.Kind == ast.KindNamespaceExport {
		return "export * / re-exports are not lowerable (import modules directly)"
	}
	for _, el := range clause.AsNamedExports().Elements.Nodes {
		if el.Kind != ast.KindExportSpecifier {
			continue
		}
		sp := el.AsExportSpecifier()
		name := ""
		if sp.PropertyName != nil {
			name = sp.PropertyName.Text()
		} else if n := el.Name(); n != nil {
			name = n.Text()
		}
		if name != "" {
			exports[name] = true
		}
	}
	return ""
}
