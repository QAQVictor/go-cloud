// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package goose provides compile-time dependency injection logic as a
// Go library.
package goose

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/printer"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/loader"
)

// Generate performs dependency injection for a single package,
// returning the gofmt'd Go source code.
func Generate(bctx *build.Context, wd string, pkg string) ([]byte, error) {
	mainPkg, err := bctx.Import(pkg, wd, build.FindOnly)
	if err != nil {
		return nil, fmt.Errorf("load: %v", err)
	}
	// TODO(light): Stop errors from printing to stderr.
	conf := &loader.Config{
		Build: new(build.Context),
		Cwd:   wd,
		TypeCheckFuncBodies: func(path string) bool {
			return path == mainPkg.ImportPath
		},
	}
	*conf.Build = *bctx
	n := len(conf.Build.BuildTags)
	// TODO(light): Only apply gooseinject build tag on main package.
	conf.Build.BuildTags = append(conf.Build.BuildTags[:n:n], "gooseinject")
	conf.Import(pkg)

	prog, err := conf.Load()
	if err != nil {
		return nil, fmt.Errorf("load: %v", err)
	}
	if len(prog.InitialPackages()) != 1 {
		// This is more of a violated precondition than anything else.
		return nil, fmt.Errorf("load: got %d packages", len(prog.InitialPackages()))
	}
	pkgInfo := prog.InitialPackages()[0]
	g := newGen(prog, pkgInfo.Pkg.Path())
	injectorFiles, err := generateInjectors(g, pkgInfo)
	if err != nil {
		return nil, err
	}
	copyNonInjectorDecls(g, injectorFiles, &pkgInfo.Info)
	goSrc := g.frame()
	fmtSrc, err := format.Source(goSrc)
	if err != nil {
		// This is likely a bug from a poorly generated source file.
		// Return an error and the unformatted source.
		return goSrc, err
	}
	return fmtSrc, nil
}

// generateInjectors generates the injectors for a given package.
func generateInjectors(g *gen, pkgInfo *loader.PackageInfo) (injectorFiles []*ast.File, _ error) {
	oc := newObjectCache(g.prog)
	injectorFiles = make([]*ast.File, 0, len(pkgInfo.Files))
	for _, f := range pkgInfo.Files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			useCall := isInjector(&pkgInfo.Info, fn)
			if useCall == nil {
				continue
			}
			if len(injectorFiles) == 0 || injectorFiles[len(injectorFiles)-1] != f {
				// This is the first injector generated for this file.
				// Write a file header.
				name := filepath.Base(g.prog.Fset.File(f.Pos()).Name())
				g.p("// Injectors from %s:\n\n", name)
				injectorFiles = append(injectorFiles, f)
			}
			set, err := oc.processNewSet(pkgInfo, useCall)
			if err != nil {
				return nil, fmt.Errorf("%v: %v", g.prog.Fset.Position(fn.Pos()), err)
			}
			sig := pkgInfo.ObjectOf(fn.Name).Type().(*types.Signature)
			if err := g.inject(g.prog.Fset, fn.Name.Name, sig, set); err != nil {
				return nil, fmt.Errorf("%v: %v", g.prog.Fset.Position(fn.Pos()), err)
			}
		}
	}
	return injectorFiles, nil
}

// copyNonInjectorDecls copies any non-injector declarations from the
// given files into the generated output.
func copyNonInjectorDecls(g *gen, files []*ast.File, info *types.Info) {
	for _, f := range files {
		name := filepath.Base(g.prog.Fset.File(f.Pos()).Name())
		first := true
		for _, decl := range f.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if isInjector(info, decl) != nil {
					continue
				}
			case *ast.GenDecl:
				if decl.Tok == token.IMPORT {
					continue
				}
			default:
				continue
			}
			if first {
				g.p("// %s:\n\n", name)
				first = false
			}
			// TODO(light): Add line number at top of each declaration.
			g.writeAST(g.prog.Fset, info, decl)
			g.p("\n\n")
		}
	}
}

// gen is the generator state.
type gen struct {
	currPackage string
	buf         bytes.Buffer
	imports     map[string]string
	prog        *loader.Program // for determining package names
}

func newGen(prog *loader.Program, pkg string) *gen {
	return &gen{
		currPackage: pkg,
		imports:     make(map[string]string),
		prog:        prog,
	}
}

// frame bakes the built up source body into an unformatted Go source file.
func (g *gen) frame() []byte {
	if g.buf.Len() == 0 {
		return nil
	}
	var buf bytes.Buffer
	buf.WriteString("// Code generated by goose. DO NOT EDIT.\n\n//+build !gooseinject\n\npackage ")
	buf.WriteString(g.prog.Package(g.currPackage).Pkg.Name())
	buf.WriteString("\n\n")
	if len(g.imports) > 0 {
		buf.WriteString("import (\n")
		imps := make([]string, 0, len(g.imports))
		for path := range g.imports {
			imps = append(imps, path)
		}
		sort.Strings(imps)
		for _, path := range imps {
			// TODO(light): Omit the local package identifier if it matches
			// the package name.
			fmt.Fprintf(&buf, "\t%s %q\n", g.imports[path], path)
		}
		buf.WriteString(")\n\n")
	}
	buf.Write(g.buf.Bytes())
	return buf.Bytes()
}

// inject emits the code for an injector.
func (g *gen) inject(fset *token.FileSet, name string, sig *types.Signature, set *ProviderSet) error {
	results := sig.Results()
	var returnsCleanup, returnsErr bool
	switch results.Len() {
	case 0:
		return fmt.Errorf("inject %s: no return values", name)
	case 1:
		returnsCleanup, returnsErr = false, false
	case 2:
		switch t := results.At(1).Type(); {
		case types.Identical(t, errorType):
			returnsCleanup, returnsErr = false, true
		case types.Identical(t, cleanupType):
			returnsCleanup, returnsErr = true, false
		default:
			return fmt.Errorf("inject %s: second return type is %s; must be error or func()", name, types.TypeString(t, nil))
		}
	case 3:
		if t := results.At(1).Type(); !types.Identical(t, cleanupType) {
			return fmt.Errorf("inject %s: second return type is %s; must be func()", name, types.TypeString(t, nil))
		}
		if t := results.At(2).Type(); !types.Identical(t, errorType) {
			return fmt.Errorf("inject %s: third return type is %s; must be error", name, types.TypeString(t, nil))
		}
		returnsCleanup, returnsErr = true, true
	default:
		return fmt.Errorf("inject %s: too many return values", name)
	}
	outType := results.At(0).Type()
	params := sig.Params()
	given := make([]types.Type, params.Len())
	for i := 0; i < params.Len(); i++ {
		given[i] = params.At(i).Type()
	}
	calls, err := solve(fset, outType, given, set)
	if err != nil {
		return err
	}
	for i := range calls {
		if calls[i].hasCleanup && !returnsCleanup {
			return fmt.Errorf("inject %s: provider for %s returns cleanup but injection does not return cleanup function", name, types.TypeString(calls[i].out, nil))
		}
		if calls[i].hasErr && !returnsErr {
			return fmt.Errorf("inject %s: provider for %s returns error but injection not allowed to fail", name, types.TypeString(calls[i].out, nil))
		}
	}

	// Prequalify all types.  Since import disambiguation ignores local
	// variables, it takes precedence.
	paramTypes := make([]string, params.Len())
	for i := 0; i < params.Len(); i++ {
		paramTypes[i] = types.TypeString(params.At(i).Type(), g.qualifyPkg)
	}
	for _, c := range calls {
		g.qualifyImport(c.importPath)
		if !c.isStruct {
			// Struct providers just omit zero-valued fields.
			continue
		}
		for i := range c.args {
			if c.args[i] == -1 {
				zeroValue(c.ins[i], g.qualifyPkg)
			}
		}
	}
	outTypeString := types.TypeString(outType, g.qualifyPkg)
	zv := zeroValue(outType, g.qualifyPkg)
	// Set up local variables.
	paramNames := make([]string, params.Len())
	localNames := make([]string, len(calls))
	cleanupNames := make([]string, len(calls))
	errVar := disambiguate("err", g.nameInFileScope)
	collides := func(v string) bool {
		if v == errVar {
			return true
		}
		for _, a := range paramNames {
			if a == v {
				return true
			}
		}
		for _, l := range localNames {
			if l == v {
				return true
			}
		}
		for _, l := range cleanupNames {
			if l == v {
				return true
			}
		}
		return g.nameInFileScope(v)
	}

	g.p("func %s(", name)
	for i := 0; i < params.Len(); i++ {
		if i > 0 {
			g.p(", ")
		}
		pi := params.At(i)
		a := pi.Name()
		if a == "" || a == "_" {
			a = typeVariableName(pi.Type())
			if a == "" {
				a = "arg"
			}
		}
		paramNames[i] = disambiguate(a, collides)
		g.p("%s %s", paramNames[i], paramTypes[i])
	}
	if returnsCleanup && returnsErr {
		g.p(") (%s, func(), error) {\n", outTypeString)
	} else if returnsCleanup {
		g.p(") (%s, func()) {\n", outTypeString)
	} else if returnsErr {
		g.p(") (%s, error) {\n", outTypeString)
	} else {
		g.p(") %s {\n", outTypeString)
	}
	for i := range calls {
		c := &calls[i]
		lname := typeVariableName(c.out)
		if lname == "" {
			lname = "v"
		}
		lname = disambiguate(lname, collides)
		localNames[i] = lname
		g.p("\t%s", lname)
		if c.hasCleanup {
			cleanupNames[i] = disambiguate("cleanup", collides)
			g.p(", %s", cleanupNames[i])
		}
		if c.hasErr {
			g.p(", %s", errVar)
		}
		g.p(" := ")
		if c.isStruct {
			if _, ok := c.out.(*types.Pointer); ok {
				g.p("&")
			}
			g.p("%s{\n", g.qualifiedID(c.importPath, c.name))
			for j, a := range c.args {
				if a == -1 {
					// Omit zero value fields from composite literal.
					continue
				}
				g.p("\t\t%s: ", c.fieldNames[j])
				if a < params.Len() {
					g.p("%s", paramNames[a])
				} else {
					g.p("%s", localNames[a-params.Len()])
				}
				g.p(",\n")
			}
			g.p("\t}\n")
		} else {
			g.p("%s(", g.qualifiedID(c.importPath, c.name))
			for j, a := range c.args {
				if j > 0 {
					g.p(", ")
				}
				if a == -1 {
					g.p("%s", zeroValue(c.ins[j], g.qualifyPkg))
				} else if a < params.Len() {
					g.p("%s", paramNames[a])
				} else {
					g.p("%s", localNames[a-params.Len()])
				}
			}
			g.p(")\n")
		}
		if c.hasErr {
			g.p("\tif %s != nil {\n", errVar)
			for j := i - 1; j >= 0; j-- {
				if calls[j].hasCleanup {
					g.p("\t\t%s()\n", cleanupNames[j])
				}
			}
			g.p("\t\treturn %s", zv)
			if returnsCleanup {
				g.p(", nil")
			}
			// TODO(light): Give information about failing provider.
			g.p(", err\n")
			g.p("\t}\n")
		}
	}
	if len(calls) == 0 {
		for i := range given {
			if types.Identical(outType, given[i]) {
				g.p("\treturn %s", paramNames[i])
				break
			}
		}
	} else {
		g.p("\treturn %s", localNames[len(calls)-1])
	}
	if returnsCleanup {
		g.p(", func() {\n")
		for i := len(calls) - 1; i >= 0; i-- {
			if calls[i].hasCleanup {
				g.p("\t\t%s()\n", cleanupNames[i])
			}
		}
		g.p("\t}")
	}
	if returnsErr {
		g.p(", nil")
	}
	g.p("\n}\n\n")
	return nil
}

// writeAST prints an AST node into the generated output, rewriting any
// package references it encounters.
func (g *gen) writeAST(fset *token.FileSet, info *types.Info, node ast.Node) {
	start, end := node.Pos(), node.End()
	node = copyAST(node)
	// First, rewrite all package names. This lets us know all the
	// potentially colliding identifiers.
	node = astutil.Apply(node, func(c *astutil.Cursor) bool {
		switch node := c.Node().(type) {
		case *ast.Ident:
			// This is an unqualified identifier (qualified identifiers are peeled off below).
			obj := info.ObjectOf(node)
			if obj == nil {
				return false
			}
			if pkg := obj.Pkg(); pkg != nil && obj.Parent() == pkg.Scope() && pkg.Path() != g.currPackage {
				// An identifier from either a dot import or read from a different package.
				newPkgID := g.qualifyImport(pkg.Path())
				c.Replace(&ast.SelectorExpr{
					X:   ast.NewIdent(newPkgID),
					Sel: ast.NewIdent(node.Name),
				})
				return false
			}
			return true
		case *ast.SelectorExpr:
			pkgIdent, ok := node.X.(*ast.Ident)
			if !ok {
				return true
			}
			pkgName, ok := info.ObjectOf(pkgIdent).(*types.PkgName)
			if !ok {
				return true
			}
			// This is a qualified identifier. Rewrite and avoid visiting subexpressions.
			newPkgID := g.qualifyImport(pkgName.Imported().Path())
			c.Replace(&ast.SelectorExpr{
				X:   ast.NewIdent(newPkgID),
				Sel: ast.NewIdent(node.Sel.Name),
			})
			return false
		default:
			return true
		}
	}, nil)
	// Now that we have all the identifiers, rename any variables declared
	// in this scope to not collide.
	newNames := make(map[types.Object]string)
	inNewNames := func(n string) bool {
		for _, other := range newNames {
			if other == n {
				return true
			}
		}
		return false
	}
	var scopeStack []*types.Scope
	pkgScope := g.prog.Package(g.currPackage).Pkg.Scope()
	node = astutil.Apply(node, func(c *astutil.Cursor) bool {
		if scope := info.Scopes[c.Node()]; scope != nil {
			scopeStack = append(scopeStack, scope)
		}
		id, ok := c.Node().(*ast.Ident)
		if !ok {
			return true
		}
		obj := info.ObjectOf(id)
		if obj == nil {
			// We rewrote this identifier earlier, so it does not need
			// further rewriting.
			return true
		}
		if n, ok := newNames[obj]; ok {
			// We picked a new name for this symbol. Rewrite it.
			c.Replace(ast.NewIdent(n))
			return false
		}
		if par := obj.Parent(); par == nil || par == pkgScope {
			// Don't rename methods, field names, or top-level identifiers.
			return true
		}

		// Rename any symbols defined within writeAST's node that conflict
		// with any symbols in the generated file.
		objName := obj.Name()
		if pos := obj.Pos(); pos < start || end <= pos || !(g.nameInFileScope(objName) || inNewNames(objName)) {
			return true
		}
		newName := disambiguate(objName, func(n string) bool {
			if g.nameInFileScope(n) || inNewNames(n) {
				return true
			}
			if len(scopeStack) > 0 {
				// Avoid picking a name that conflicts with other names in the
				// current scope.
				_, obj := scopeStack[len(scopeStack)-1].LookupParent(n, 0)
				if obj != nil {
					return true
				}
			}
			return false
		})
		newNames[obj] = newName
		c.Replace(ast.NewIdent(newName))
		return false
	}, func(c *astutil.Cursor) bool {
		if info.Scopes[c.Node()] != nil {
			// Should be top of stack; pop it.
			scopeStack = scopeStack[:len(scopeStack)-1]
		}
		return true
	})
	if err := printer.Fprint(&g.buf, fset, node); err != nil {
		panic(err)
	}
}

func (g *gen) qualifiedID(path, sym string) string {
	name := g.qualifyImport(path)
	if name == "" {
		return sym
	}
	return name + "." + sym
}

func (g *gen) qualifyImport(path string) string {
	if path == g.currPackage {
		return ""
	}
	// TODO(light): This is depending on details of the current loader.
	const vendorPart = "vendor/"
	unvendored := path
	if i := strings.LastIndex(path, vendorPart); i != -1 && (i == 0 || path[i-1] == '/') {
		unvendored = path[i+len(vendorPart):]
	}
	if name := g.imports[unvendored]; name != "" {
		return name
	}
	// TODO(light): Use parts of import path to disambiguate.
	name := disambiguate(g.prog.Package(path).Pkg.Name(), func(n string) bool {
		// Don't let an import take the "err" name. That's annoying.
		return n == "err" || g.nameInFileScope(n)
	})
	g.imports[unvendored] = name
	return name
}

func (g *gen) nameInFileScope(name string) bool {
	for _, other := range g.imports {
		if other == name {
			return true
		}
	}
	_, obj := g.prog.Package(g.currPackage).Pkg.Scope().LookupParent(name, 0)
	return obj != nil
}

func (g *gen) qualifyPkg(pkg *types.Package) string {
	return g.qualifyImport(pkg.Path())
}

func (g *gen) p(format string, args ...interface{}) {
	fmt.Fprintf(&g.buf, format, args...)
}

// zeroValue returns the shortest expression that evaluates to the zero
// value for the given type.
func zeroValue(t types.Type, qf types.Qualifier) string {
	switch u := t.Underlying().(type) {
	case *types.Array, *types.Struct:
		return types.TypeString(t, qf) + "{}"
	case *types.Basic:
		info := u.Info()
		switch {
		case info&types.IsBoolean != 0:
			return "false"
		case info&(types.IsInteger|types.IsFloat|types.IsComplex) != 0:
			return "0"
		case info&types.IsString != 0:
			return `""`
		default:
			panic("unreachable")
		}
	case *types.Chan, *types.Interface, *types.Map, *types.Pointer, *types.Signature, *types.Slice:
		return "nil"
	default:
		panic("unreachable")
	}
}

// typeVariableName invents a variable name derived from the type name
// or returns the empty string if one could not be found.
func typeVariableName(t types.Type) string {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	tn, ok := t.(*types.Named)
	if !ok {
		return ""
	}
	// TODO(light): Include package name when appropriate.
	return unexport(tn.Obj().Name())
}

// unexport converts a name that is potentially exported to an unexported name.
func unexport(name string) string {
	r, sz := utf8.DecodeRuneInString(name)
	if !unicode.IsUpper(r) {
		// foo -> foo
		return name
	}
	r2, sz2 := utf8.DecodeRuneInString(name[sz:])
	if !unicode.IsUpper(r2) {
		// Foo -> foo
		return string(unicode.ToLower(r)) + name[sz:]
	}
	// UPPERWord -> upperWord
	sbuf := new(strings.Builder)
	sbuf.WriteRune(unicode.ToLower(r))
	i := sz
	r, sz = r2, sz2
	for unicode.IsUpper(r) && sz > 0 {
		r2, sz2 := utf8.DecodeRuneInString(name[i+sz:])
		if sz2 > 0 && unicode.IsLower(r2) {
			break
		}
		i += sz
		sbuf.WriteRune(unicode.ToLower(r))
		r, sz = r2, sz2
	}
	sbuf.WriteString(name[i:])
	return sbuf.String()
}

// disambiguate picks a unique name, preferring name if it is already unique.
func disambiguate(name string, collides func(string) bool) string {
	if !collides(name) {
		return name
	}
	buf := []byte(name)
	if len(buf) > 0 && buf[len(buf)-1] >= '0' && buf[len(buf)-1] <= '9' {
		buf = append(buf, '_')
	}
	base := len(buf)
	for n := 2; ; n++ {
		buf = strconv.AppendInt(buf[:base], int64(n), 10)
		sbuf := string(buf)
		if !collides(sbuf) {
			return sbuf
		}
	}
}

var (
	errorType   = types.Universe.Lookup("error").Type()
	cleanupType = types.NewSignature(nil, nil, nil, false)
)