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

// fileExports is one linked file's export surface: direct definitions,
// default, re-export edges and the resolved qualified names.
type fileExports struct {
	exports map[string]bool
	rets    map[string]saType
	// defLocal is the local name of the default export ("" if none).
	defLocal string
	// defNS maps member -> local for `export default {a, b: c}` object
	// defaults (namespace-object shape; methods/spreads refuse loudly).
	defNS map[string]string
	// defNSFrom holds a file key whose export surface becomes the default
	// members (`export default nsAlias` passthrough; expanded at links).
	defNSFrom string
	// reexp maps a locally-exported name to "fileKey.remote" for
	// `export {x} from` forms; starFrom lists `export * from` targets.
	reexp    map[string]string
	starFrom []string
	// reexpQualified maps every export to its linked @name (filled by
	// resolveReExports).
	reexpQualified map[string]string
}

// dtsSig is one .d.ts signature override for an unannotated .js def.
type dtsSig struct {
	ret   saType
	arity int
	defs  []bool
}

// fileLink is one linked file's link-time environment (see emitter.link).
type fileLink struct {
	key      string
	prefix   string
	resolved map[string]*modResolution
	// valueUsed holds this file's value-position identifier uses; the
	// per-file lowerer consults it to erase imports with no runtime use.
	valueUsed map[string]bool
}

// modResolution binds one module specifier to its target file exports.
type modResolution struct {
	key      string
	prefix   string
	exports  map[string]bool
	rets     map[string]saType
	// qualified maps an export name to its linked @name (direct defs and
	// resolved re-exports alike; default imports use defQualified).
	qualified    map[string]string
	defLocal     string
	defQualified string
	// defNS maps default-object member -> local (see fileExports.defNS).
	defNS map[string]string
}

// ProgramResult is the linked program outcome.
type ProgramResult struct {
	SAI         string
	Files       []string
	PerFile     map[string]Result
	Refused     bool
	Diagnostics []string
	// Unresolved lists bare third-party specifiers met during linking
	// (Phase-3 candidates; also surfaced per-package in the report).
	Unresolved []string
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
	// Namespace-import aliases per file (`import * as A`; feeds default
	// passthrough) and value-position identifier uses per file.
	nsAliasOf := map[string]map[string]string{}
	usedOf := map[string]map[string]bool{}
	for p, sf := range parsed {
		usedOf[p] = valueUsedNames(sf.AsSourceFile().Statements.Nodes)
	}
	// Import graph over relative specifiers.
	graph := map[string][]string{}
	specOf := map[string]map[string]string{} // file -> spec -> target
	unresolved := map[string]bool{}
	isBuiltinMod := func(spec string) bool {
		switch spec {
		case "fs", "net", "path", "os",
			"node:fs", "node:net", "node:path", "node:os",
			"node:process", "node:buffer":
			return true
		}
		return strings.HasSuffix(spec, ".wasm") || strings.HasSuffix(spec, ".wit")
	}
	for p, sf := range parsed {
		for _, st := range sf.AsSourceFile().Statements.Nodes {
			if st.Kind != ast.KindImportDeclaration && st.Kind != ast.KindExportDeclaration {
				continue
			}
			// Type-only imports/exports erase at compile time and carry
			// no runtime edge (`import type { T } from "./y"` must not
			// fuse cycles; planck Shape->Body is exactly this shape).
			// Bare side-effect imports have no clause and stay linked.
			if st.Kind == ast.KindImportDeclaration {
				if cl := st.AsImportDeclaration().ImportClause; cl != nil && cl.IsTypeOnly() {
					continue
				}
			}
			if st.Kind == ast.KindExportDeclaration && st.IsTypeOnly() {
				continue
			}
			spec := moduleSpecifierOf(st)
			if spec == "" {
				continue
			}
			if !strings.HasPrefix(spec, ".") {
				if !isBuiltinMod(spec) {
					unresolved[spec] = true
				}
				continue
			}
			tgt := resolveRelative(p, spec, files)
			if tgt == "" {
				continue
			}
			// Namespace-import aliases feed `export default ns` passthrough
			// (link_nsobject module); recorded before usage erasure so a
			// re-exported-only alias still resolves.
			if st.Kind == ast.KindImportDeclaration {
				recordNsAlias(st, tgt, nsAliasOf, p)
			}
			// Usage-based erasure: a relative import with no value-position
			// use carries no runtime edge (esbuild importsNotUsedAsValues
			// semantics; planck Shape<->Distance shape). Bare third-party
			// specifiers keep their unresolved marks above regardless.
			if st.Kind == ast.KindImportDeclaration && !importDeclValueEdge(st, usedOf[p]) {
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
		base := p
		base = strings.TrimSuffix(base, ".ts")
		base = strings.TrimSuffix(base, ".js")
		base = strings.TrimSuffix(base, ".d.ts")
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
	expOf := map[string]*fileExports{}
	globalDefaults := map[string]map[string][]bool{}
	// Pair co-located x.d.ts with x.js (signatures for unannotated bodies).
	dtsFor := map[string]string{}
	for p := range files {
		if strings.HasSuffix(p, ".js") {
			dts := strings.TrimSuffix(p, ".js") + ".d.ts"
			if _, ok := files[dts]; ok {
				dtsFor[p] = dts
			}
		}
	}
	dtsSigs := map[string]map[string]dtsSig{}
	for js, dts := range dtsFor {
		m := map[string]dtsSig{}
		for _, st := range parsed[dts].AsSourceFile().Statements.Nodes {
			if st.Kind != ast.KindFunctionDeclaration || st.Name() == nil ||
				st.Name().Kind != ast.KindIdentifier {
				continue
			}
			name := st.Name().Text()
			ret := tI32
			if fd := st.AsFunctionDeclaration(); fd.Type != nil {
				ret = annotationType(fd.Type)
				if ret == tUnknown {
					ret = tI32
				}
			}
			params := st.Parameters()
			defs := make([]bool, len(params))
			for i, pm := range params {
				if pd := pm.AsParameterDeclaration(); pd.Initializer != nil || pd.QuestionToken != nil {
					defs[i] = true
				}
			}
			m[name] = dtsSig{ret: ret, arity: len(params), defs: defs}
		}
		dtsSigs[js] = m
	}
	sharedLayouts := map[string]*layout{}
	sharedClassDefs := map[string]*classDef{}
	sharedStaticDefs := map[string]*classDef{}
	sharedEnums := map[string]map[string]int64{}
	globalRets := map[string]map[string]saType{} // file -> name -> ret
	globalArity := map[string]map[string]int{}
	globalRest := map[string]map[string]bool{}
	for _, p := range reachable {
		expOf[p] = &fileExports{exports: map[string]bool{}, rets: map[string]saType{}, reexp: map[string]string{}}
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
					// Anonymous `export default function`: needs a
					// synthesized symbol (loud gap for now).
					if hasDefaultModifier(st) {
						res.Refused = true
						res.Diagnostics = append(res.Diagnostics, fmt.Sprintf("%s: anonymous default export needs a name", p))
					}
					continue
				}
				name := st.Name().Text()
				if hasExportModifier(st) {
					expOf[p].exports[name] = true
				}
				if hasDefaultModifier(st) {
					expOf[p].defLocal = name
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
				fromForm := collectReExport(st, p, files, expOf[p])
				if !fromForm {
					if refused := collectExportList(st, expOf[p].exports); refused != "" {
						res.Refused = true
						res.Diagnostics = append(res.Diagnostics, fmt.Sprintf("%s: %s", p, refused))
					}
					if def := collectDefaultExport(st); def != "" {
						expOf[p].defLocal = def
					}
				}
			case ast.KindExportAssignment:
				// `export default foo;`: the identifier names the default.
				ea := st.AsExportAssignment()
				if ea.IsExportEquals {
					res.Refused = true
					res.Diagnostics = append(res.Diagnostics, fmt.Sprintf("%s: export = is not lowerable (use ES exports)", p))
				} else if ea.Expression != nil && ea.Expression.Kind == ast.KindIdentifier {
					// Namespace-alias passthrough (`export default ns`;
					// link_nsobject module) wins over plain default.
					if !adoptDefNSAlias(expOf[p], ea.Expression.Text(), nsAliasOf, p) {
						expOf[p].defLocal = ea.Expression.Text()
					}
				} else if ea.Expression != nil && ea.Expression.Kind == ast.KindObjectLiteralExpression {
					// `export default {a, b: c}`: namespace-object default;
					// members must be plain local names (methods/spreads
					// refuse loudly; see collectDefObject).
					if msg := collectDefObject(ea.Expression, expOf[p]); msg != "" {
						res.Refused = true
						res.Diagnostics = append(res.Diagnostics, fmt.Sprintf("%s: %s", p, msg))
					}
				} else {
					res.Refused = true
					res.Diagnostics = append(res.Diagnostics, fmt.Sprintf("%s: non-identifier default export is not lowerable", p))
				}
			}
		}
		for k, l := range scratch.layouts {
			sharedLayouts[k] = l
		}
		for k, c := range scratch.classDefs {
			sharedClassDefs[k] = c
		}
		for k, c := range scratch.staticDefs {
			sharedStaticDefs[k] = c
		}
		for k, v := range scratch.enums {
			sharedEnums[k] = v
		}
	}
	// Pre-pass refusals (export * / default) abort before lowering.
	if res.Refused {
		return res
	}
	// .d.ts overrides: unannotated .js bodies take signatures (ret,
	// arity, optionals-as-defaults) from their co-located declarations.
	for js, sigs := range dtsSigs {
		for name, sg := range sigs {
			if _, ok := globalRets[js][name]; ok {
				globalRets[js][name] = sg.ret
				globalArity[js][name] = sg.arity
				globalDefaults[js][name] = sg.defs
			}
		}
		if ex, ok := expOf[js]; ok {
			for name, sg := range sigs {
				ex.rets[name] = sg.ret
			}
		}
	}
	// Re-export resolution: every export (direct, re-exported, star)
	// maps to its defining qualified name, with rets/arity/defaults
	// propagated and cycles refused.
	qualDiag := resolveReExports(reachable, expOf, prefixOf, globalRets, globalArity, globalDefaults)
	if len(qualDiag) > 0 {
		res.Refused = true
		res.Diagnostics = append(res.Diagnostics, qualDiag...)
		return res
	}
	// Per-file link environments (defNSFrom expands after re-exports
	// resolved, so passthrough members track renames).
	for _, p := range reachable {
		expandDefNSFrom(expOf, prefixOf, p)
	}
	links := map[string]*fileLink{}
	for _, p := range reachable {
		lk := &fileLink{key: p, prefix: prefixOf[p], resolved: map[string]*modResolution{}, valueUsed: usedOf[p]}
		for spec, tgt := range specOf[p] {
			defQ := ""
			if dl := expOf[tgt].defLocal; dl != "" {
				if q, ok := expOf[tgt].reexpQualified[dl]; ok {
					defQ = q
				} else {
					defQ = prefixOf[tgt] + dl
				}
			} else if q, ok := expOf[tgt].reexpQualified["default"]; ok {
				// `export {x as default} from` without a local default.
				defQ = q
			}
			lk.resolved[spec] = &modResolution{
				key:          tgt,
				prefix:       prefixOf[tgt],
				exports:      expOf[tgt].exports,
				rets:         expOf[tgt].rets,
				qualified:    expOf[tgt].reexpQualified,
				defLocal:     expOf[tgt].defLocal,
				defQualified: defQ,
				defNS:        expOf[tgt].defNS,
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
		// .d.ts return overrides for unannotated bodies in this file.
		e.dtsRet = map[string]saType{}
		for name, sg := range dtsSigs[p] {
			e.dtsRet[name] = sg.ret
		}
		e.layouts = sharedLayouts
		e.classDefs = sharedClassDefs
		e.staticDefs = sharedStaticDefs
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
	for spec := range unresolved {
		res.Unresolved = append(res.Unresolved, spec)
	}
	sort.Strings(res.Unresolved)
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
// (tries x.ts, x.js, extensionless, x/index.ts, x/index.js; "" when absent).
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
	cands := []string{
		join(spec) + ".ts", join(spec) + ".js", join(spec),
		join(spec) + "/index.ts", join(spec) + "/index.js",
	}
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

// resolveReExports computes per-file qualified export names, following
// re-export edges (named and star, default excluded from star per spec)
// with cycle diagnostics. Direct definitions map to prefix+name; rets,
// arity and defaults propagate to the re-exporting file.
func resolveReExports(reachable []string, expOf map[string]*fileExports, prefixOf map[string]string, rets map[string]map[string]saType, arity map[string]map[string]int, defs map[string]map[string][]bool) []string {
	var diags []string
	qual := map[string]map[string]string{}
	for _, p := range reachable {
		qual[p] = map[string]string{}
	}
	var resolve func(p, name string, stack []string) (string, bool)
	resolve = func(p, name string, stack []string) (string, bool) {
		if q, ok := qual[p][name]; ok {
			return q, true
		}
		for _, s := range stack {
			if s == p+"\x00"+name {
				chain := append(append([]string{}, stack...), p+"\x00"+name)
				diags = append(diags, "re-export cycle: "+strings.Join(chain, " -> "))
				return "", false
			}
		}
		stack = append(stack, p+"\x00"+name)
		ex := expOf[p]
		// Direct definition (function, arrow): own prefix.
		if _, ok := rets[p][name]; ok {
			if _, isReexp := ex.reexp[name]; !isReexp {
				q := prefixOf[p] + name
				qual[p][name] = q
				return q, true
			}
		}
		// Named re-export edge.
		if edge, ok := ex.reexp[name]; ok {
			parts := strings.SplitN(edge, "\x00", 2)
			if len(parts) == 2 {
				tgt, remote := parts[0], parts[1]
				if q, ok := resolve(tgt, remote, stack); ok {
					qual[p][name] = q
					if _, ok := rets[p][name]; !ok {
						if r, ok := rets[tgt][remote]; ok {
							rets[p][name] = r
						}
					}
					if _, ok := arity[p][name]; !ok {
						if a, ok := arity[tgt][remote]; ok {
							arity[p][name] = a
						}
					}
					if _, ok := defs[p][name]; !ok {
						if d, ok := defs[tgt][remote]; ok {
							defs[p][name] = d
						}
					}
					ex.rets[name] = rets[p][name]
					return q, true
				}
				return "", false
			}
		}
		// Star re-exports (first match wins, shadowing local defs never:
		// locals were handled above).
		for _, tgt := range ex.starFrom {
			if _, ok := expOf[tgt]; !ok {
				continue
			}
			// Enumerate the target's export names.
			names := map[string]bool{}
			for n := range rets[tgt] {
				names[n] = true
			}
			for n := range expOf[tgt].exports {
				names[n] = true
			}
			if _, ok := names[name]; ok {
				if q, ok := resolve(tgt, name, stack); ok {
					qual[p][name] = q
					if r, ok := rets[tgt][name]; ok {
						rets[p][name] = r
						ex.rets[name] = r
					}
					if a, ok := arity[tgt][name]; ok {
						arity[p][name] = a
					}
					if d, ok := defs[tgt][name]; ok {
						defs[p][name] = d
					}
					return q, true
				}
				return "", false
			}
		}
		return "", false
	}
	for _, p := range reachable {
		// Direct definitions first.
		for name := range rets[p] {
			if _, isReexp := expOf[p].reexp[name]; !isReexp {
				qual[p][name] = prefixOf[p] + name
			}
		}
		// Named re-exports (default included: `export {x as default} from`).
		for local := range expOf[p].reexp {
			if _, ok := qual[p][local]; !ok {
				resolve(p, local, nil)
			}
		}
		// Star enumerations for namespace imports (and the existence
		// table importers check against).
		for _, tgt := range expOf[p].starFrom {
			if _, ok := expOf[tgt]; !ok {
				continue
			}
			for n := range rets[tgt] {
				if _, ok := qual[p][n]; !ok {
					resolve(p, n, nil)
				}
				expOf[p].exports[n] = true
			}
			for n := range expOf[tgt].exports {
				if strings.HasPrefix(n, "*") {
					continue
				}
				if _, ok := qual[p][n]; !ok {
					resolve(p, n, nil)
				}
				expOf[p].exports[n] = true
			}
		}
		// Export existence for star markers is informational only.
		for _, tgt := range expOf[p].starFrom {
			delete(expOf[p].exports, "*"+tgt)
			_ = tgt
		}
	}
	// Store qualified maps for link building.
	for _, p := range reachable {
		expOf[p].reexpQualified = qual[p]
	}
	return diags
}

// hasDefaultModifier reports a `default` keyword modifier.
func hasDefaultModifier(st *ast.Node) bool {
	mods := st.Modifiers()
	if mods == nil {
		return false
	}
	for _, m := range mods.Nodes {
		if m.Kind == ast.KindDefaultKeyword {
			return true
		}
	}
	return false
}

// collectDefaultExport records `export { x as default }` ("" if absent).
func collectDefaultExport(st *ast.Node) string {
	ed := st.AsExportDeclaration()
	if ed.ModuleSpecifier != nil || ed.ExportClause == nil {
		return ""
	}
	clause := ed.ExportClause
	if clause.Kind == ast.KindNamespaceExport {
		return ""
	}
	for _, el := range clause.AsNamedExports().Elements.Nodes {
		if el.Kind != ast.KindExportSpecifier {
			continue
		}
		sp := el.AsExportSpecifier()
		if n := el.Name(); n != nil && n.Text() == "default" {
			if sp.PropertyName != nil {
				return sp.PropertyName.Text()
			}
			return "default"
		}
	}
	return ""
}

// collectReExport records `export {x} from "./m"`, `export {x as y} from`
// and `export * from` edges (true when the statement is a resolvable local
// from-form; bare/absolute/unresolvable specifiers refuse loudly).
func collectReExport(st *ast.Node, importer string, files map[string]string, exp *fileExports) bool {
	ed := st.AsExportDeclaration()
	if ed.ModuleSpecifier == nil {
		return false
	}
	spec, ok := stringLiteralText(ed.ModuleSpecifier)
	if !ok || !strings.HasPrefix(spec, ".") {
		return false
	}
	tgt := resolveRelative(importer, spec, files)
	if tgt == "" {
		return false
	}
	// Bare `export * from`: nil clause fans out the whole target.
	if ed.ExportClause == nil {
		exp.starFrom = append(exp.starFrom, tgt)
		return true
	}
	if ed.ExportClause == nil {
		return false
	}
	clause := ed.ExportClause
	if clause.Kind == ast.KindNamespaceExport {
		exp.starFrom = append(exp.starFrom, tgt)
		exp.exports["*"+tgt] = true
		return true
	}
	for _, el := range clause.AsNamedExports().Elements.Nodes {
		if el.Kind != ast.KindExportSpecifier {
			continue
		}
		sp := el.AsExportSpecifier()
		remote := ""
		if sp.PropertyName != nil {
			remote = sp.PropertyName.Text()
		}
		local := ""
		if n := el.Name(); n != nil {
			local = n.Text()
		}
		if remote == "" {
			remote = local
		}
		if local == "" {
			continue
		}
		exp.reexp[local] = tgt + "\x00" + remote
		exp.exports[local] = true
	}
	return true
}
// collectExportList records local `export { a, b }` names; from-forms and
// `export *` refuse here (from-forms resolve via collectReExport first).
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
