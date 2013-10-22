package translator

import (
	"code.google.com/p/go.tools/go/exact"
	"code.google.com/p/go.tools/go/types"
	"fmt"
	"go/ast"
	"go/token"
	"strings"
)

var ReservedKeywords = []string{"arguments", "class", "delete", "eval", "export", "false", "function", "implements", "interface", "in", "let", "new", "package", "private", "protected", "public", "static", "this", "true", "try", "yield"}

type ErrorList []error

func (err ErrorList) Error() string {
	return err[0].Error()
}

type PkgContext struct {
	pkg          *types.Package
	info         *types.Info
	pkgVars      map[string]string
	objectVars   map[types.Object]string
	usedVarNames []string
	functionSig  *types.Signature
	resultNames  []ast.Expr
	postLoopStmt ast.Stmt
	output       []byte
	indentation  int
	delayedLines []byte
}

func (c *PkgContext) Write(b []byte) (int, error) {
	c.output = append(c.output, b...)
	return len(b), nil
}

func (c *PkgContext) Printf(format string, values ...interface{}) {
	c.Write([]byte(strings.Repeat("\t", c.indentation)))
	fmt.Fprintf(c, format, values...)
	c.Write([]byte{'\n'})
	c.Write(c.delayedLines)
	c.delayedLines = nil
}

func (c *PkgContext) Indent(f func()) {
	c.indentation += 1
	f()
	c.indentation -= 1
}

func (c *PkgContext) CatchOutput(f func()) []byte {
	origoutput := c.output
	c.output = nil
	f()
	catched := c.output
	c.output = origoutput
	return catched
}

func (c *PkgContext) Delayed(f func()) {
	c.delayedLines = c.CatchOutput(f)
}

func TranslatePackage(importPath string, files []*ast.File, fileSet *token.FileSet, config *types.Config) ([]byte, error) {
	info := &types.Info{
		Types:      make(map[ast.Expr]types.Type),
		Values:     make(map[ast.Expr]exact.Value),
		Objects:    make(map[*ast.Ident]types.Object),
		Implicits:  make(map[ast.Node]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}

	var errList ErrorList
	var previousErr error
	config.Error = func(err error) {
		if previousErr != nil && previousErr.Error() == err.Error() {
			return
		}
		errList = append(errList, err)
		previousErr = err
	}
	config.Sizes = sizes32
	typesPkg, err := config.Check(importPath, fileSet, files, info)
	if errList != nil {
		return nil, errList
	}
	if err != nil {
		return nil, err
	}
	config.Packages[importPath] = typesPkg

	c := &PkgContext{
		pkg:          typesPkg,
		info:         info,
		pkgVars:      make(map[string]string),
		objectVars:   make(map[types.Object]string),
		usedVarNames: ReservedKeywords,
	}

	functionsByType := make(map[types.Type][]*ast.FuncDecl)
	functionsByObject := make(map[types.Object]*ast.FuncDecl)
	var initStmts []ast.Stmt
	var typeSpecs []*ast.TypeSpec
	var constSpecs []*ast.ValueSpec
	var varSpecs []*ast.ValueSpec
	for _, file := range files {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				sig := c.info.Objects[d.Name].(*types.Func).Type().(*types.Signature)
				var recvType types.Type
				if sig.Recv() != nil {
					recvType = sig.Recv().Type()
					if ptr, isPtr := recvType.(*types.Pointer); isPtr {
						recvType = ptr.Elem()
					}
				}
				if sig.Recv() == nil && d.Name.Name == "init" {
					initStmts = append(initStmts, d.Body.List...)
					continue
				}
				functionsByType[recvType] = append(functionsByType[recvType], d)
				o := c.info.Objects[d.Name]
				functionsByObject[o] = d
				if sig.Recv() == nil {
					c.objectName(o) // register toplevel name
				}
			case *ast.GenDecl:
				switch d.Tok {
				case token.TYPE:
					for _, spec := range d.Specs {
						s := spec.(*ast.TypeSpec)
						typeSpecs = append(typeSpecs, s)
						c.objectName(c.info.Objects[s.Name]) // register toplevel name
					}
				case token.CONST:
					for _, spec := range d.Specs {
						s := spec.(*ast.ValueSpec)
						constSpecs = append(constSpecs, s)
						for _, name := range s.Names {
							if !isUnderscore(name) {
								c.objectName(c.info.Objects[name]) // register toplevel name
							}
						}
					}
				case token.VAR:
					for _, spec := range d.Specs {
						s := spec.(*ast.ValueSpec)
						varSpecs = append(varSpecs, s)
						for _, name := range s.Names {
							if !isUnderscore(name) {
								c.objectName(c.info.Objects[name]) // register toplevel name
							}
						}
					}
				}
			}
		}
	}

	return c.CatchOutput(func() {
		c.Indent(func() {
			c.Printf("var Go$pkg = {};")

			for _, importedPkg := range typesPkg.Imports() {
				varName := c.newVariable(importedPkg.Name())
				c.Printf(`var %s = Go$packages["%s"];`, varName, importedPkg.Path())
				c.pkgVars[importedPkg.Path()] = varName
			}

			// types and their functions
			for _, spec := range typeSpecs {
				obj := c.info.Objects[spec.Name]
				typeName := c.objectName(obj)
				c.Printf("var %s;", typeName)
				c.translateSpec(spec)
				for _, fun := range functionsByType[obj.Type()] {
					_, isStruct := obj.Type().Underlying().(*types.Struct)
					c.translateMethod(typeName, isStruct, fun)
				}
				c.Printf("Go$pkg.%s = %s;", typeName, typeName)
			}

			// package functions
			for _, fun := range functionsByType[nil] {
				name := fun.Name.Name
				jsCode, _ := typesPkg.Scope().Lookup("js_" + name).(*types.Const)
				if jsCode != nil {
					c.Printf("var %s = function(%s) {\n%s\n};", name, strings.Join(c.translateParams(fun.Type), ", "), exact.StringVal(jsCode.Val()))
					continue
				}
				if fun.Body == nil {
					c.Printf(`var %s = function() { throw new Go$Panic("Native function not implemented: %s"); };`, name, name)
					continue
				}
				params := c.translateParams(fun.Type)
				c.Printf("var %s = function(%s) {", c.translateExpr(fun.Name), strings.Join(params, ", "))
				c.Indent(func() {
					c.translateFunctionBody(fun.Body.List, c.info.Objects[fun.Name].Type().(*types.Signature), params)
				})
				c.Printf("};")
			}

			// constants
			for _, spec := range constSpecs {
				for _, name := range spec.Names {
					if isUnderscore(name) || strings.HasPrefix(name.Name, "js_") {
						continue
					}
					o := c.info.Objects[name].(*types.Const)
					c.info.Types[name] = o.Type()
					c.info.Values[name] = o.Val()
					c.Printf("%s = %s;", c.objectName(o), c.translateExpr(name))
				}
			}

			// variables (TODO: handle tuples)
			var unorderedSingleVarSpecs []*ast.ValueSpec
			pendingObjects := make(map[types.Object]bool)
			for _, spec := range varSpecs {
				for i, name := range spec.Names {
					o := c.info.Objects[name].(*types.Var)
					c.Printf("%s = %s;", c.objectName(o), c.zeroValue(o.Type()))
					if i < len(spec.Values) {
						unorderedSingleVarSpecs = append(unorderedSingleVarSpecs, &ast.ValueSpec{
							Names:  []*ast.Ident{name},
							Type:   spec.Type,
							Values: []ast.Expr{spec.Values[i]},
						})
						pendingObjects[c.info.Objects[name]] = true
					}
				}
			}

			// native implementations
			if native, hasNative := natives[importPath]; hasNative {
				c.Write([]byte(strings.TrimSpace(native)))
				c.Write([]byte{'\n'})
			}

			// exports for package functions
			for _, fun := range functionsByType[nil] {
				name := fun.Name.Name
				if fun.Name.IsExported() || name == "main" {
					c.Printf("Go$pkg.%s = %s;", name, name)
				}
			}

			// init function
			c.Printf("Go$pkg.init = function() {")
			c.Indent(func() {
				complete := false
				for !complete {
					complete = true
					for i, spec := range unorderedSingleVarSpecs {
						if spec == nil {
							continue
						}
						v := IsReadyVisitor{info: c.info, functions: functionsByObject, pendingObjects: pendingObjects, isReady: true}
						ast.Walk(&v, spec.Values[0])
						if !v.isReady {
							complete = false
							continue
						}
						c.translateStmt(&ast.AssignStmt{
							Lhs: []ast.Expr{spec.Names[0]},
							Tok: token.ASSIGN,
							Rhs: []ast.Expr{spec.Values[0]},
						}, "")
						delete(pendingObjects, c.info.Objects[spec.Names[0]])
						unorderedSingleVarSpecs[i] = nil
					}
				}

				c.translateFunctionBody(initStmts, types.NewSignature(c.pkg.Scope(), nil, types.NewTuple(), types.NewTuple(), false), nil)
			})
			c.Printf("};")

			c.Printf("return Go$pkg;")
		})
	}), nil
}

func (c *PkgContext) translateSpec(spec ast.Spec) {
	switch s := spec.(type) {
	case *ast.ValueSpec:
		for _, name := range s.Names {
			c.info.Types[name] = c.info.Objects[name].Type()
		}
		i := 0
		for i < len(s.Names) {
			var rhs ast.Expr
			n := 1
			if i < len(s.Values) {
				rhs = s.Values[i]
				if tuple, isTuple := c.info.Types[rhs].(*types.Tuple); isTuple {
					n = tuple.Len()
				}
			}
			lhs := make([]ast.Expr, n)
			for j := range lhs {
				if j >= len(s.Names) {
					lhs[j] = ast.NewIdent("_")
					continue
				}
				lhs[j] = s.Names[i+j]
			}
			c.translateStmt(&ast.AssignStmt{
				Lhs: lhs,
				Tok: token.DEFINE,
				Rhs: []ast.Expr{rhs},
			}, "")
			i += n
		}

	case *ast.TypeSpec:
		obj := c.info.Objects[s.Name]
		typeName := c.objectName(obj)
		if isWrapped(obj.Type()) {
			c.Printf(`var %s = function(v) { this.Go$val = v; };`, typeName)
			c.Printf(`%s.prototype.Go$key = function() { return "%s$" + this.Go$val; };`, typeName, typeName)
			c.Printf("%s.Go$Pointer = function(getter, setter) { this.Go$get = getter; this.Go$set = setter; };", typeName)
			return
		}
		switch t := obj.Type().Underlying().(type) {
		case *types.Struct:
			params := make([]string, t.NumFields())
			for i := 0; i < t.NumFields(); i++ {
				params[i] = t.Field(i).Name() + "_"
			}
			c.Printf("%s = function(%s) {", typeName, strings.Join(params, ", "))
			c.Indent(func() {
				c.Printf("this.Go$id = Go$idCounter++;")
				c.Printf("this.Go$val = this;")
				for i := 0; i < t.NumFields(); i++ {
					field := t.Field(i)
					c.Printf("this.%s = %s_ || %s;", field.Name(), field.Name(), c.zeroValue(field.Type()))
				}
			})
			c.Printf("};")
			c.Printf(`%s.prototype.Go$key = function() { return this.Go$id; };`, typeName)
			c.Printf("%s.Go$NonPointer = function(v) { this.Go$val = v; };", typeName)
			fields := make([]string, t.NumFields())
			for i := range fields {
				field := t.Field(i)
				path := ""
				if !field.IsExported() {
					path = field.Pkg().Name() + "." + field.Name()
				}
				fields[i] = fmt.Sprintf(`new Go$reflect.structField(Go$dataPtr("%s"), Go$dataPtr("%s"), %s.prototype.Go$type(), Go$dataPtr(%#v), 0)`, field.Name(), path, c.typeName(field.Type()), t.Tag(i))
			}
			c.Printf(`%s.Go$NonPointer.prototype.Go$type = function() { var t = new Go$reflect.rtype(0, 0, 0, 0, 0, Go$reflect.Struct, %s, null, Go$dataPtr("%s.%s"), null, null); t.structType = new Go$reflect.structType(null, new Go$Slice([%s])); return t; };`, typeName, typeName, obj.Pkg().Name(), obj.Name(), strings.Join(fields, ", "))
			for i := 0; i < t.NumFields(); i++ {
				field := t.Field(i)
				if field.Anonymous() {
					fieldType := field.Type()
					_, isPointer := fieldType.(*types.Pointer)
					_, isUnderlyingInterface := fieldType.Underlying().(*types.Interface)
					if !isPointer && !isUnderlyingInterface {
						fieldType = types.NewPointer(fieldType) // strange, seems like a bug in go/types
					}
					methods := fieldType.MethodSet()
					for j := 0; j < methods.Len(); j++ {
						name := methods.At(j).Obj().Name()
						sig := methods.At(j).Type().(*types.Signature)
						params := make([]string, sig.Params().Len())
						for k := range params {
							params[k] = sig.Params().At(k).Name()
						}
						value := "this." + field.Name()
						if isWrapped(field.Type()) {
							value = fmt.Sprintf("new %s(%s)", field.Name(), value)
						}
						paramList := strings.Join(params, ", ")
						c.Printf("%s.prototype.%s = function(%s) { return %s.%s(%s); };", typeName, name, paramList, value, name, paramList)
						c.Printf("%s.Go$NonPointer.prototype.%s = function(%s) { return this.Go$val.%s(%s); };", typeName, name, paramList, name, paramList)
					}
				}
			}
		case *types.Interface:
			c.Printf("%s = { Go$implementedBy: [] };", typeName)
		default:
			underlyingTypeName := c.typeName(t)
			c.Printf("%s = function() { %s.apply(this, arguments); };", typeName, underlyingTypeName)
			c.Printf("%s.prototype.Go$key = function() { return \"%s$\" + %s.prototype.Go$key.apply(this); };", typeName, typeName, underlyingTypeName)
			c.Printf("%s.Go$Pointer = function(getter, setter) { this.Go$get = getter; this.Go$set = setter; };", typeName)
			if _, isSlice := t.(*types.Slice); isSlice {
				c.Printf("%s.Go$nil = new %s({ isNil: true, length: 0 });", typeName, typeName)
			}
		}

	case *ast.ImportSpec:
		// ignored

	default:
		panic(fmt.Sprintf("Unhandled spec: %T\n", s))

	}
}

func (c *PkgContext) translateMethod(typeName string, isStruct bool, fun *ast.FuncDecl) {
	outerVarNames := c.usedVarNames
	defer func() { c.usedVarNames = outerVarNames }()

	sig := c.info.Objects[fun.Name].(*types.Func).Type().(*types.Signature)
	recvType := sig.Recv().Type()
	ptr, isPointer := recvType.(*types.Pointer)

	params := c.translateParams(fun.Type)
	joinedParams := strings.Join(params, ", ")
	printPrimaryFunction := func(lhs string) {
		c.Printf("%s = function(%s) {", lhs, joinedParams)
		c.Indent(func() {
			if jsCode, ok := c.pkg.Scope().Lookup("js_" + typeName + "_" + fun.Name.Name).(*types.Const); ok {
				c.Write([]byte(exact.StringVal(jsCode.Val())))
				c.Write([]byte{'\n'})
				return
			}

			if fun.Recv.List[0].Names != nil {
				recv := fun.Recv.List[0].Names[0]
				this := "this"
				if isWrapped(recvType) {
					this = "this.Go$val"
				}
				c.Printf("var %s = %s;", c.objectName(c.info.Objects[recv]), this)
			}

			c.translateFunctionBody(fun.Body.List, sig, params)
		})
		c.Printf("};")
	}

	switch {
	case isStruct:
		printPrimaryFunction(typeName + ".prototype." + fun.Name.Name)
		c.Printf("%s.Go$NonPointer.prototype.%s = function(%s) { return this.Go$val.%s(%s); };", typeName, fun.Name.Name, joinedParams, fun.Name.Name, joinedParams)
	case !isStruct && !isPointer:
		value := "this.Go$get()"
		if isWrapped(recvType) {
			value = fmt.Sprintf("new %s(%s)", typeName, value)
		}
		printPrimaryFunction(typeName + ".prototype." + fun.Name.Name)
		c.Printf("%s.Go$Pointer.prototype.%s = function(%s) { return %s.%s(%s); };", typeName, fun.Name.Name, joinedParams, value, fun.Name.Name, joinedParams)
	case !isStruct && isPointer:
		value := "this"
		if isWrapped(ptr.Elem()) {
			value = "this.Go$val"
		}
		c.Printf("%s.prototype.%s = function(%s) { var obj = %s; return (new %s.Go$Pointer(function() { return obj; }, null)).%s(%s); };", typeName, fun.Name.Name, joinedParams, value, typeName, fun.Name.Name, joinedParams)
		printPrimaryFunction(typeName + ".Go$Pointer.prototype." + fun.Name.Name)
	}
}

func (c *PkgContext) translateFunctionBody(stmts []ast.Stmt, sig *types.Signature, params []string) {
	outerVarNames := c.usedVarNames
	defer func() { c.usedVarNames = outerVarNames }()
	c.usedVarNames = append(c.usedVarNames, params...)

	body := c.CatchOutput(func() {
		var resultNames []ast.Expr
		if sig.Results().Len() != 0 && sig.Results().At(0).Name() != "" {
			resultNames = make([]ast.Expr, sig.Results().Len())
			for i := 0; i < sig.Results().Len(); i++ {
				result := sig.Results().At(i)
				name := result.Name()
				if result.Name() == "_" {
					name = "result"
				}
				id := ast.NewIdent(name)
				c.info.Types[id] = result.Type()
				c.info.Objects[id] = result
				c.Printf("%s = %s;", c.translateExpr(id), c.zeroValue(result.Type()))
				resultNames[i] = id
			}
		}

		s := c.functionSig
		defer func() { c.functionSig = s }()
		c.functionSig = sig
		r := c.resultNames
		defer func() { c.resultNames = r }()
		c.resultNames = resultNames
		p := c.postLoopStmt
		defer func() { c.postLoopStmt = p }()
		c.postLoopStmt = nil

		v := HasDeferVisitor{}
		ast.Walk(&v, &ast.BlockStmt{List: stmts})
		switch v.hasDefer {
		case true:
			c.Printf("var Go$deferred = [];")
			c.Printf("try {")
			c.Indent(func() {
				c.translateStmtList(stmts)
			})
			c.Printf("} catch(Go$err) {")
			c.Indent(func() {
				c.Printf("if (Go$err.constructor !== Go$Panic) { Go$err = Go$wrapJavaScriptError(Go$err); };")
				c.Printf("Go$errorStack.push({ frame: Go$getStackDepth(), error: Go$err });")
			})
			c.Printf("} finally {")
			c.Indent(func() {
				c.Printf("Go$callDeferred(Go$deferred);")
				if resultNames != nil {
					c.translateStmt(&ast.ReturnStmt{}, "")
				}
			})
			c.Printf("}")
		case false:
			c.translateStmtList(stmts)
		}
	})

	innerVarNames := c.usedVarNames[len(outerVarNames)+len(params):]
	if len(innerVarNames) != 0 {
		c.Printf("var %s;", strings.Join(innerVarNames, ", "))
	}
	c.Write(body)
}

func (c *PkgContext) translateParams(t *ast.FuncType) []string {
	n := c.usedVarNames
	params := make([]string, 0)
	for _, param := range t.Params.List {
		for _, ident := range param.Names {
			if isUnderscore(ident) {
				params = append(params, c.newVariable("param"))
				continue
			}
			params = append(params, c.objectName(c.info.Objects[ident]))
		}
	}
	c.usedVarNames = n
	return params
}

func (c *PkgContext) translateArgs(call *ast.CallExpr) []string {
	funType := c.info.Types[call.Fun].Underlying().(*types.Signature)
	args := make([]string, funType.Params().Len())
	for i := range args {
		if funType.IsVariadic() && i == len(args)-1 && !call.Ellipsis.IsValid() {
			varargType := funType.Params().At(i).Type().(*types.Slice).Elem()
			varargs := make([]string, len(call.Args)-i)
			for i, vararg := range call.Args[i:] {
				varargs[i] = c.translateExprToType(vararg, varargType)
			}
			args[i] = fmt.Sprintf("new Go$Slice(%s)", createListComposite(varargType, varargs))
			break
		}
		argType := funType.Params().At(i).Type()
		args[i] = c.translateExprToType(call.Args[i], argType)
	}
	return args
}

func (c *PkgContext) zeroValue(ty types.Type) string {
	named, isNamed := ty.(*types.Named)
	switch t := ty.Underlying().(type) {
	case *types.Basic:
		if is64Bit(t) {
			return fmt.Sprintf("new %s(0, 0)", c.typeName(ty))
		}
		if t.Info()&types.IsBoolean != 0 {
			return "false"
		}
		if t.Info()&types.IsNumeric != 0 {
			return "0"
		}
		if t.Info()&types.IsString != 0 {
			return `""`
		}
		if t.Kind() == types.UntypedNil {
			panic("Zero value for untyped nil.")
		}
	case *types.Array:
		return fmt.Sprintf("Go$clear(new %s(%d), %s)", toArrayType(t.Elem()), t.Len(), c.zeroValue(t.Elem()))
	case *types.Slice:
		return fmt.Sprintf("%s.Go$nil", c.typeName(ty))
	case *types.Struct:
		if isNamed {
			return fmt.Sprintf("new %s()", c.objectName(named.Obj()))
		}
		fields := make([]string, t.NumFields())
		for i := range fields {
			field := t.Field(i)
			fields[i] = field.Name() + ": " + c.zeroValue(field.Type())
		}
		return fmt.Sprintf("{%s}", strings.Join(fields, ", "))
	}
	return "null"
}

func (c *PkgContext) newVariable(prefix string) string {
	n := 0
	for {
		name := prefix
		for _, b := range []byte(name) {
			if b < '0' || b > 'z' {
				name = "nonAasciiName"
				break
			}
		}
		if n != 0 {
			name += fmt.Sprintf("%d", n)
		}
		used := false
		for _, usedName := range c.usedVarNames {
			if usedName == name {
				used = true
				break
			}
		}
		if !used {
			c.usedVarNames = append(c.usedVarNames, name)
			return name
		}
		n += 1
	}
}

func (c *PkgContext) objectName(o types.Object) string {
	if o.Name() == "error" {
		return "Go$error"
	}
	if o.Pkg() != nil && o.Pkg() != c.pkg {
		return c.pkgVars[o.Pkg().Path()] + "." + o.Name()
	}

	name, found := c.objectVars[o]
	if !found {
		name = c.newVariable(o.Name())
		c.objectVars[o] = name
	}

	switch o.(type) {
	case *types.Var, *types.Const:
		if o.Parent() == c.pkg.Scope() {
			return "Go$pkg." + name
		}
	}
	return name
}

func (c *PkgContext) typeName(ty types.Type) string {
	switch t := ty.(type) {
	case *types.Basic:
		if t.Kind() == types.UntypedNil {
			return "null"
		}
		return "Go$" + toJavaScriptType(t)
	case *types.Named:
		if _, isStruct := t.Underlying().(*types.Struct); isStruct {
			return c.objectName(t.Obj()) + ".Go$NonPointer"
		}
		return c.objectName(t.Obj())
	case *types.Pointer:
		if named, isNamed := t.Elem().(*types.Named); isNamed && named.Obj().Name() != "error" {
			switch t.Elem().Underlying().(type) {
			case *types.Struct:
				return c.objectName(named.Obj())
			case *types.Interface:
				return "Go$Pointer"
			default:
				return c.objectName(named.Obj()) + ".Go$Pointer"
			}
		}
		return "Go$Pointer"
	case *types.Array:
		return "Go$Array"
	case *types.Slice:
		return "Go$Slice"
	case *types.Map:
		return "Go$Map"
	case *types.Interface:
		return "Go$Interface"
	case *types.Chan:
		return "Go$Channel"
	case *types.Signature:
		return "Go$Func"
	default:
		panic(fmt.Sprintf("Unhandled type: %T\n", t))
	}
}

func toJavaScriptType(t *types.Basic) string {
	switch t.Kind() {
	case types.UntypedInt:
		return "Int"
	default:
		name := t.String()
		return strings.ToUpper(name[:1]) + name[1:]
	}
}

func is64Bit(t *types.Basic) bool {
	return t.Kind() == types.Int64 || t.Kind() == types.Uint64
}

func isComplex(t *types.Basic) bool {
	return t.Kind() == types.Complex64 || t.Kind() == types.Complex128
}

func isTypedArray(t types.Type) bool {
	basic, isBasic := t.(*types.Basic)
	return isBasic && basic.Info()&types.IsNumeric != 0 && !is64Bit(basic) && !isComplex(basic)
}

func toArrayType(t types.Type) string {
	if isTypedArray(t) {
		return "Go$" + toJavaScriptType(t.(*types.Basic)) + "Array"
	}
	return "Go$Array"
}

func createListComposite(elementType types.Type, elements []string) string {
	if isTypedArray(elementType) {
		return fmt.Sprintf("new %s([%s])", toArrayType(elementType), strings.Join(elements, ", "))
	}
	return fmt.Sprintf("[%s]", strings.Join(elements, ", "))
}

func isUnderscore(expr ast.Expr) bool {
	if id, isIdent := expr.(*ast.Ident); isIdent {
		return id.Name == "_"
	}
	return false
}

func hasId(ty types.Type) bool {
	switch t := ty.Underlying().(type) {
	case *types.Basic:
		return is64Bit(t)
	case *types.Pointer, *types.Interface:
		return true
	}
	return false
}

func isWrapped(ty types.Type) bool {
	switch t := ty.Underlying().(type) {
	case *types.Basic:
		return !is64Bit(t) && t.Kind() != types.UntypedNil
	case *types.Array, *types.Signature:
		return true
	}
	return false
}

func elemType(ty types.Type) types.Type {
	switch t := ty.Underlying().(type) {
	case *types.Array:
		return t.Elem()
	case *types.Slice:
		return t.Elem()
	default:
		panic("")
	}
}

type IsReadyVisitor struct {
	info           *types.Info
	functions      map[types.Object]*ast.FuncDecl
	pendingObjects map[types.Object]bool
	isReady        bool
}

func (v *IsReadyVisitor) Visit(node ast.Node) (w ast.Visitor) {
	if !v.isReady {
		return nil
	}
	switch n := node.(type) {
	case *ast.Ident:
		o := v.info.Objects[n]
		if v.pendingObjects[o] {
			v.isReady = false
			return nil
		}
		if fun, found := v.functions[o]; found {
			delete(v.functions, o)
			ast.Walk(v, fun)
			v.functions[o] = fun
		}
	}
	return v
}