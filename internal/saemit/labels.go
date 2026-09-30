// Labeled statements and labeled break/continue.
//
// `break lbl` / `continue lbl` route through a label table bound when the
// labeled statement lowers: loops and switch bind via the push helpers
// (pending labels attach to the freshly pushed targets), labeled blocks
// bind a break-only entry around their body. Labels die with their
// statement (sequential reuse is legal; nesting the same name is not),
// and never cross function boundaries (out-of-line arrows save/clear
// them like the breaks/conts stacks).
package saemit

import (
	"github.com/microsoft/typescript-go/internal/ast"
)

// labelDef is one bound label: the break jump plus its scope depth, and
// the continue jump for loop labels (nil for blocks and switch).
type labelDef struct {
	breakT jumpTarget
	contT  *jumpTarget
}

// pushLoopTargets pushes a loop's break/continue entries and binds any
// pending labels to them (see lowerLabeled).
func (e *emitter) pushLoopTargets(endL, topL string) {
	e.breaks = append(e.breaks, jumpTarget{endL, len(e.scopes)})
	e.conts = append(e.conts, jumpTarget{topL, len(e.scopes)})
	e.bindPendingLabels(false)
}

// pushBreakTarget pushes a break-only entry (switch, labeled blocks) and
// binds any pending labels to it (continue to such labels refuses).
func (e *emitter) pushBreakTarget(endL string) {
	e.breaks = append(e.breaks, jumpTarget{endL, len(e.scopes)})
	e.bindPendingLabels(true)
}

// bindPendingLabels attaches every pending label to the just-pushed
// targets (`a: b: for` binds both to the loop). Pending labels only
// exist between a LabeledStatement case and its inner loop/switch push.
func (e *emitter) bindPendingLabels(breakOnly bool) {
	if len(e.pendingLabels) == 0 {
		return
	}
	if e.labels == nil {
		e.labels = map[string]*labelDef{}
	}
	bt := e.breaks[len(e.breaks)-1]
	ld := &labelDef{breakT: bt}
	if !breakOnly {
		ct := e.conts[len(e.conts)-1]
		ld.contT = &ct
	}
	for _, nm := range e.pendingLabels {
		e.labels[nm] = ld
	}
	e.pendingLabels = nil
}

// lowerLabeled lowers `lbl: stmt` for loop, switch and block targets
// (anything else refuses loudly). The label binds during the inner
// lowering and dies with the statement.
func (e *emitter) lowerLabeled(st *ast.Node) {
	ls := st.AsLabeledStatement()
	lbl := ls.Label.Text()
	if _, dup := e.labels[lbl]; dup {
		e.refuse(st, "duplicate label %s is not lowerable (labels share the function scope)", lbl)
		return
	}
	inner := ls.Statement
	if inner.Kind == ast.KindBlock {
		endL := e.freshLabel("lbl_end")
		e.pendingLabels = append(e.pendingLabels, lbl)
		e.pushBreakTarget(endL)
		e.lowerBlockStatement(inner)
		e.breaks = e.breaks[:len(e.breaks)-1]
		delete(e.labels, lbl)
		wasTerm := e.terminated
		e.emitRaw("%s:", endL)
		e.terminated = wasTerm
		return
	}
	switch inner.Kind {
	case ast.KindForStatement, ast.KindWhileStatement, ast.KindForOfStatement,
		ast.KindForInStatement, ast.KindDoStatement, ast.KindSwitchStatement:
		e.pendingLabels = append(e.pendingLabels, lbl)
		e.lowerBlockStatement(inner)
		delete(e.labels, lbl)
		if len(e.pendingLabels) > 0 {
			// The inner statement never pushed targets (unreachable in
			// practice: all six kinds push); drop loudly, never silently.
			e.pendingLabels = nil
			e.refuse(st, "labeled %s did not bind (internal invariant)", inner.Kind.String())
		}
	default:
		e.refuse(st, "labeled %s is not lowerable (loops, switch and blocks only)", inner.Kind.String())
	}
}

// bodyHasContinue reports whether a loop body can execute a continue
// (labeled or not). Function boundaries reset the target, so nested
// functions are skipped; anything else over-approximates (an inner-loop
// continue also counts), which only ever adds a dead cont label.
func bodyHasContinue(n *ast.Node) bool {
	found := false
	var walk func(x *ast.Node)
	walk = func(x *ast.Node) {
		if x == nil || found {
			return
		}
		if x.Kind == ast.KindContinueStatement {
			found = true
			return
		}
		switch x.Kind {
		case ast.KindFunctionDeclaration, ast.KindArrowFunction,
			ast.KindFunctionExpression, ast.KindClassDeclaration:
			return
		}
		x.ForEachChild(func(c *ast.Node) bool {
			walk(c)
			return false
		})
	}
	walk(n)
	return found
}

// lowerBreakLabel jumps to a labeled statement's end (scope-depth
// release mirrors unlabeled break).
func (e *emitter) lowerBreakLabel(lbl string, pos *ast.Node) {
	ld, ok := e.labels[lbl]
	if !ok {
		e.refuse(pos, "break to undefined label %s is not lowerable", lbl)
		return
	}
	e.releaseForJump(ld.breakT.depth)
	e.emit("jmp %s", ld.breakT.label)
	e.terminated = true
}

// lowerContinueLabel jumps to a labeled loop's top (blocks and switch
// have no continue target and refuse loudly).
func (e *emitter) lowerContinueLabel(lbl string, pos *ast.Node) {
	ld, ok := e.labels[lbl]
	if !ok {
		e.refuse(pos, "continue to undefined label %s is not lowerable", lbl)
		return
	}
	if ld.contT == nil {
		e.refuse(pos, "continue to non-loop label %s is not lowerable", lbl)
		return
	}
	e.releaseForJump(ld.contT.depth)
	e.emit("jmp %s", ld.contT.label)
	e.contJumps++
	e.terminated = true
}
