package converter

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yunabe/lgo/parser"
)

// isIdentRune returns whether a rune can be a part of an identifier.
func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func identifierAt(src []byte, idx int) (start, end int) {
	if idx > len(src) || idx < 0 {
		return -1, -1
	}
	end = idx
	for {
		r, size := utf8.DecodeRune(src[end:])
		if !isIdentRune(r) {
			break
		}
		end += size
	}
	start = idx
	for {
		r, size := utf8.DecodeLastRune(src[:start])
		if !isIdentRune(r) {
			break
		}
		start -= size
	}
	if start == end {
		return -1, -1
	}
	if r, _ := utf8.DecodeRune(src[start:]); unicode.IsDigit(r) {
		// Starts with a digit, which is not an identifier.
		return -1, -1
	}
	return
}

func findLastDot(src []byte, idx int) (dot, idStart, idEnd int) {
	idStart, idEnd = identifierAt(src, idx)
	var s int
	if idStart < 0 {
		s = idx
	} else {
		s = idStart
	}
	for {
		r, size := utf8.DecodeLastRune(src[:s])
		if unicode.IsSpace(r) {
			s -= size
			continue
		}
		if r == '.' {
			s -= size
		}
		break
	}
	if src[s] == '.' {
		if idStart < 0 {
			return s, idx, idx
		}
		return s, idStart, idEnd
	}
	return -1, -1, -1
}

func Complete(src []byte, pos token.Pos, conf *Config) ([]string, int, int) {
	if dot, start, end := findLastDot(src, int(pos-1)); dot >= 0 {
		return completeDot(src, dot, start, end, conf), start, end
	}
	return nil, 0, 0
}

type findSelectorVisitor struct {
	dotPos   token.Pos
	selector *ast.SelectorExpr
}

func (v *findSelectorVisitor) Visit(n ast.Node) ast.Visitor {
	if v.selector != nil || n == nil {
		return nil
	}
	if v.dotPos < n.Pos() || n.End() <= v.dotPos {
		return nil
	}
	s, _ := n.(*ast.SelectorExpr)
	if s == nil {
		return v
	}
	if s.X.End() <= v.dotPos && v.dotPos < s.Sel.Pos() {
		v.selector = s
		return nil
	}
	return v
}

type isPosInFuncBodyVisitor struct {
	pos    token.Pos
	inBody bool
}

func (v *isPosInFuncBodyVisitor) Visit(n ast.Node) ast.Visitor {
	if n == nil || v.inBody {
		return nil
	}
	pos := v.pos
	if pos < n.Pos() || n.End() <= pos {
		// pos is out side of n.
		return nil
	}
	var body *ast.BlockStmt
	switch n := n.(type) {
	case *ast.FuncDecl:
		body = n.Body
	case *ast.FuncLit:
		body = n.Body
	}
	if body != nil && body.Pos() < pos && pos < body.End() {
		// Note: pos == n.Pos() means the cursor is right before '{'. Return false in that case.
		v.inBody = true
	}
	return v
}

// isPosInFuncBody returns whether pos is inside a function body in lgo source.
// Please call this method before any convertion on blk.
func isPosInFuncBody(blk *parser.LGOBlock, pos token.Pos) bool {
	v := isPosInFuncBodyVisitor{pos: pos}
	for _, stmt := range blk.Stmts {
		ast.Walk(&v, stmt)
		if v.inBody {
			return true
		}
	}
	return false
}

func completeDot(src []byte, dot, start, end int, conf *Config) []string {
	// TODO: Consolidate code with Convert and Inspect.
	fset, blk, _ := parseLesserGoString(string(src))
	var sel *ast.SelectorExpr
	for _, stmt := range blk.Stmts {
		v := &findSelectorVisitor{dotPos: token.Pos(dot + 1)}
		ast.Walk(v, stmt)
		if v.selector != nil {
			sel = v.selector
			break
		}
	}
	if sel == nil {
		return nil
	}
	// Whether dot is inside a function body.
	inFuncBody := isPosInFuncBody(blk, token.Pos(dot+1))

	phase1 := convertToPhase1(blk)
	makePkg := func() (pkg *types.Package, runctx types.Object) {
		// TODO: Add a proper name to the package though it's not used at this moment.
		pkg, vscope := types.NewPackageWithOldValues("cmd/hello", "", conf.Olds)
		// TODO: Come up with better implementation to resolve pkg <--> vscope circular deps.
		for _, im := range conf.OldImports {
			pname := types.NewPkgName(token.NoPos, pkg, im.Name(), im.Imported())
			vscope.Insert(pname)
		}
		if vscope.Lookup("runctx") == nil {
			ctxP, err := defaultImporter.Import("context")
			if err != nil {
				panic(fmt.Sprintf("Failed to import context: %v", err))
			}
			runctx = types.NewVar(token.NoPos, pkg, "runctx", ctxP.Scope().Lookup("Context").Type())
			vscope.Insert(runctx)
		}
		return pkg, runctx
	}

	chConf := &types.Config{
		Importer:          defaultImporter,
		Error:             func(err error) {},
		IgnoreFuncBodies:  true,
		DontIgnoreLgoInit: true,
	}
	var info = types.Info{
		Defs:   make(map[*ast.Ident]types.Object),
		Uses:   make(map[*ast.Ident]types.Object),
		Scopes: make(map[ast.Node]*types.Scope),
		Types:  make(map[ast.Expr]types.TypeAndValue),
	}
	pkg, runctx := makePkg()
	checker := types.NewChecker(chConf, fset, pkg, &info)
	checker.Files([]*ast.File{phase1.file})

	orig := strings.ToLower(string(src[start:end]))

	if !inFuncBody {
		return completeSelectExpr(checker, sel, orig)
	}

	convertToPhase2(phase1, pkg, checker, conf, runctx)
	{
		chConf := &types.Config{
			Importer: newImporterWithOlds(conf.Olds),
			Error: func(err error) {
				// Ignore errors.
				// It is necessary to set this noop func because checker stops analyzing code
				// when the first error is found if Error is nil.
			},
			IgnoreFuncBodies:  false,
			DontIgnoreLgoInit: true,
		}
		var info = types.Info{
			Defs:   make(map[*ast.Ident]types.Object),
			Uses:   make(map[*ast.Ident]types.Object),
			Scopes: make(map[ast.Node]*types.Scope),
			Types:  make(map[ast.Expr]types.TypeAndValue),
		}
		// Note: Do not reuse pkg above here because variables are already defined in the scope of pkg above.
		pkg, _ := makePkg()
		checker := types.NewChecker(chConf, fset, pkg, &info)
		checker.Files([]*ast.File{phase1.file})

		return completeSelectExpr(checker, sel, orig)
	}
}

func completeSelectExpr(checker *types.Checker, sel *ast.SelectorExpr, orig string) []string {
	suggests := make(map[string]bool)
	add := func(s string) {
		if strings.HasPrefix(strings.ToLower(s), orig) {
			suggests[s] = true
		}
	}
	func() {
		// Complete package fields selector (e.g. bytes.buf[cur] --> bytes.Buffer)
		x, _ := sel.X.(*ast.Ident)
		if x == nil {
			return
		}
		obj := checker.Uses[x]
		if obj == nil {
			return
		}
		pkg, _ := obj.(*types.PkgName)
		if pkg == nil {
			return
		}
		im := pkg.Imported()
		for _, name := range im.Scope().Names() {
			if o := im.Scope().Lookup(name); o.Exported() {
				add(name)
			}
		}
	}()
	func() {
		// Complete x.sel[cur] case.
		// TODO: Support more cases (cf. https://golang.org/ref/spec#Selectors)
		tv, ok := checker.Types[sel.X]
		if !ok {
			return
		}
		if !tv.IsValue() {
			return
		}
		ty, ok := tv.Type.(*types.Named)
		if !ok {
			return
		}
		for i := 0; i < ty.NumMethods(); i++ {
			if m := ty.Method(i); m.Exported() {
				add(m.Name())
			}
		}
	}()
	if len(suggests) == 0 {
		return nil
	}
	var results []string
	for key := range suggests {
		results = append(results, key)
	}
	sort.Strings(results)
	return results
}
