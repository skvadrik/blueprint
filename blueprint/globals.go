package blueprint

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"runtime"
	"strings"
)

type pkg struct {
	fullName  string
	shortName string
	pkgPath   string
	scope     *scope
}

var pkgs = map[string]*pkg{}

var pkgRegexp = regexp.MustCompile(`(.*)\.init(·[0-9]+)?`)

var Phony Rule = &builtinRule{
	name_: "phony",
}

var errRuleIsBuiltin = errors.New("the rule is a built-in")
var errVariableIsArg = errors.New("argument variables have no value")

// We make a Ninja-friendly name out of a Go package name by replaceing all the
// '/' characters with '.'.  We assume the results are unique, though this is
// not 100% guaranteed for Go package names that already contain '.' characters.
// Disallowing package names with '.' isn't reasonable since many package names
// contain the name of the hosting site (e.g. "code.google.com").  In practice
// this probably isn't really a problem.
func pkgPathToName(pkgPath string) string {
	return strings.Replace(pkgPath, "/", ".", -1)
}

// callerPackage returns the pkg of the function that called the caller of
// callerPackage.  The caller of callerPackage must have been called from an
// init function of the package or callerPackage will panic.
//
// Looking for the package's init function on the call stack and using that to
// determine its package name is unfortunately dependent upon Go runtime
// implementation details.  However, it allows us to ensure that it's easy to
// determine where a definition in a .ninja file came from.
func callerPackage() *pkg {
	var pc [1]uintptr
	n := runtime.Callers(3, pc[:])
	if n != 1 {
		panic("unable to get caller pc")
	}

	f := runtime.FuncForPC(pc[0])
	callerName := f.Name()

	submatches := pkgRegexp.FindSubmatch([]byte(callerName))
	if submatches == nil {
		println(callerName)
		panic("not called from an init func")
	}

	pkgPath := string(submatches[1])

	pkgName := pkgPathToName(pkgPath)
	err := validateNinjaName(pkgName)
	if err != nil {
		panic(err)
	}

	i := strings.LastIndex(pkgPath, "/")
	shortName := pkgPath[i+1:]

	p, ok := pkgs[pkgPath]
	if !ok {
		p = &pkg{
			fullName:  pkgName,
			shortName: shortName,
			pkgPath:   pkgPath,
			scope:     newScope(nil),
		}
		pkgs[pkgPath] = p
	}

	return p
}

// Import enables access to the global Ninja rules and variables that are
// exported by another Go package.  It may only be called from a Go package's
// init() function.  The Go package path passed to Import must have already been
// imported into the Go package using a Go import statement.  The imported
// variables may then be accessed from Ninja strings as "${pkg.Variable}", while
// the imported rules can simply be accessed as exported Go variables from the
// package.  For example:
//
//     import (
//         "blueprint"
//         "foo/bar"
//     )
//
//     func init() {
//         blueprint.Import("foo/bar")
//     }
//
//     ...
//
//     func (m *MyModule) GenerateBuildActions(ctx blueprint.Module) {
//         ctx.Build(blueprint.BuildParams{
//             Rule:    bar.SomeRule,
//             Outputs: []string{"${bar.SomeVariable}"},
//         })
//     }
func Import(pkgPath string) {
	callerPkg := callerPackage()

	importPkg, ok := pkgs[pkgPath]
	if !ok {
		panic(fmt.Errorf("package %q has no Blueprints definitions", pkgPath))
	}

	err := callerPkg.scope.AddImport(importPkg.shortName, importPkg.scope)
	if err != nil {
		panic(err)
	}
}

func ImportAs(as, pkgPath string) {
	callerPkg := callerPackage()

	importPkg, ok := pkgs[pkgPath]
	if !ok {
		panic(fmt.Errorf("package %q has no Blueprints definitions", pkgPath))
	}

	err := validateNinjaName(as)
	if err != nil {
		panic(err)
	}

	err = callerPkg.scope.AddImport(as, importPkg.scope)
	if err != nil {
		panic(err)
	}
}

type staticVariable struct {
	pkg_   *pkg
	name_  string
	value_ string
}

// StaticVariable returns a Variable that does not depend on any configuration
// information.
func StaticVariable(name, value string) Variable {
	err := validateNinjaName(name)
	if err != nil {
		panic(err)
	}

	pkg := callerPackage()

	v := &staticVariable{pkg, name, value}
	err = pkg.scope.AddVariable(v)
	if err != nil {
		panic(err)
	}

	return v
}

func (v *staticVariable) pkg() *pkg {
	return v.pkg_
}

func (v *staticVariable) name() string {
	return v.name_
}

func (v *staticVariable) fullName(pkgNames map[*pkg]string) string {
	return packageNamespacePrefix(pkgNames[v.pkg_]) + v.name_
}

func (v *staticVariable) value(interface{}) (*ninjaString, error) {
	return parseNinjaString(v.pkg_.scope, v.value_)
}

type variableFunc struct {
	pkg_   *pkg
	name_  string
	value_ func(interface{}) (string, error)
}

// VariableFunc returns a Variable whose value is determined by a function that
// takes a interface{} object as input and returns either the variable value or an
// error.
func VariableFunc(name string, f func(interface{}) (string, error)) Variable {
	err := validateNinjaName(name)
	if err != nil {
		panic(err)
	}

	pkg := callerPackage()

	v := &variableFunc{pkg, name, f}
	err = pkg.scope.AddVariable(v)
	if err != nil {
		panic(err)
	}

	return v
}

// VariableConfigMethod returns a Variable whose value is determined by calling
// a method on the interface{} object.  The method must take no arguments and return
// a single string that will be the variable's value.
func VariableConfigMethod(name string, method interface{}) Variable {
	err := validateNinjaName(name)
	if err != nil {
		panic(err)
	}

	pkg := callerPackage()

	methodValue := reflect.ValueOf(method)
	validateVariableMethod(name, methodValue)

	fun := func(config interface{}) (string, error) {
		result := methodValue.Call([]reflect.Value{reflect.ValueOf(config)})
		resultStr := result[0].Interface().(string)
		return resultStr, nil
	}

	v := &variableFunc{pkg, name, fun}
	err = pkg.scope.AddVariable(v)
	if err != nil {
		panic(err)
	}

	return v
}

func (v *variableFunc) pkg() *pkg {
	return v.pkg_
}

func (v *variableFunc) name() string {
	return v.name_
}

func (v *variableFunc) fullName(pkgNames map[*pkg]string) string {
	return packageNamespacePrefix(pkgNames[v.pkg_]) + v.name_
}

func (v *variableFunc) value(config interface{}) (*ninjaString, error) {
	value, err := v.value_(config)
	if err != nil {
		return nil, err
	}
	return parseNinjaString(v.pkg_.scope, value)
}

func validateVariableMethod(name string, methodValue reflect.Value) {
	methodType := methodValue.Type()
	if methodType.Kind() != reflect.Func {
		panic(fmt.Errorf("method given for variable %s is not a function",
			name))
	}
	if n := methodType.NumIn(); n != 1 {
		panic(fmt.Errorf("method for variable %s has %d inputs (should be 1)",
			name, n))
	}
	if n := methodType.NumOut(); n != 1 {
		panic(fmt.Errorf("method for variable %s has %d outputs (should be 1)",
			name, n))
	}
	if kind := methodType.Out(0).Kind(); kind != reflect.String {
		panic(fmt.Errorf("method for variable %s does not return a string",
			name))
	}
}

// An argVariable is a Variable that exists only when it is set by a build
// statement to pass a value to the rule being invoked.  It has no value, so it
// can never be used to create a Ninja assignment statement.  It is inserted
// into the rule's scope, which is used for name lookups within the rule and
// when assigning argument values as part of a build statement.
type argVariable struct {
	name_ string
}

func (v *argVariable) pkg() *pkg {
	panic("this should not be called")
}

func (v *argVariable) name() string {
	return v.name_
}

func (v *argVariable) fullName(pkgNames map[*pkg]string) string {
	return v.name_
}

func (v *argVariable) value(config interface{}) (*ninjaString, error) {
	return nil, errVariableIsArg
}

type staticPool struct {
	pkg_   *pkg
	name_  string
	params PoolParams
}

func StaticPool(name string, params PoolParams) Pool {
	err := validateNinjaName(name)
	if err != nil {
		panic(err)
	}

	pkg := callerPackage()

	p := &staticPool{pkg, name, params}
	err = pkg.scope.AddPool(p)
	if err != nil {
		panic(err)
	}

	return p
}

func (p *staticPool) pkg() *pkg {
	return p.pkg_
}

func (p *staticPool) name() string {
	return p.name_
}

func (p *staticPool) fullName(pkgNames map[*pkg]string) string {
	return packageNamespacePrefix(pkgNames[p.pkg_]) + p.name_
}

func (p *staticPool) def(config interface{}) (*poolDef, error) {
	def, err := parsePoolParams(p.pkg_.scope, &p.params)
	if err != nil {
		panic(fmt.Errorf("error parsing PoolParams for %s: %s", p.name_, err))
	}
	return def, nil
}

type poolFunc struct {
	pkg_       *pkg
	name_      string
	paramsFunc func(interface{}) (PoolParams, error)
}

func PoolFunc(name string, f func(interface{}) (PoolParams, error)) Pool {
	err := validateNinjaName(name)
	if err != nil {
		panic(err)
	}

	pkg := callerPackage()

	p := &poolFunc{pkg, name, f}
	err = pkg.scope.AddPool(p)
	if err != nil {
		panic(err)
	}

	return p
}

func (p *poolFunc) pkg() *pkg {
	return p.pkg_
}

func (p *poolFunc) name() string {
	return p.name_
}

func (p *poolFunc) fullName(pkgNames map[*pkg]string) string {
	return packageNamespacePrefix(pkgNames[p.pkg_]) + p.name_
}

func (p *poolFunc) def(config interface{}) (*poolDef, error) {
	params, err := p.paramsFunc(config)
	if err != nil {
		return nil, err
	}
	def, err := parsePoolParams(p.pkg_.scope, &params)
	if err != nil {
		panic(fmt.Errorf("error parsing PoolParams for %s: %s", p.name_, err))
	}
	return def, nil
}

type staticRule struct {
	pkg_     *pkg
	name_    string
	params   RuleParams
	argNames map[string]bool
	scope_   *scope
}

func StaticRule(name string, params RuleParams, argNames ...string) Rule {
	pkg := callerPackage()

	err := validateNinjaName(name)
	if err != nil {
		panic(err)
	}

	err = validateArgNames(argNames)
	if err != nil {
		panic(fmt.Errorf("invalid argument name: %s", err))
	}

	argNamesSet := make(map[string]bool)
	for _, argName := range argNames {
		argNamesSet[argName] = true
	}

	ruleScope := (*scope)(nil) // This will get created lazily

	r := &staticRule{pkg, name, params, argNamesSet, ruleScope}
	err = pkg.scope.AddRule(r)
	if err != nil {
		panic(err)
	}

	return r
}

func (r *staticRule) pkg() *pkg {
	return r.pkg_
}

func (r *staticRule) name() string {
	return r.name_
}

func (r *staticRule) fullName(pkgNames map[*pkg]string) string {
	return packageNamespacePrefix(pkgNames[r.pkg_]) + r.name_
}

func (r *staticRule) def(interface{}) (*ruleDef, error) {
	def, err := parseRuleParams(r.scope(), &r.params)
	if err != nil {
		panic(fmt.Errorf("error parsing RuleParams for %s: %s", r.name_, err))
	}
	return def, nil
}

func (r *staticRule) scope() *scope {
	// We lazily create the scope so that all the global variables get declared
	// before the args are created.  Otherwise we could incorrectly shadow a
	// global variable with an arg variable.
	if r.scope_ == nil {
		r.scope_ = makeRuleScope(r.pkg_.scope, r.argNames)
	}
	return r.scope_
}

func (r *staticRule) isArg(argName string) bool {
	return r.argNames[argName]
}

type ruleFunc struct {
	pkg_       *pkg
	name_      string
	paramsFunc func(interface{}) (RuleParams, error)
	argNames   map[string]bool
	scope_     *scope
}

func RuleFunc(name string, f func(interface{}) (RuleParams, error),
	argNames ...string) Rule {

	pkg := callerPackage()

	err := validateNinjaName(name)
	if err != nil {
		panic(err)
	}

	err = validateArgNames(argNames)
	if err != nil {
		panic(fmt.Errorf("invalid argument name: %s", err))
	}

	argNamesSet := make(map[string]bool)
	for _, argName := range argNames {
		argNamesSet[argName] = true
	}

	ruleScope := (*scope)(nil) // This will get created lazily

	r := &ruleFunc{pkg, name, f, argNamesSet, ruleScope}
	err = pkg.scope.AddRule(r)
	if err != nil {
		panic(err)
	}

	return r
}

func (r *ruleFunc) pkg() *pkg {
	return r.pkg_
}

func (r *ruleFunc) name() string {
	return r.name_
}

func (r *ruleFunc) fullName(pkgNames map[*pkg]string) string {
	return packageNamespacePrefix(pkgNames[r.pkg_]) + r.name_
}

func (r *ruleFunc) def(config interface{}) (*ruleDef, error) {
	params, err := r.paramsFunc(config)
	if err != nil {
		return nil, err
	}
	def, err := parseRuleParams(r.scope(), &params)
	if err != nil {
		panic(fmt.Errorf("error parsing RuleParams for %s: %s", r.name_, err))
	}
	return def, nil
}

func (r *ruleFunc) scope() *scope {
	// We lazily create the scope so that all the global variables get declared
	// before the args are created.  Otherwise we could incorrectly shadow a
	// global variable with an arg variable.
	if r.scope_ == nil {
		r.scope_ = makeRuleScope(r.pkg_.scope, r.argNames)
	}
	return r.scope_
}

func (r *ruleFunc) isArg(argName string) bool {
	return r.argNames[argName]
}

type builtinRule struct {
	name_  string
	scope_ *scope
}

func (r *builtinRule) pkg() *pkg {
	return nil
}

func (r *builtinRule) name() string {
	return r.name_
}

func (r *builtinRule) fullName(pkgNames map[*pkg]string) string {
	return r.name_
}

func (r *builtinRule) def(config interface{}) (*ruleDef, error) {
	return nil, errRuleIsBuiltin
}

func (r *builtinRule) scope() *scope {
	if r.scope_ == nil {
		r.scope_ = makeRuleScope(nil, nil)
	}
	return r.scope_
}

func (r *builtinRule) isArg(argName string) bool {
	return false
}

type ModuleType interface {
	pkg() *pkg
	name() string
	new() (m Module, properties interface{})
}

type moduleTypeFunc struct {
	pkg_  *pkg
	name_ string
	new_  func() (Module, interface{})
}

func MakeModuleType(name string,
	new func() (m Module, properties interface{})) ModuleType {

	pkg := callerPackage()
	return &moduleTypeFunc{pkg, name, new}
}

func (m *moduleTypeFunc) pkg() *pkg {
	return m.pkg_
}

func (m *moduleTypeFunc) name() string {
	return m.pkg_.pkgPath + "." + m.name_
}

func (m *moduleTypeFunc) new() (Module, interface{}) {
	return m.new_()
}