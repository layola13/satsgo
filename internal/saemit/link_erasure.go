// Import erasure: explicit `import type` / `export type` filtering lives
// inline at the use sites; this module owns usage-based erasure
// (esbuild importsNotUsedAsValues semantics): value-position identifier
// analysis plus the per-declaration runtime-edge decision.
//
// program.go keeps the graph/filter hooks, saemit.go keeps the lowering
// hook; the failure direction stays conservative (a missed use only keeps
// an edge, never drops a real one — and a wrongly dropped edge surfaces
// as a loud "import it first" refusal, never a silent miscompile).
package saemit

import (
	"github.com/microsoft/typescript-go/internal/ast"
)

// pureTypeKinds are AST kinds that can never contain a runtime value use.
// The usage walk never descends into them, so type names are not mistaken
// for runtime deps. The set stays conservative: missing a pure-type kind
// only keeps an edge (status quo), never drops a real one.
var pureTypeKinds = map[ast.Kind]bool{
	ast.KindTypeReference:       true,
	ast.KindTypeQuery:           true,
	ast.KindTypeLiteral:         true,
	ast.KindTupleType:           true,
	ast.KindArrayType:           true,
	ast.KindUnionType:           true,
	ast.KindIntersectionType:    true,
	ast.KindFunctionType:        true,
	ast.KindConstructorType:     true,
	ast.KindTypeOperator:        true,
	ast.KindIndexedAccessType:   true,
	ast.KindMappedType:          true,
	ast.KindLiteralType:         true,
	ast.KindOptionalType:        true,
	ast.KindRestType:            true,
	ast.KindTypeParameter:       true,
	ast.KindTypePredicate:       true,
	ast.KindThisType:            true,
	ast.KindTemplateLiteralType: true,
	ast.KindParenthesizedType:   true,
	ast.KindMethodSignature:     true,
	ast.KindPropertySignature:   true,
	ast.KindCallSignature:       true,
	ast.KindConstructSignature:  true,
	ast.KindIndexSignature:      true,
}

// bindingNameKinds are AST kinds whose Name() child binds rather than uses
// (declarations, parameters, patterns, member names). The usage walk skips
// exactly that child; every other occurrence still counts as a use.
// ShorthandPropertyAssignment is deliberately absent: `{Shape}` reads Shape.
var bindingNameKinds = map[ast.Kind]bool{
	ast.KindVariableDeclaration:      true,
	ast.KindParameter:                true,
	ast.KindBindingElement:           true,
	ast.KindFunctionDeclaration:      true,
	ast.KindFunctionExpression:       true,
	ast.KindClassDeclaration:         true,
	ast.KindClassExpression:          true,
	ast.KindEnumDeclaration:          true,
	ast.KindEnumMember:               true,
	ast.KindInterfaceDeclaration:     true,
	ast.KindTypeAliasDeclaration:     true,
	ast.KindMethodDeclaration:        true,
	ast.KindGetAccessor:              true,
	ast.KindSetAccessor:              true,
	ast.KindPropertyAssignment:       true,
	ast.KindPropertyAccessExpression: true,
}

// valueUsedNames collects identifier texts referenced in value positions
// across one file's statements: whole import declarations (bindings and
// remote names) and MetaProperty nodes are skipped, binding names are
// skipped, and pure-type subtrees are never entered. Heritage clauses
// (`extends B`) stay visited: they are runtime deps.
//
// Local export lists reference local values (`export {X}` keeps X's edge;
// `export default <expr>` bodies count fully); re-export specifiers name
// remote values (the graph loop keeps that edge separately).
func valueUsedNames(stmts []*ast.Node) map[string]bool {
	used := map[string]bool{}
	var walk func(n *ast.Node)
	walk = func(n *ast.Node) {
		if n == nil {
			return
		}
		// Import specifiers bind; remote names are never local uses.
		if n.Kind == ast.KindImportDeclaration || n.Kind == ast.KindMetaProperty {
			return
		}
		if n.Kind == ast.KindExportDeclaration {
			if n.IsTypeOnly() {
				return
			}
			ed := n.AsExportDeclaration()
			if ed.ModuleSpecifier != nil {
				return
			}
			for ch := range n.IterChildren() {
				if ch.Kind == ast.KindExportSpecifier {
					if ch.IsTypeOnly() {
						continue
					}
				}
				if pureTypeKinds[ch.Kind] {
					continue
				}
				walk(ch)
			}
			return
		}
		if n.Kind == ast.KindIdentifier {
			used[n.Text()] = true
			return
		}
		var skip *ast.Node
		if bindingNameKinds[n.Kind] {
			if nm := n.Name(); nm != nil {
				skip = nm
			}
		}
		for ch := range n.IterChildren() {
			if skip != nil && ch == skip {
				continue
			}
			if pureTypeKinds[ch.Kind] {
				continue
			}
			walk(ch)
		}
	}
	for _, st := range stmts {
		walk(st)
	}
	return used
}

// importDeclValueEdge reports whether an import declaration carries a
// runtime edge: side-effect imports always do; otherwise the default name,
// the namespace name, or at least one non-type-only named specifier must
// be value-used (esbuild importsNotUsedAsValues semantics, per declaration).
func importDeclValueEdge(st *ast.Node, used map[string]bool) bool {
	cl := st.AsImportDeclaration().ImportClause
	if cl == nil {
		return true
	}
	clause := cl.AsImportClause()
	if nm := clause.Name(); nm != nil && nm.Kind == ast.KindIdentifier {
		if used[nm.Text()] {
			return true
		}
	}
	nb := clause.NamedBindings
	if nb == nil {
		return false
	}
	if nb.Kind == ast.KindNamespaceImport {
		return used[nb.AsNamespaceImport().Name().Text()]
	}
	edge := false
	var walk func(n *ast.Node)
	walk = func(n *ast.Node) {
		if edge || n == nil {
			return
		}
		if n.Kind == ast.KindImportSpecifier {
			sp := n.AsImportSpecifier()
			if sp.IsTypeOnly {
				return
			}
			if nm := n.Name(); nm != nil && nm.Kind == ast.KindIdentifier && used[nm.Text()] {
				edge = true
			}
			return
		}
		for ch := range n.IterChildren() {
			walk(ch)
		}
	}
	walk(nb)
	return edge
}
