package gen

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"io"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const pkgWriter = "github.com/CosmWasm/tinyjson/jwriter"
const pkgLexer = "github.com/CosmWasm/tinyjson/jlexer"
const pkgEasyJSON = "github.com/CosmWasm/tinyjson"

// FieldNamer defines a policy for generating names for struct fields.
type FieldNamer interface {
	GetJSONFieldName(t reflect.Type, f reflect.StructField) string
}

// Generator generates the requested marshaler/unmarshalers.
type Generator struct {
	out *bytes.Buffer

	pkgName    string
	pkgPath    string
	buildTags  string
	hashString string

	varCounter int

	noStdMarshalers          bool
	omitEmpty                bool
	disallowUnknownFields    bool
	fieldNamer               FieldNamer
	simpleBytes              bool
	skipMemberNameUnescaping bool

	// package path to local alias map for tracking imports
	imports map[string]string

	// types that marshalers were requested for by user
	marshalers map[reflect.Type]bool

	// types that encoders were already generated for
	typesSeen map[reflect.Type]bool

	// types that encoders were requested for (e.g. by encoders of other types)
	typesUnseen []reflect.Type

	// function name to relevant type maps to track names of de-/encoders in
	// case of a name clash or unnamed structs
	functionNames map[string]reflect.Type
}

// NewGenerator initializes and returns a Generator.
func NewGenerator(filename string) *Generator {
	ret := &Generator{
		imports: map[string]string{
			pkgWriter:   "jwriter",
			pkgLexer:    "jlexer",
			pkgEasyJSON: "tinyjson",
		},
		fieldNamer:    DefaultFieldNamer{},
		marshalers:    make(map[reflect.Type]bool),
		typesSeen:     make(map[reflect.Type]bool),
		functionNames: make(map[string]reflect.Type),
	}

	// Use a file-unique prefix on all auxiliary funcs to avoid
	// name clashes.
	hash := fnv.New32()
	hash.Write([]byte(filename))
	ret.hashString = fmt.Sprintf("%x", hash.Sum32())

	return ret
}

// SetPkg sets the name and path of output package.
func (g *Generator) SetPkg(name, path string) {
	g.pkgName = name
	g.pkgPath = path
}

// SetBuildTags sets build tags for the output file.
func (g *Generator) SetBuildTags(tags string) {
	g.buildTags = tags
}

// SetFieldNamer sets field naming strategy.
func (g *Generator) SetFieldNamer(n FieldNamer) {
	g.fieldNamer = n
}

// UseSnakeCase sets snake_case field naming strategy.
func (g *Generator) UseSnakeCase() {
	g.fieldNamer = SnakeCaseFieldNamer{}
}

// UseLowerCamelCase sets lowerCamelCase field naming strategy.
func (g *Generator) UseLowerCamelCase() {
	g.fieldNamer = LowerCamelCaseFieldNamer{}
}

// NoStdMarshalers instructs not to generate standard MarshalJSON/UnmarshalJSON
// methods (only the custom interface).
func (g *Generator) NoStdMarshalers() {
	g.noStdMarshalers = true
}

// DisallowUnknownFields instructs not to skip unknown fields in json and return error.
func (g *Generator) DisallowUnknownFields() {
	g.disallowUnknownFields = true
}

// SkipMemberNameUnescaping instructs to skip member names unescaping to improve performance
func (g *Generator) SkipMemberNameUnescaping() {
	g.skipMemberNameUnescaping = true
}

// OmitEmpty triggers `json=",omitempty"` behaviour by default.
func (g *Generator) OmitEmpty() {
	g.omitEmpty = true
}

// SimpleBytes triggers generate output bytes as slice byte
func (g *Generator) SimpleBytes() {
	g.simpleBytes = true
}

// addTypes requests to generate encoding/decoding funcs for the given type.
func (g *Generator) addType(t reflect.Type) {
	if g.typesSeen[t] {
		return
	}
	for _, t1 := range g.typesUnseen {
		if t1 == t {
			return
		}
	}
	g.typesUnseen = append(g.typesUnseen, t)
}

// Add requests to generate marshaler/unmarshalers and encoding/decoding
// funcs for the type of given object.
func (g *Generator) Add(obj interface{}) {
	t := reflect.TypeOf(obj)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	g.addType(t)
	g.marshalers[t] = true
}

// printHeader prints package declaration and imports.
func (g *Generator) printHeader() {
	if g.buildTags != "" {
		fmt.Println("// +build ", g.buildTags)
		fmt.Println()
	}
	fmt.Println("// Code generated by tinyjson for marshaling/unmarshaling. DO NOT EDIT.")
	fmt.Println()
	fmt.Println("package ", g.pkgName)
	fmt.Println()

	byAlias := make(map[string]string, len(g.imports))
	aliases := make([]string, 0, len(g.imports))

	for path, alias := range g.imports {
		aliases = append(aliases, alias)
		byAlias[alias] = path
	}

	sort.Strings(aliases)
	fmt.Println("import (")
	for _, alias := range aliases {
		fmt.Printf("  %s %q\n", alias, byAlias[alias])
	}

	fmt.Println(")")
	fmt.Println("")
	fmt.Println("// suppress unused package warning")
	fmt.Println("var (")
	fmt.Println("   _ *jlexer.Lexer")
	fmt.Println("   _ *jwriter.Writer")
	fmt.Println("   _ tinyjson.Marshaler")
	fmt.Println(")")

	fmt.Println()
}

// Run runs the generator and outputs generated code to out.
func (g *Generator) Run(out io.Writer) error {
	g.out = &bytes.Buffer{}

	for len(g.typesUnseen) > 0 {
		t := g.typesUnseen[len(g.typesUnseen)-1]
		g.typesUnseen = g.typesUnseen[:len(g.typesUnseen)-1]
		g.typesSeen[t] = true

		if err := g.genDecoder(t); err != nil {
			return err
		}
		if err := g.genEncoder(t); err != nil {
			return err
		}

		if !g.marshalers[t] {
			continue
		}

		if err := g.genStructMarshaler(t); err != nil {
			return err
		}
		if err := g.genStructUnmarshaler(t); err != nil {
			return err
		}
	}
	g.printHeader()
	_, err := out.Write(g.out.Bytes())
	return err
}

// fixes vendored paths
func fixPkgPathVendoring(pkgPath string) string {
	const vendor = "/vendor/"
	if i := strings.LastIndex(pkgPath, vendor); i != -1 {
		return pkgPath[i+len(vendor):]
	}
	return pkgPath
}

func fixAliasName(alias string) string {
	alias = strings.Replace(
		strings.Replace(alias, ".", "_", -1),
		"-",
		"_",
		-1,
	)

	if alias[0] == 'v' { // to void conflicting with var names, say v1
		alias = "_" + alias
	}
	return alias
}

// pkgAlias creates and returns and import alias for a given package.
func (g *Generator) pkgAlias(pkgPath string) string {
	pkgPath = fixPkgPathVendoring(pkgPath)
	if alias := g.imports[pkgPath]; alias != "" {
		return alias
	}

	for i := 0; ; i++ {
		alias := fixAliasName(path.Base(pkgPath))
		if i > 0 {
			alias += fmt.Sprint(i)
		}

		exists := false
		for _, v := range g.imports {
			if v == alias {
				exists = true
				break
			}
		}

		if !exists {
			g.imports[pkgPath] = alias
			return alias
		}
	}
}

// getType return the textual type name of given type that can be used in generated code.
func (g *Generator) getType(t reflect.Type) string {
	if t.Name() == "" {
		switch t.Kind() {
		case reflect.Ptr:
			return "*" + g.getType(t.Elem())
		case reflect.Slice:
			return "[]" + g.getType(t.Elem())
		case reflect.Array:
			return "[" + strconv.Itoa(t.Len()) + "]" + g.getType(t.Elem())
		case reflect.Map:
			return "map[" + g.getType(t.Key()) + "]" + g.getType(t.Elem())
		}
	}

	if t.Name() == "" || t.PkgPath() == "" {
		if t.Kind() == reflect.Struct {
			// the fields of an anonymous struct can have named types,
			// and t.String() will not be sufficient because it does not
			// remove the package name when it matches g.pkgPath.
			// so we convert by hand
			nf := t.NumField()
			lines := make([]string, 0, nf)
			for i := 0; i < nf; i++ {
				f := t.Field(i)
				var line string
				if !f.Anonymous {
					line = f.Name + " "
				} // else the field is anonymous (an embedded type)
				line += g.getType(f.Type)
				t := f.Tag
				if t != "" {
					line += " " + escapeTag(t)
				}
				lines = append(lines, line)
			}
			return strings.Join([]string{"struct { ", strings.Join(lines, "; "), " }"}, "")
		}
		return t.String()
	} else if t.PkgPath() == g.pkgPath {
		return t.Name()
	}
	return g.pkgAlias(t.PkgPath()) + "." + t.Name()
}

// escape a struct field tag string back to source code
func escapeTag(tag reflect.StructTag) string {
	t := string(tag)
	if strings.ContainsRune(t, '`') {
		// there are ` in the string; we can't use ` to enclose the string
		return strconv.Quote(t)
	}
	return "`" + t + "`"
}

// uniqueVarName returns a file-unique name that can be used for generated variables.
func (g *Generator) uniqueVarName() string {
	g.varCounter++
	return fmt.Sprint("v", g.varCounter)
}

// safeName escapes unsafe characters in pkg/type name and returns a string that can be used
// in encoder/decoder names for the type.
func (g *Generator) safeName(t reflect.Type) string {
	name := t.PkgPath()
	if t.Name() == "" {
		name += "anonymous"
	} else {
		name += "." + t.Name()
	}

	parts := []string{}
	part := []rune{}
	for _, c := range name {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			part = append(part, c)
		} else if len(part) > 0 {
			parts = append(parts, string(part))
			part = []rune{}
		}
	}
	return joinFunctionNameParts(false, parts...)
}

// functionName returns a function name for a given type with a given prefix. If a function
// with this prefix already exists for a type, it is returned.
//
// Method is used to track encoder/decoder names for the type.
func (g *Generator) functionName(prefix string, t reflect.Type) string {
	prefix = joinFunctionNameParts(true, "tinyjson", g.hashString, prefix)
	name := joinFunctionNameParts(true, prefix, g.safeName(t))

	// Most of the names will be unique, try a shortcut first.
	if e, ok := g.functionNames[name]; !ok || e == t {
		g.functionNames[name] = t
		return name
	}

	// Search if the function already exists.
	for name1, t1 := range g.functionNames {
		if t1 == t && strings.HasPrefix(name1, prefix) {
			return name1
		}
	}

	// Create a new name in the case of a clash.
	for i := 1; ; i++ {
		nm := fmt.Sprint(name, i)
		if _, ok := g.functionNames[nm]; ok {
			continue
		}
		g.functionNames[nm] = t
		return nm
	}
}

// DefaultFieldsNamer implements trivial naming policy equivalent to encoding/json.
type DefaultFieldNamer struct{}

func (DefaultFieldNamer) GetJSONFieldName(t reflect.Type, f reflect.StructField) string {
	jsonName := strings.Split(f.Tag.Get("json"), ",")[0]
	if jsonName != "" {
		return jsonName
	}

	return f.Name
}

// LowerCamelCaseFieldNamer
type LowerCamelCaseFieldNamer struct{}

func isLower(b byte) bool {
	return b <= 122 && b >= 97
}

func isUpper(b byte) bool {
	return b >= 65 && b <= 90
}

// convert HTTPRestClient to httpRestClient
func lowerFirst(s string) string {
	if s == "" {
		return ""
	}

	str := ""
	strlen := len(s)

	/**
	  Loop each char
	  If is uppercase:
	    If is first char, LOWER it
	    If the following char is lower, LEAVE it
	    If the following char is upper OR numeric, LOWER it
	    If is the end of string, LEAVE it
	  Else lowercase
	*/

	foundLower := false
	for i := range s {
		ch := s[i]
		if isUpper(ch) {
			switch {
			case i == 0:
				str += string(ch + 32)
			case !foundLower: // Currently just a stream of capitals, eg JSONRESTS[erver]
				if strlen > (i+1) && isLower(s[i+1]) {
					// Next char is lower, keep this a capital
					str += string(ch)
				} else {
					// Either at end of string or next char is capital
					str += string(ch + 32)
				}
			default:
				str += string(ch)
			}
		} else {
			foundLower = true
			str += string(ch)
		}
	}

	return str
}

func (LowerCamelCaseFieldNamer) GetJSONFieldName(t reflect.Type, f reflect.StructField) string {
	jsonName := strings.Split(f.Tag.Get("json"), ",")[0]
	if jsonName != "" {
		return jsonName
	}

	return lowerFirst(f.Name)
}

// SnakeCaseFieldNamer implements CamelCase to snake_case conversion for fields names.
type SnakeCaseFieldNamer struct{}

func camelToSnake(name string) string {
	var ret bytes.Buffer

	multipleUpper := false
	var lastUpper rune
	var beforeUpper rune

	for _, c := range name {
		// Non-lowercase character after uppercase is considered to be uppercase too.
		isUpper := (unicode.IsUpper(c) || (lastUpper != 0 && !unicode.IsLower(c)))

		if lastUpper != 0 {
			// Output a delimiter if last character was either the first uppercase character
			// in a row, or the last one in a row (e.g. 'S' in "HTTPServer").
			// Do not output a delimiter at the beginning of the name.

			firstInRow := !multipleUpper
			lastInRow := !isUpper

			if ret.Len() > 0 && (firstInRow || lastInRow) && beforeUpper != '_' {
				ret.WriteByte('_')
			}
			ret.WriteRune(unicode.ToLower(lastUpper))
		}

		// Buffer uppercase char, do not output it yet as a delimiter may be required if the
		// next character is lowercase.
		if isUpper {
			multipleUpper = (lastUpper != 0)
			lastUpper = c
			continue
		}

		ret.WriteRune(c)
		lastUpper = 0
		beforeUpper = c
		multipleUpper = false
	}

	if lastUpper != 0 {
		ret.WriteRune(unicode.ToLower(lastUpper))
	}
	return string(ret.Bytes())
}

func (SnakeCaseFieldNamer) GetJSONFieldName(t reflect.Type, f reflect.StructField) string {
	jsonName := strings.Split(f.Tag.Get("json"), ",")[0]
	if jsonName != "" {
		return jsonName
	}

	return camelToSnake(f.Name)
}

func joinFunctionNameParts(keepFirst bool, parts ...string) string {
	buf := bytes.NewBufferString("")
	for i, part := range parts {
		if i == 0 && keepFirst {
			buf.WriteString(part)
		} else {
			if len(part) > 0 {
				buf.WriteString(strings.ToUpper(string(part[0])))
			}
			if len(part) > 1 {
				buf.WriteString(part[1:])
			}
		}
	}
	return buf.String()
}
