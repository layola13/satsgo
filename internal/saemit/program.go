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
	// nsMembers maps a namespace name to member -> flat local
	// (`namespace N { function f }` records N -> f -> "N_f"; the importer
	// binds N.f per member; classes/enums/values stay out so their uses
	// keep today's loud diagnostics).
	nsMembers map[string]map[string]string
	// nsConsts maps a namespace name to member -> fold text for literal
	// consts (nsConstStr marks string consts); importers fold instead of
	// binding, mirroring top-level consts. Lets and computed inits stay
	// out so their uses keep loud diagnostics.
	nsConsts map[string]map[string]string
	nsConstStr map[string]map[string]bool
	// reexp maps a locally-exported name to "fileKey.remote" for
	// `export {x} from` forms; starFrom lists `export * from` targets.
	// starProvided marks names this file provides ONLY via star fan-out
	// (no local binding: same-file calls must import first, like named
	// re-exports; see resolveReExports + the localDefs seeding below).
	reexp        map[string]string
	starFrom     []string
	starProvided map[string]bool
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
	// consts maps an exported literal const/let name to its fold text
	// (constStr marks string consts); importers fold instead of binding.
	// Direct definitions only: re-export propagation stays loud.
	consts   map[string]string
	constStr map[string]bool
	// enums maps an exported all-integer enum name to its member table;
	// importers fold ordinals through the single-file enum machinery.
	enums map[string]map[string]int64
	// qualified maps an export name to its linked @name (direct defs and
	// resolved re-exports alike; default imports use defQualified).
	qualified    map[string]string
	defLocal     string
	defQualified string
	// defNS maps default-object member -> local (see fileExports.defNS).
	defNS map[string]string
	// nsMembers maps a namespace name to member -> flat local
	// (see fileExports.nsMembers).
	nsMembers map[string]map[string]string
	// nsConsts maps a namespace name to member -> fold text
	// (see fileExports.nsConsts).
	nsConsts map[string]map[string]string
	nsConstStr map[string]map[string]bool
}

// unreachableOrder lists files outside the reachable set in sorted order
// (deterministic diagnostics: map iteration order must not leak).
func unreachableOrder(parsed map[string]*ast.SourceFile, reachable []string) []string {
	inReach := map[string]bool{}
	for _, p := range reachable {
		inReach[p] = true
	}
	var out []string
	for p := range parsed {
		if !inReach[p] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// linkRoute renders the cross-file miss diagnostic for name: importable
// kinds point at the import, unimportable ones name their gap honestly.
// Returns "" when the name is unknown or linking is off (callers fall
// through to their existing diagnostics).
func (e *emitter) linkRoute(name string) string {
	if e.link == nil {
		return ""
	}
	// Already available here (own definition or import): advising an
	// import is vacuous; the gap is the use shape. Functions have no
	// first-class value in the subset (call them instead).
	if e.linkExportKind[name] == "function" && (e.localDefs[name] || e.importedNames[name]) {
		return fmt.Sprintf("%s as a value is not lowerable (functions have no first-class value; call %s(...) directly)", name, name)
	}
	file, ok := e.linkExports[name]
	if !ok {
		return ""
	}
	// Namespaces aren't name-importable at all: their message names the
	// gap regardless of export flags (namespaces are never in
	// expOf.exports). Exported literal const values link by value, so a
	// miss points at the import; other values stay an honest gap.
	// Functions and classes point at the import when
	// exported, else at the missing export.
	switch e.linkExportKind[name] {
	case "namespace":
		return fmt.Sprintf("%s is a namespace defined in %s; cross-file namespace member access is not lowerable yet", name, file)
	case "value":
		// Exported literal const scalars link by value (globalConsts):
		// a miss is an import away, not a backend gap. Unexported or
		// non-const values (let, templates, objects) stay an honest gap.
		if e.linkExported[name] {
			return fmt.Sprintf("%s is defined in %s; import it first", name, file)
		}
		return fmt.Sprintf("%s is defined in %s, but cross-file value imports are not lowerable yet", name, file)
	}
	if e.linkExported[name] {
		return fmt.Sprintf("%s is defined in %s; import it first", name, file)
	}
	return fmt.Sprintf("%s is defined in %s but not exported (export it, then import it)", name, file)
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
	typeUsedOf := map[string]map[string]bool{}
	for p, sf := range parsed {
		usedOf[p] = valueUsedNames(sf.AsSourceFile().Statements.Nodes)
		typeUsedOf[p] = typeUsedNames(sf.AsSourceFile().Statements.Nodes)
	}
	// Import graph over relative specifiers.
	graph := map[string][]string{}
	specOf := map[string]map[string]string{} // file -> spec -> target
	typeSpecOf := map[string]map[string]string{} // file -> spec -> target (layouts only)
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
			// Type-only-used imports keep a layouts-only edge instead:
			// interface/class layouts lower field accesses, so the target
			// must join prescan and the checker set (but never fuses
			// cycles, links code, or binds values).
			if st.Kind == ast.KindImportDeclaration && !importDeclValueEdge(st, usedOf[p]) {
				if importDeclTypeEdge(st, typeUsedOf[p]) {
					if typeSpecOf[p] == nil {
						typeSpecOf[p] = map[string]string{}
					}
					typeSpecOf[p][spec] = tgt
				}
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
	// Layouts-only closure over type edges (transitive fixpoint, no cycle
	// check: these files feed prescan maps and the checker set, never
	// codegen or linking, so cycles cannot fuse).
	inReachSet := map[string]bool{}
	for _, p := range reachable {
		inReachSet[p] = true
	}
	typeReached := []string{}
	typeSeen := map[string]bool{}
	queue := append([]string{}, reachable...)
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		// Value edges out of type-reached files matter too: a base
		// class (heritage is a runtime dep) lives behind one. Both
		// edge kinds only feed prescan maps and the checker set.
		seen := map[string]bool{}
		for _, q := range graph[p] {
			seen[q] = true
		}
		for _, q := range typeSpecOf[p] {
			seen[q] = true
		}
		for q := range seen {
			if inReachSet[q] || typeSeen[q] {
				continue
			}
			typeSeen[q] = true
			typeReached = append(typeReached, q)
			queue = append(queue, q)
		}
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
	sharedClassParent := map[string]string{} // subclass -> direct base (multi-level super chains link across files)
	sharedStaticDefs := map[string]*classDef{}
	sharedEnums := map[string]map[string]int64{}
	globalRets := map[string]map[string]saType{} // file -> name -> ret
	globalConsts := map[string]map[string]string{} // file -> name -> literal text
	globalConstStr := map[string]map[string]bool{} // file -> name -> string const
	globalEnums := map[string]map[string]map[string]int64{} // file -> enum -> member -> ordinal
	globalArity := map[string]map[string]int{}
	globalRest := map[string]map[string]bool{}
	linkKindTmp := map[string]map[string]string{} // file -> name -> kind
	linkExpTmp := map[string]map[string]bool{}    // file -> name -> exported
	for _, p := range reachable {
		expOf[p] = &fileExports{exports: map[string]bool{}, rets: map[string]saType{}, reexp: map[string]string{}, starProvided: map[string]bool{}}
		linkKindTmp[p] = map[string]string{}
		linkExpTmp[p] = map[string]bool{}
		globalRets[p] = map[string]saType{}
		globalConsts[p] = map[string]string{}
		globalConstStr[p] = map[string]bool{}
		globalEnums[p] = map[string]map[string]int64{}
		globalArity[p] = map[string]int{}
		globalRest[p] = map[string]bool{}
		globalDefaults[p] = map[string][]bool{}
		text := files[p]
		scratch := &emitter{file: p, src: text, lines: lineOffsets(text)}
		// Type maps are shared across the prescan (reachable is
		// post-order, leaves first): cross-file heritage bases and
		// aliased layouts resolve instead of silently missing.
		scratch.layouts = sharedLayouts
		scratch.classDefs = sharedClassDefs
		scratch.staticDefs = sharedStaticDefs
		scratch.classParent = sharedClassParent
		scratch.enums = sharedEnums
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
				linkKindTmp[p][name] = "function"
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
					// Literal const/let values record for routed miss
					// diagnostics (cross-file value imports stay a
					// listed gap; the message says so honestly).
					if init := d.Initializer(); init != nil {
						switch init.Kind {
						case ast.KindNumericLiteral, ast.KindStringLiteral,
							ast.KindTrueKeyword, ast.KindFalseKeyword,
							ast.KindNoSubstitutionTemplateLiteral:
							if name, ok := bindingNameText(d); ok {
								linkKindTmp[p][name] = "value"
								// Exported CONST scalars link by value (let
								// stays out: reassignment would stale the
								// importer's fold, and the importer's const
								// reassign guard assumes immutability).
								// Shapes mirror tryTopLevelConst; templates
								// stay loud (no constVals form).
								isConst := st.AsVariableStatement().DeclarationList.Flags&ast.NodeFlagsConst != 0
								if isConst && hasExportModifier(st) {
									switch init.Kind {
									case ast.KindNumericLiteral:
										expOf[p].exports[name] = true
										globalConsts[p][name] = init.Text()
									case ast.KindStringLiteral:
										if s, ok := stringLiteralText(init); ok {
											expOf[p].exports[name] = true
											globalConsts[p][name] = s
											globalConstStr[p][name] = true
										}
									case ast.KindTrueKeyword:
										expOf[p].exports[name] = true
										globalConsts[p][name] = "1"
									case ast.KindFalseKeyword:
										expOf[p].exports[name] = true
										globalConsts[p][name] = "0"
									}
								}
							}
						}
					}
					// Arrow and function-expression callees share the
					// alias path (see lowerArrowBinding); both export.
					if d.Initializer() == nil || (d.Initializer().Kind != ast.KindArrowFunction &&
						d.Initializer().Kind != ast.KindFunctionExpression) {
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
					linkKindTmp[p][name] = "function"
				}
			case ast.KindInterfaceDeclaration, ast.KindTypeAliasDeclaration, ast.KindEnumDeclaration:
				scratch.lowerTypeDecl(st)
				// Exported all-integer enums ship member tables for
				// cross-file ordinal folds (same numbering core as
				// recordEnum; string/computed members stay loud).
				if st.Kind == ast.KindEnumDeclaration && hasExportModifier(st) &&
					st.Name() != nil && st.Name().Kind == ast.KindIdentifier {
					if members, ok := enumMemberTable(st, true); ok {
						name := st.Name().Text()
						expOf[p].exports[name] = true
						globalEnums[p][name] = members
					}
				}
			case ast.KindClassDeclaration:
				scratch.recordClass(st)
				if st.Name() != nil && st.Name().Kind == ast.KindIdentifier {
					linkKindTmp[p][st.Name().Text()] = "class"
					if hasExportModifier(st) {
						expOf[p].exports[st.Name().Text()] = true
					}
				}
			case ast.KindModuleDeclaration:
				// Runtime namespaces contribute qualified signatures for
				// same-file calls (ambient blocks skip; see namespace_ts.go).
				if isAmbientModule(st) {
					continue
				}
				if nm, ok := moduleDeclName(st); ok {
					linkKindTmp[p][nm] = "namespace"
					collectNsProgramSigs(moduleMemberStmts(st), nm, globalRets[p], globalArity[p], globalRest[p], globalDefaults[p])
					collectNsMembers(moduleMemberStmts(st), nm, expOf[p])
					collectNsConsts(moduleMemberStmts(st), nm, expOf[p])
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
	// Layouts-only prescan for type-reached files (never lowered): harvest
	// interfaces, aliases, enums and classes into the shared maps so
	// cross-file field/method/ordinal folds resolve. No signatures, no
	// links, no diagnostics; scratch refusals stay silent (unrecordable
	// shapes simply don't share). Fixpoint iteration (bounded by file
	// count): heritage chains resolve base-first regardless of order,
	// and true cycles converge to unrecorded instead of hanging.
	for pass := 0; pass < len(typeReached)+1; pass++ {
		before := len(sharedLayouts) + len(sharedClassDefs) + len(sharedStaticDefs) + len(sharedEnums)
		for _, p := range typeReached {
			text := files[p]
			scratch := &emitter{file: p, src: text, lines: lineOffsets(text)}
			// Type maps are shared across the prescan: cross-file
			// heritage bases and aliased layouts resolve instead of
			// silently missing.
			scratch.layouts = sharedLayouts
			scratch.classDefs = sharedClassDefs
			scratch.staticDefs = sharedStaticDefs
			scratch.classParent = sharedClassParent
			scratch.enums = sharedEnums
			for _, st := range parsed[p].AsSourceFile().Statements.Nodes {
				switch st.Kind {
				case ast.KindInterfaceDeclaration, ast.KindTypeAliasDeclaration, ast.KindEnumDeclaration:
					scratch.lowerTypeDecl(st)
				case ast.KindClassDeclaration:
					scratch.recordClass(st)
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
		if len(sharedLayouts)+len(sharedClassDefs)+len(sharedStaticDefs)+len(sharedEnums) == before {
			break
		}
	}
	// Diagnostic-only name index over UNREACHABLE files: a miss can name
	// a defining file the user hasn't imported yet (importing it then
	// works through the normal link). Reachable files win ties below;
	// nothing here lowers or emits.
	inReach := map[string]bool{}
	for _, p := range reachable {
		inReach[p] = true
	}
	for p, sf := range parsed {
		if inReach[p] {
			continue
		}
		if linkKindTmp[p] == nil {
			linkKindTmp[p] = map[string]string{}
		}
		if linkExpTmp[p] == nil {
			linkExpTmp[p] = map[string]bool{}
		}
		for _, st := range sf.AsSourceFile().Statements.Nodes {
			switch st.Kind {
			case ast.KindFunctionDeclaration:
				if st.Name() != nil && st.Name().Kind == ast.KindIdentifier {
					linkKindTmp[p][st.Name().Text()] = "function"
					if hasExportModifier(st) {
						linkExpTmp[p][st.Name().Text()] = true
					}
				}
			case ast.KindClassDeclaration:
				if st.Name() != nil && st.Name().Kind == ast.KindIdentifier {
					linkKindTmp[p][st.Name().Text()] = "class"
					if hasExportModifier(st) {
						linkExpTmp[p][st.Name().Text()] = true
					}
				}
			case ast.KindEnumDeclaration:
				if st.Name() != nil && st.Name().Kind == ast.KindIdentifier {
					// Only all-integer enums fold by value once imported: string
					// and computed members stay loud, so their misses must not
					// promise an import that cannot help.
					if _, ok := enumMemberTable(st, true); ok {
						linkKindTmp[p][st.Name().Text()] = "value"
					}
					if hasExportModifier(st) {
						linkExpTmp[p][st.Name().Text()] = true
					}
				}
			case ast.KindModuleDeclaration:
				if isAmbientModule(st) {
					continue
				}
				if nm, ok := moduleDeclName(st); ok {
					linkKindTmp[p][nm] = "namespace"
				}
			case ast.KindVariableStatement:
				dl := st.AsVariableStatement().DeclarationList.AsVariableDeclarationList()
				for _, d := range dl.Declarations.Nodes {
					name, ok := bindingNameText(d)
					if !ok || d.Initializer() == nil {
						continue
					}
					switch d.Initializer().Kind {
					case ast.KindArrowFunction, ast.KindFunctionExpression:
						linkKindTmp[p][name] = "function"
					case ast.KindNumericLiteral, ast.KindStringLiteral,
						ast.KindTrueKeyword, ast.KindFalseKeyword,
						ast.KindNoSubstitutionTemplateLiteral:
						linkKindTmp[p][name] = "value"
					}
					if hasExportModifier(st) {
						linkExpTmp[p][name] = true
					}
				}
			}
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
				consts:       globalConsts[tgt],
				constStr:     globalConstStr[tgt],
				enums:        globalEnums[tgt],
				qualified:    expOf[tgt].reexpQualified,
				defLocal:     expOf[tgt].defLocal,
				defQualified: defQ,
				defNS:        expOf[tgt].defNS,
				nsMembers:   expOf[tgt].nsMembers,
				nsConsts:    expOf[tgt].nsConsts,
				nsConstStr:  expOf[tgt].nsConstStr,
			}
		}
		links[p] = lk
	}
	// Lower in dependency order (leaves first; reachable is post-order).
	order := append([]string{}, reachable...)
	res.Files = order
	// Shared binder/checker over the reachable set (nil-safe fallback).
	// Type-reached files join the set so cross-file types resolve, even
	// though their bodies never lower.
	tcx := newTypeCtx(func() map[string]string {
		m := map[string]string{}
		for _, p := range reachable {
			m[p] = files[p]
		}
		for _, p := range typeReached {
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
	linkExportKind := map[string]string{}
	linkExported := map[string]bool{}
	// Reachable files first (tie priority), then the unreachable
	// diagnostic index (a miss can name a file worth importing).
	mergeOrder := append(append([]string{}, reachable...), unreachableOrder(parsed, reachable)...)
	for _, p := range mergeOrder {
		for name := range globalRets[p] {
			if _, ok := linkExports[name]; !ok {
				linkExports[name] = p
			}
		}
		// Kinds route miss diagnostics (first wins, like linkExports);
		// exported-ness resolves per owner file below.
		for name, kind := range linkKindTmp[p] {
			if _, ok := linkExportKind[name]; !ok {
				linkExportKind[name] = kind
				linkExports[name] = p
			}
		}
		if ex, ok := expOf[p]; ok {
			for name := range ex.exports {
				if linkExports[name] == p {
					linkExported[name] = true
				}
			}
		}
		if exp, ok := linkExpTmp[p]; ok {
			for name, exported := range exp {
				if linkExports[name] == p && exported {
					linkExported[name] = true
				}
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
		e.linkExportKind = linkExportKind
		e.linkExported = linkExported
		e.tcx = tcx
		// .d.ts return overrides for unannotated bodies in this file.
		e.dtsRet = map[string]saType{}
		for name, sg := range dtsSigs[p] {
			e.dtsRet[name] = sg.ret
		}
		e.layouts = sharedLayouts
		e.classDefs = sharedClassDefs
		e.staticDefs = sharedStaticDefs
		e.classParent = sharedClassParent
		e.enums = sharedEnums
		e.funcSigs = map[string]saType{}
		e.funcParams = map[string]int{}
		e.funcHasRest = map[string]bool{}
		e.localDefs = map[string]bool{}
		// Seed signatures: own file unprefixed + qualified names for
		// spread-arity lookups of imported callees. Re-exported names
		// (named or star) bind no local: they seed signatures for the
		// "import it first" diagnostic but never localDefs, so same-file
		// calls refuse instead of emitting the own prefix. Seeded names
		// are marked: namespace prescan must not mistake them for
		// colliding definitions (they ARE the member, same entity).
		if e.linkSeeded == nil {
			e.linkSeeded = map[string]bool{}
		}
		for name, ret := range globalRets[p] {
			e.funcSigs[name] = ret
			e.linkSeeded[name] = true
			if _, isReexp := expOf[p].reexp[name]; !isReexp && !expOf[p].starProvided[name] {
				e.localDefs[name] = true
			}
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
					// Star-provided (no local binding): direct defs and
					// named re-exports returned above, so reaching here
					// marks the name for the localDefs seeding below.
					ex.starProvided[name] = true
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
