package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/ioutil"
	"os"
	"reflect"
	"strings"
)

const usage = `gointefacegen <type> <interface> <file>

Generates an interface from the type's methods found in the specified file. File must be valid go source. 
If the already interface exists, it is updated in place with the methods found for the type. 
Default behavior prints the resulting file containing the interface to standard out. 

Examples:
gointefacegen somecustomtype somecustominterface src.go
`

type config struct {
	typeName       string
	interfaceName  string
	filename       string
	printInterface bool
	writeToFile    bool
}

func main() {

	printInterfaceFlag := flag.Bool("i", false, "Print only interface to standard out. This takes precedence over -w flag")
	writeFlag := flag.Bool("w", false, "Write result to file instead of stdout")

	flag.Parse()

	if len(flag.Args()) != 3 {
		fmt.Println(usage)
		flag.PrintDefaults()
		return
	}

	c := config{}
	c.typeName = flag.Arg(0)
	c.interfaceName = flag.Arg(1)
	c.filename = flag.Arg(2)
	c.printInterface = *printInterfaceFlag
	c.writeToFile = *writeFlag

	if err := run(c); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(c config) error {

	srcBytes, err := ioutil.ReadFile(c.filename)
	if err != nil {
		return err
	}

	// Format the file first. This allows us to
	// make some assumptions later on
	srcBytes, err = format.Source(srcBytes)
	if err != nil {
		return err
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", srcBytes, parser.ParseComments)
	if err != nil {
		return err
	}

	typeMethods := gatherTypeMethods(c.typeName, file)
	interfaceMethods := generateInterfaceMethods(typeMethods)

	if existing := file.Scope.Lookup(c.interfaceName); existing != nil {
		typ := existing.Decl
		tSpec, ok := typ.(*ast.TypeSpec)
		if !ok {
			return fmt.Errorf("requested interface not of type spec")
		}

		iface, ok := tSpec.Type.(*ast.InterfaceType)
		if !ok {
			return fmt.Errorf("desired interface type name already in use")
		}

		iface.Methods = mergeInterfaceMethods(iface.Methods, interfaceMethods)

		genDecl := findTopLevelGenDeclForTypeSpec(tSpec, file)
		pos, err := firstLineOfTypeIncludingComments(c.interfaceName, file)
		if err != nil {
			return err
		}
		position := fset.Position(pos)
		fmt.Println("POS", position)
		cmap := ast.NewCommentMap(fset, file, file.Comments)
		genDeclIndex := -1
		for i, decl := range file.Decls {
			if decl == genDecl {
				genDeclIndex = i
			}
		}

		if genDeclIndex == -1 {
			return fmt.Errorf("interface declaration is not top level")
		}

		file.Decls = append(file.Decls[:genDeclIndex], file.Decls[genDeclIndex+1:]...)
		file.Comments = cmap.Filter(file).Comments()

		newSrc, err := newSourceByInsertingInterfaceAtLine(genDecl, position.Line, fset, file)
		if err != nil {
			return err
		}

		// parse new source. this feels (and is) grossly
		// inefficient but will suffice for now
		fset = token.NewFileSet()
		file, err = parser.ParseFile(fset, c.filename, newSrc, parser.ParseComments)
		if err != nil {
			return err
		}
	} else {
		decl, _ := newInterface(c.interfaceName, interfaceMethods)
		newSrc, err := newSourceByInsertingInterfaceAboveType(decl, c.typeName, fset, file)
		if err != nil {
			return err
		}

		// parse new source. this feels (and is) grossly
		// inefficient but will suffice for now
		fset = token.NewFileSet()
		file, err = parser.ParseFile(fset, c.filename, newSrc, parser.ParseComments)
		if err != nil {
			return err
		}
	}

	// Print only interface
	if c.printInterface {
		ifaceObj := file.Scope.Lookup(c.interfaceName)
		if ifaceObj == nil {
			return fmt.Errorf("could not find generated interface")
		}

		typ := ifaceObj.Decl
		tSpec, ok := typ.(*ast.TypeSpec)
		if !ok {
			return fmt.Errorf("unexpected generated interface type")
		}

		decl := findTopLevelGenDeclForTypeSpec(tSpec, file)
		if decl == nil {
			return fmt.Errorf("could not find generated interface declaration")
		}

		var iSrcBuff bytes.Buffer
		err = format.Node(&iSrcBuff, fset, decl)
		if err != nil {
			return err
		}

		fmt.Println(iSrcBuff.String())
		return nil
	}

	// Generate new source
	var newSrcBuff bytes.Buffer
	err = format.Node(&newSrcBuff, fset, file)
	if err != nil {
		return err
	}

	// Write it to file
	if c.writeToFile {
		return ioutil.WriteFile(c.filename, newSrcBuff.Bytes(), 0)
	}

	// or print it out
	fmt.Print(newSrcBuff.String())

	return nil
}

// newSourceByInsertingInterfaceAboveType generates new sourcecode by inserting the interface above the specified type (or the type's comments)
func newSourceByInsertingInterfaceAboveType(interfaceDecl *ast.GenDecl, aboveType string, fset *token.FileSet, file *ast.File) (string, error) {
	pos, err := firstLineOfTypeIncludingComments(aboveType, file)
	if err != nil {
		return "", err
	}

	position := fset.Position(pos)
	return newSourceByInsertingInterfaceAtLine(interfaceDecl, position.Line, fset, file)
}

// newSourceByInsertingInterfaceAtLine generates new sourcecode by inserting the interface at the specified line
//
// *** here be the dragons *** Ideally, we insert the interface declaration node (and it's children) into the ast. Unforuntately,
// handling comments properly when inserting nodes into the ast is hard. Just inserting the node naively produces some funky results.
// To avoid all of the headaches associated with that we convert the source into a slice of lines, insert the interface at the proper
// location and then generate a new source string for the caller to use and parse again if need be.
func newSourceByInsertingInterfaceAtLine(interfaceDecl *ast.GenDecl, line int, fset *token.FileSet, file *ast.File) (string, error) {

	// Format input file and render to a string
	var orig bytes.Buffer
	err := format.Node(&orig, fset, file)
	if err != nil {
		return "", err
	}
	origSrc := orig.String()

	// Split into lines
	lines := strings.Split(origSrc, "\n")

	// convert to index
	lineIndex := line - 1

	// Render our interface into a string
	var iBuf bytes.Buffer
	err = format.Node(&iBuf, fset, interfaceDecl)
	if err != nil {
		return "", err
	}
	iSrc := iBuf.String() + "\n"

	if lineIndex > len(lines) { // this should never happen in theory
		lines = append(lines, iSrc)
	} else {
		lines = append(lines[:lineIndex], append([]string{iSrc}, lines[lineIndex:]...)...)
	}

	newSrc := strings.Join(lines, "\n")

	return newSrc, nil
}

// firstLineOfTypeIncludingComments returns the first line of the type including its comments.
// for example, given the following type declaration
//
// 1: // comment
// 2: // comment
// 3: type test string
//
// a token.Pos for line 1 would be returned
func firstLineOfTypeIncludingComments(typeName string, file *ast.File) (token.Pos, error) {
	// Find the object for the type
	typeObj := file.Scope.Lookup(typeName)
	if typeObj == nil || typeObj.Pos().IsValid() == false {
		return token.NoPos, fmt.Errorf("invalid type")
	}

	// Make sure it's a type
	typeSpec, ok := typeObj.Decl.(*ast.TypeSpec)
	if !ok {
		return token.NoPos, fmt.Errorf("expected a type spec but received %v", reflect.TypeOf(typeObj.Decl))
	}

	// Find the ast.GenDecl for the type. We do this because
	// doc comments for a type are associated with the ast.GenDecl for the type
	genDecl := findTopLevelGenDeclForTypeSpec(typeSpec, file)
	if genDecl == nil {
		return token.NoPos, fmt.Errorf("could not find GenDecl for type")
	}

	// The position to insert at is either the line at which type occurs (ast.GenDecl)
	// or the first line of the comments above the type declaration
	pos := genDecl.Pos()
	if genDecl.Doc != nil {
		pos = genDecl.Doc.Pos()
	}

	return pos, nil
}

// Find the top level ast.GenDecl for the given ast.TypeSpec
func findTopLevelGenDeclForTypeSpec(typeSpec *ast.TypeSpec, file *ast.File) *ast.GenDecl {
	var genDecl *ast.GenDecl
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok {
			if gen.Specs[0] == typeSpec {
				genDecl = gen
			}
		}
	}

	return genDecl
}

// gatherTypeMethods returns all of the *ast.FuncDecl for a given type
func gatherTypeMethods(typeName string, file *ast.File) []*ast.FuncDecl {
	methods := []*ast.FuncDecl{}
	ast.Inspect(file, func(x ast.Node) bool {
		f, ok := x.(*ast.FuncDecl)
		if !ok {
			return true
		}

		if f.Recv == nil { //function
			return false
		}

		if len(f.Recv.List) != 1 {
			return false // this should never happen, there should only be one receiver
		}

		typ := f.Recv.List[0].Type
		ident, ok := typ.(*ast.Ident)
		if !ok {
			return false
		}

		if typeName == ident.String() {
			methods = append(methods, f)
		}

		return false
	})

	return methods
}

// generateInterfaceMethods generates a ast.FieldList suitable for use of as the Methods of an ast.InterfaceType
func generateInterfaceMethods(funcDecls []*ast.FuncDecl) *ast.FieldList {
	fl := &ast.FieldList{}

	for _, decl := range funcDecls {
		field := &ast.Field{}
		name := dupIdent(decl.Name)
		name.Obj = ast.NewObj(ast.Fun, name.Name) // a FuncDecl's name doesn't have an object but a field's name does
		name.Obj.Decl = field
		field.Names = append(field.Names, name)

		funcType := dupFuncType(decl.Type)

		// erase the names of any named returns
		// since they don't really make
		// sense for interfaces
		if funcType.Results != nil {
			for _, r := range funcType.Results.List {
				r.Names = nil
			}
		}

		field.Type = funcType
		fl.List = append(fl.List, field)
	}

	return fl
}

// mergeInterfaceMethods merges two FieldLists of interface methods
// into a new FieldList. If a method with the same name exists
// in both FieldLists, the right one wins.
//
func mergeInterfaceMethods(left, right *ast.FieldList) *ast.FieldList {
	new := &ast.FieldList{}
	names := make(map[string]bool)
	for _, field := range right.List {
		if len(field.Names) == 0 { // shouldn't happen
			continue
		}

		names[field.Names[0].Name] = true
		new.List = append(new.List, field)
	}

	for _, field := range left.List {
		if len(field.Names) == 0 { // shouldn't happen
			continue
		}

		if names[field.Names[0].Name] == false {
			new.List = append(new.List, field)
		}
	}

	return new
}

func newInterface(name string, methods *ast.FieldList) (*ast.GenDecl, *ast.TypeSpec) {

	// given:
	//
	// type someInterface interface {
	//     MethodOne()
	//     MethodTwo()
	// }
	//

	// type
	decl := &ast.GenDecl{Tok: token.TYPE}

	//  someInterface
	tSpec := &ast.TypeSpec{}
	tSpec.Name = &ast.Ident{Name: name}
	tSpec.Name.Obj = ast.NewObj(ast.Typ, name)
	tSpec.Name.Obj.Decl = tSpec

	decl.Specs = []ast.Spec{tSpec}

	// interface {
	//     MethodOne()
	//     MethodTwo()
	// }
	iType := &ast.InterfaceType{
		Methods: methods,
	}

	tSpec.Type = iType

	return decl, tSpec
}

func dupFuncType(old *ast.FuncType) *ast.FuncType {
	if old == nil {
		return nil
	}

	new := &ast.FuncType{}
	new.Params = dupFieldList(old.Params)
	new.Results = dupFieldList(old.Results)

	return new
}

func dupFieldList(old *ast.FieldList) *ast.FieldList {
	if old == nil {
		return nil
	}

	new := &ast.FieldList{}

	for _, oldField := range old.List {
		new.List = append(new.List, dupField(oldField))
	}

	return new
}

// dupField duplicates an ast.Field ignoring position information.
// this is written specifically for copying fields that are
// a part of an ast.InterfaceType's Method list or a
// ast.FuncType's Params and Results
func dupField(old *ast.Field) *ast.Field {
	if old == nil {
		return nil
	}

	new := &ast.Field{}

	switch t := old.Type.(type) {
	case *ast.Ident:
		new.Type = dupIdent(t)
	case *ast.FuncType:
		new.Type = dupFuncType(t)
	default:
		fmt.Println("unsuporrted field type")
	}

	for _, oldName := range old.Names {
		newName := dupIdent(oldName)
		newName.Obj.Decl = new
		new.Names = append(new.Names, newName)
	}

	return new
}

// dupIdent duplicates an ast.Ident ignoring position information
func dupIdent(old *ast.Ident) *ast.Ident {
	if old == nil {
		return nil
	}

	new := ast.NewIdent(old.Name)
	new.Obj = dupObject(old.Obj)

	return new
}

func dupObject(old *ast.Object) *ast.Object {
	if old == nil {
		return nil
	}

	return ast.NewObj(old.Kind, old.Name)
}
