package fuzzgen

import (
	"fmt"
	"math/rand/v2"
)

// hazardBlankImports exercises blank-import handling on both ingest and
// emit's import-preservation path.
func hazardBlankImports(_ *rand.Rand, m *SyntheticModule) {
	src := "package blankimp\n\nimport _ \"sort\"\n\nfunc UsesNothing() int {\n\treturn 1\n}\n"
	m.AddFile("pkg/blankimp/blank.go", src)
}

// hazardCollidingBasenames reproduces the emitModule basename-collision
// bug (v0.26.23): N>=2 different packages, each containing a file with
// the exact same basename ("types.go"), whose definitions must not be
// merged or dropped on emit.
func hazardCollidingBasenames(r *rand.Rand, m *SyntheticModule) {
	n := 2 + r.IntN(2) // 2 or 3 colliding packages
	for i := 0; i < n; i++ {
		pkg := fmt.Sprintf("collide%d", i)
		typeName := fmt.Sprintf("Thing%d", i)
		src := fmt.Sprintf("package %s\n\ntype %s struct {\n\tX int\n}\n\nfunc New%s() %s {\n\treturn %s{X: %d}\n}\n",
			pkg, typeName, typeName, typeName, typeName, i)
		m.AddFile(fmt.Sprintf("pkg/%s/types.go", pkg), src)
	}
}

// hazardEmbeddedFields exercises ingestStructFields' handling of an
// anonymous (embedded) struct field.
func hazardEmbeddedFields(_ *rand.Rand, m *SyntheticModule) {
	src := "package embedded\n\ntype Base struct {\n\tID int\n}\n\ntype Derived struct {\n\tBase\n\tName string\n}\n"
	m.AddFile("pkg/embedded/types.go", src)
}

// hazardFloatingComments exercises the "all comments are preserved"
// invariant: a comment that binds to neither the preceding nor following
// declaration.
func hazardFloatingComments(_ *rand.Rand, m *SyntheticModule) {
	src := "package comments\n\n// FuncOne does a thing.\nfunc FuncOne() {}\n\n// a floating comment between declarations, attached to neither\n\n// FuncTwo does another thing.\nfunc FuncTwo() {}\n"
	m.AddFile("pkg/comments/funcs.go", src)
}

// hazardGoEmbedDirective exercises ingestEmbedFiles: a real //go:embed
// directive plus the small file it embeds.
func hazardGoEmbedDirective(_ *rand.Rand, m *SyntheticModule) {
	m.AddFile("pkg/embedfile/hello.txt", "hello\n")
	m.AddFile("pkg/embedfile/data.go", "package embedfile\n\nimport _ \"embed\"\n\n//go:embed hello.txt\nvar Hello string\n")
}

// hazardGroupedConstIota exercises emit's dedicated grouped-const/iota
// merge logic (mergeDeclsIntoSource / bodyIsGroupedGenDecl).
func hazardGroupedConstIota(_ *rand.Rand, m *SyntheticModule) {
	src := "package constgrp\n\ntype Level int\n\nconst (\n\tLevelLow Level = iota\n\tLevelMedium\n\tLevelHigh\n)\n"
	m.AddFile("pkg/constgrp/level.go", src)
}

// hazardMultiPackage adds a couple of unrelated leaf packages so the
// module always spans more than main+one package -- the precondition for
// hazard #1's basename collision to even be possible in a real repo.
func hazardMultiPackage(r *rand.Rand, m *SyntheticModule) {
	n := 1 + r.IntN(2) // 1-2 extra packages
	for i := 0; i < n; i++ {
		pkg := fmt.Sprintf("leaf%d", i)
		src := fmt.Sprintf("package %s\n\nfunc Leaf%d() int {\n\treturn %d\n}\n", pkg, i, i)
		m.AddFile(fmt.Sprintf("pkg/%s/leaf.go", pkg), src)
	}
}

// hazardScatteredInit reproduces the ingest initCounter keying bug
// (v0.26.26): N>=2 files in ONE package each declare their own func
// init(), which must resolve to a stable identity regardless of whether
// the caller ingests the whole module at once or one file at a time.
func hazardScatteredInit(r *rand.Rand, m *SyntheticModule) {
	n := 2 + r.IntN(3) // 2-4 files
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("package initpkg\n\nvar registered%d bool\n\nfunc init() {\n\tregistered%d = true\n}\n", i, i)
		m.AddFile(fmt.Sprintf("pkg/initpkg/file%d.go", i), src)
	}
}

// collisionKind is a Go-legal mechanism by which two unrelated
// declarations share the exact same bare name within one package --
// the precondition for lookupFuncDefID's #372 bare-name lookup to ever
// be ambiguous in the first place. This is the ONLY enumeration axis
// this family varies. An earlier version of this hazard also enumerated
// a "refSite" axis (calling Target from the body/error-branch/loop/
// return of the colliding declaration) -- dropped after reading
// resolve.go closely: lookupFuncDefID computes fromID ONCE per
// ast.FuncDecl (line ~515), then collectRefs walks the entire body via
// a position-agnostic ast.Inspect and attributes EVERY reference found
// inside -- regardless of where it sits -- to that single fromID
// (line ~540). astRename's rewrite (server.go's astRename) is the same
// blanket ast.Inspect. So whatever's inside the body, and wherever it
// sits, is provably irrelevant to whether this bug class reproduces;
// only WHICH declaration the bug misattributes the reference to
// (collisionKind) matters. The 4 refSite variants this produced added
// zero marginal detection power for 4x the runtime -- see docs/
// lessons-learned.md's "pareto horizon" note before adding a similar
// axis to another hazard family.
//
// A same-name-across-files collision gated by mutually exclusive build
// tags was also considered and dropped: go/packages.Load (which
// internal/ingest uses) applies the host's default build constraints
// when loading, so the tag-excluded file is never parsed at all (see
// ingest.go's ingestPackage, "go/packages.Load still returns a
// *packages.Package for directories whose Go files are all excluded..."
// comment) -- a naive version would only ever ingest ONE of the two
// same-named declarations, never actually colliding.
type collisionKind int

const (
	collisionInitMulti collisionKind = iota
	collisionMethodVsFunction
)

func (c collisionKind) name() string {
	switch c {
	case collisionInitMulti:
		return "init_multi"
	case collisionMethodVsFunction:
		return "method_vs_function"
	default:
		panic("fuzzgen: unknown collisionKind")
	}
}

// apply materializes this collision kind's file set: one or more
// unrelated declarations sharing the SAME bare name via a Go-legal
// mechanism, with exactly one of them calling Target.
func (c collisionKind) apply(pkg string, r *rand.Rand, m *SyntheticModule) {
	switch c {
	case collisionInitMulti:
		// N>=2 files in ONE package each declare their own unrelated
		// func init(); one further file's init() calls Target.
		// Reproduces #372 directly: lookupFuncDefID resolved callers by
		// bare name only, so a reference made from inside one file's
		// init() could be attributed to a DIFFERENT file's unrelated
		// same-named init().
		n := 2 + r.IntN(3) // 2-4 unrelated init() files
		m.AddFile(fmt.Sprintf("pkg/%s/caller.go", pkg), fmt.Sprintf("package %s\n\nfunc init() {\n\t_ = Target()\n}\n", pkg))
		for i := 0; i < n; i++ {
			src := fmt.Sprintf("package %s\n\nvar unrelated%d bool\n\nfunc init() {\n\tunrelated%d = true\n}\n", pkg, i, i)
			m.AddFile(fmt.Sprintf("pkg/%s/unrelated%d.go", pkg, i), src)
		}
	case collisionMethodVsFunction:
		// A package-level function and a METHOD share the same bare
		// name, declared in DIFFERENT files -- a method can never
		// actually collide with a package-level function of the same
		// name (the receiver already distinguishes it), but
		// hazardMethodNamedInit (#354) found defn's own ingest counter
		// conflating the two anyway, in the SAME file. This generalizes
		// the same identity-confusion class cross-file and to an
		// ordinary exported name, at the caller-attribution layer #372
		// fixed rather than the ingest-counter layer #354 fixed.
		m.AddFile(fmt.Sprintf("pkg/%s/service.go", pkg), fmt.Sprintf("package %s\n\ntype Service struct{}\n\nfunc (s *Service) Run() error {\n\treturn nil\n}\n", pkg))
		m.AddFile(fmt.Sprintf("pkg/%s/run.go", pkg), fmt.Sprintf("package %s\n\nfunc Run() error {\n\treturn Target()\n}\n", pkg))
	default:
		panic("fuzzgen: unknown collisionKind")
	}
}

// crossCallHazard builds one member of the #372 hazard family: a
// Go-legal same-bare-name collision mechanism (collision) in which one
// of the colliding declarations calls a function (Target) defined
// elsewhere in the package.
func crossCallHazard(collision collisionKind) Hazard {
	pkg := "crosscall_" + collision.name()
	return Hazard{
		Name: "cross_call_" + collision.name(),
		Apply: func(r *rand.Rand, m *SyntheticModule) {
			m.AddFile(fmt.Sprintf("pkg/%s/target.go", pkg), fmt.Sprintf("package %s\n\nfunc Target() error {\n\treturn nil\n}\n", pkg))
			collision.apply(pkg, r, m)
		},
	}
}

// CrossCallHazards is every collisionKind enumerated by crossCallHazard
// -- the full #372 hazard family, exported so tests can drive a
// deliberate rename of Target through each variant individually
// (TestMutationSequence_Hazards's random pickMutation would only ever
// hit one of these by chance).
var CrossCallHazards = buildCrossCallHazards()

func buildCrossCallHazards() []Hazard {
	var hazards []Hazard
	for _, collision := range []collisionKind{collisionInitMulti, collisionMethodVsFunction} {
		hazards = append(hazards, crossCallHazard(collision))
	}
	return hazards
}

// hazardMethodOnDifferentTypesSameMethodName puts TWO distinct types in
// the same package, each with a method of the SAME name (a very common,
// always-legal Go shape -- e.g. two types both implementing the same
// interface method). Distinct from hazardMethodNamedInit/#354's
// method-vs-FUNCTION collision and collisionMethodVsFunction's
// cross-file variant: this is method-vs-METHOD, the shape that
// actually dominates real Go code (any two types satisfying the same
// interface), and was previously untested -- every existing hazard
// with same-named methods keeps them on the SAME type.
func hazardMethodOnDifferentTypesSameMethodName(_ *rand.Rand, m *SyntheticModule) {
	src := `package methodclash

import "errors"

var errInvalid = errors.New("invalid")

type Widget struct{ N int }

func (w *Widget) Validate() error {
	if w.N < 0 {
		return errInvalid
	}
	return nil
}

type Gadget struct{ N int }

func (g *Gadget) Validate() error {
	if g.N > 100 {
		return errInvalid
	}
	return nil
}
`
	m.AddFile("pkg/methodclash/types.go", src)
}

// hazardSameFieldNameAcrossTypes puts TWO distinct struct types in the
// same package, each with a field of the SAME name (e.g. two unrelated
// configs both having a "Name" field) -- always legal (field namespace
// is per-type), always common, and untested until now: every existing
// field-collision hazard (hazardFieldNamedAfterOwnType, #352) collides
// a field name against a TYPE name, never against another type's field
// of the same name.
func hazardSameFieldNameAcrossTypes(_ *rand.Rand, m *SyntheticModule) {
	src := `package fieldclash

type Person struct {
	Name string
}

type Company struct {
	Name string
}

func Describe(p Person, c Company) string {
	return p.Name + " works at " + c.Name
}
`
	m.AddFile("pkg/fieldclash/types.go", src)
}

// hazardPromotedFieldShadowing exercises Go's field-shadowing rule: an
// embedded type's field is "promoted" onto the embedding type UNLESS
// the embedding type declares its own field of the same name, in which
// case the direct field wins and the promoted one is only reachable via
// the embedded type's own name (d.Base.Label, not just d.Label).
// hazardEmbeddedFields already covers plain (non-colliding) embedding;
// this is the identity-confusion variant -- untested until now.
func hazardPromotedFieldShadowing(_ *rand.Rand, m *SyntheticModule) {
	src := `package shadowfield

type Base struct {
	Label string
}

type Derived struct {
	Base
	Label string
}

func Describe(d Derived) string {
	return d.Label + "/" + d.Base.Label
}
`
	m.AddFile("pkg/shadowfield/types.go", src)
}

// hazardGenericTypeParamShadowsNamedType exercises a generic function
// whose type parameter identifier is the SAME bare name as an unrelated
// package-level named type -- legal Go (the type parameter shadows the
// named type within the generic func's own scope), and exactly the
// "type-overwriting" mutation class Hephaestus (PLDI 2022) targets for
// generics/type-checker fuzzing. The lit review in this doc's own
// "grammar-driven synthetic Go generation" section found ZERO existing
// coverage of generics anywhere in this fuzzer; this is the first.
func hazardGenericTypeParamShadowsNamedType(_ *rand.Rand, m *SyntheticModule) {
	src := `package genericsclash

type T struct {
	X int
}

func Wrap[T any](v T) []T {
	return []T{v}
}

func UseT() T {
	return T{X: 1}
}
`
	m.AddFile("pkg/genericsclash/types.go", src)
}

// hazardPromotedMethodShadowing is hazardPromotedFieldShadowing's
// method counterpart: an embedded type's method is normally promoted
// onto the embedding type, UNLESS the embedding type declares its own
// method of the same name, in which case the outer method wins and the
// embedded one is only reachable by qualifying through the embedded
// type's own name (d.Base.Greet(), not just d.Greet()). A very common
// Go idiom (wrapping/overriding embedded behavior); untested until now.
func hazardPromotedMethodShadowing(_ *rand.Rand, m *SyntheticModule) {
	src := `package shadowmethod

type Base struct{}

func (Base) Greet() string {
	return "base"
}

type Derived struct {
	Base
}

func (Derived) Greet() string {
	return "derived"
}

func UseGreet(d Derived) string {
	return d.Greet() + "/" + d.Base.Greet()
}
`
	m.AddFile("pkg/shadowmethod/types.go", src)
}

// hazardEmbeddedInterfaceComposition exercises interface EMBEDDING
// (ReadWriter embeds Reader and Writer) -- a distinct ast.InterfaceType
// shape from hazardInterfaceSatisfaction's flat interface: an embedded
// interface entry in Methods is a bare Ident/SelectorExpr with no
// Names/Func signature of its own, which a naive interface-methods
// walker could easily mishandle by assuming every entry is a named
// method. Untested until now.
func hazardEmbeddedInterfaceComposition(_ *rand.Rand, m *SyntheticModule) {
	src := `package ifacecompose

type Reader interface {
	Read() string
}

type Writer interface {
	Write(s string)
}

type ReadWriter interface {
	Reader
	Writer
}

type Buffer struct {
	data string
}

func (b *Buffer) Read() string {
	return b.data
}

func (b *Buffer) Write(s string) {
	b.data = s
}

func UseReadWriter(rw ReadWriter) string {
	rw.Write("hello")
	return rw.Read()
}
`
	m.AddFile("pkg/ifacecompose/types.go", src)
}

// hazardCrossPackageQualifiedCall exercises the most basic cross-package
// reference shape (producer package exports a func, consumer package
// imports it and calls it qualified as producer.Helper()) -- checked
// against every existing hazard and found to be completely absent:
// hazardMultiPackage only adds independent leaf funcs with no calls
// BETWEEN them, and hazardCollidingBasenames' packages never call each
// other either. Cross-package identifier resolution was entirely
// untested by this fuzzer before this hazard.
func hazardCrossPackageQualifiedCall(_ *rand.Rand, m *SyntheticModule) {
	m.AddFile("pkg/crosspkgcall/producer/producer.go", `package producer

func Helper() int {
	return 42
}
`)
	m.AddFile("pkg/crosspkgcall/consumer/consumer.go", `package consumer

import "example.com/synth/pkg/crosspkgcall/producer"

func UseHelper() int {
	return producer.Helper()
}
`)
}

// hazardConstIotaBlankSkip is a variant of hazardGroupedConstIota's
// sequential iota block: a leading blank identifier "_ = iota" skips
// the first iota value entirely, a common idiom for reserving 0 as an
// invalid/unset sentinel. Distinct declaration shape (a ValueSpec whose
// Names[0] is "_") from the plain sequential block already covered.
func hazardConstIotaBlankSkip(_ *rand.Rand, m *SyntheticModule) {
	src := `package iotaskip

type Level int

const (
	_ Level = iota
	LevelLow
	LevelMedium
	LevelHigh
)
`
	m.AddFile("pkg/iotaskip/level.go", src)
}

// hazardSelfReferencingPointerField exercises the ordinary linked-list/
// tree idiom -- a struct field whose type is a pointer to the struct's
// OWN type (Node.Next *Node) -- extremely common in real Go, and a
// simpler, more frequent variant than hazardFieldNamedAfterOwnType's
// cross-type "Foo *Foo" shape (which names a field after an UNRELATED
// type). Cheap defense-in-depth for a shape real corpora are full of.
func hazardSelfReferencingPointerField(_ *rand.Rand, m *SyntheticModule) {
	src := `package selfref

type Node struct {
	Value int
	Next  *Node
}

func Sum(n *Node) int {
	if n == nil {
		return 0
	}
	return n.Value + Sum(n.Next)
}
`
	m.AddFile("pkg/selfref/node.go", src)
}

// hazardBlankStructFields exercises Go's one real exception to "no two
// fields in a struct share a name": the blank identifier "_" may be
// repeated as a field name arbitrarily many times (a common padding/
// alignment idiom), unlike every other identifier in the same
// namespace. A field-uniqueness assumption anywhere in ingest/store
// (e.g. a map keyed by (type, field name)) would silently collide or
// drop these; untested until now.
func hazardBlankStructFields(_ *rand.Rand, m *SyntheticModule) {
	src := `package blankfields

type Padded struct {
	A int
	_ int
	_ int
	B int
}

func NewPadded(a, b int) Padded {
	return Padded{A: a, B: b}
}
`
	m.AddFile("pkg/blankfields/types.go", src)
}

// hazardDotImportUnqualifiedCall is hazardCrossPackageQualifiedCall's
// dot-import variant: "import . \"path\"" injects the imported package's
// exported identifiers directly into the importing file's scope, so the
// call site is UNQUALIFIED (Helper(), not producer.Helper()) despite
// being a genuine cross-package reference. Rare in real code but fully
// legal Go, and a plausible blind spot for any reference-resolution
// logic that assumes an unqualified call always resolves within the
// current package.
func hazardDotImportUnqualifiedCall(_ *rand.Rand, m *SyntheticModule) {
	m.AddFile("pkg/dotimport/producer/producer.go", "package producer\n\nfunc Helper() int {\n\treturn 7\n}\n")
	m.AddFile("pkg/dotimport/consumer/consumer.go", "package consumer\n\nimport . \"example.com/synth/pkg/dotimport/producer\"\n\nfunc UseHelper() int {\n\treturn Helper()\n}\n")
}

// hazardSplitMethods puts one type's methods across multiple files with
// unrelated basenames in the same package, exercising method-set
// reassembly during emit.
func hazardSplitMethods(_ *rand.Rand, m *SyntheticModule) {
	m.AddFile("pkg/splitm/types.go", "package splitm\n\ntype Widget struct {\n\tN int\n}\n")
	m.AddFile("pkg/splitm/a.go", "package splitm\n\nfunc (w *Widget) MethodA() int {\n\treturn w.N\n}\n")
	m.AddFile("pkg/splitm/b.go", "package splitm\n\nfunc (w *Widget) MethodB() int {\n\treturn w.N * 2\n}\n")
}

// hazardTestFileInterleave puts a _test.go file alongside its non-test
// sibling in the same package, exercising the Test bool dimension
// threaded through ingest.
func hazardTestFileInterleave(_ *rand.Rand, m *SyntheticModule) {
	m.AddFile("pkg/testintl/foo.go", "package testintl\n\nfunc Foo() int {\n\treturn 1\n}\n")
	m.AddFile("pkg/testintl/foo_test.go", "package testintl\n\nimport \"testing\"\n\nfunc TestFoo(t *testing.T) {\n\tif Foo() != 1 {\n\t\tt.Fail()\n\t}\n}\n")
}

// Hazard is a named, composable stress-injection function. Each hazard
// adds one or more files to a SyntheticModule, targeting one specific
// structural edge case in defn's ingest/emit round trip. Hazards use
// disjoint package names/paths so any subset can be combined into one
// module without collision.
type Hazard struct {
	Name  string
	Apply func(r *rand.Rand, m *SyntheticModule)
}

var AllHazards = buildAllHazards()

func buildAllHazards() []Hazard {
	hazards := []Hazard{
		{"colliding_basenames", hazardCollidingBasenames},
		{"scattered_init", hazardScatteredInit},
		{"method_named_init", hazardMethodNamedInit},
		{"multi_package", hazardMultiPackage},
		{"grouped_const_iota", hazardGroupedConstIota},
		{"embedded_fields", hazardEmbeddedFields},
		{"split_methods", hazardSplitMethods},
		{"floating_comments", hazardFloatingComments},
		{"blank_imports", hazardBlankImports},
		{"test_file_interleave", hazardTestFileInterleave},
		{"go_embed_directive", hazardGoEmbedDirective},
		{"interface_satisfaction", hazardInterfaceSatisfaction},
		{"field_named_after_own_type", hazardFieldNamedAfterOwnType},
		{"multi_name_var_decl", hazardMultiNameVarDecl},
		{"type_alias", hazardTypeAlias},
		{"method_on_different_types_same_name", hazardMethodOnDifferentTypesSameMethodName},
		{"same_field_name_across_types", hazardSameFieldNameAcrossTypes},
		{"promoted_field_shadowing", hazardPromotedFieldShadowing},
		{"generic_type_param_shadows_named_type", hazardGenericTypeParamShadowsNamedType},
		{"promoted_method_shadowing", hazardPromotedMethodShadowing},
		{"embedded_interface_composition", hazardEmbeddedInterfaceComposition},
		{"cross_package_qualified_call", hazardCrossPackageQualifiedCall},
		{"dot_import_unqualified_call", hazardDotImportUnqualifiedCall},
		{"const_iota_blank_skip", hazardConstIotaBlankSkip},
		{"self_referencing_pointer_field", hazardSelfReferencingPointerField},
		{"blank_struct_fields", hazardBlankStructFields},
		{"interface_dispatch_cross_package_test_variant", hazardInterfaceDispatchCrossPackageTestVariant},
		{"field_ref_cross_package_test_variant", hazardFieldRefCrossPackageTestVariant},
	}
	return append(hazards, CrossCallHazards...)
}

// hazardInterfaceSatisfaction adds an interface and a concrete type that
// satisfies it in the same package, plus a caller that dispatches
// through the interface. Exercises the interface-satisfaction class of
// bug found in a real defn session: a rename/edit of the concrete
// method (or a shape-changing edit of the interface/type itself) must
// be gated on a real build rather than defn's own fast-path heuristics,
// since a broken interface satisfaction is a compile error, not
// something the AST alone can rule out.
func hazardInterfaceSatisfaction(_ *rand.Rand, m *SyntheticModule) {
	m.AddFile("pkg/ifacesat/iface.go", "package ifacesat\n\ntype Greeter interface {\n\tGreet() string\n}\n")
	m.AddFile("pkg/ifacesat/impl.go", "package ifacesat\n\ntype Person struct {\n\tName string\n}\n\nfunc (p *Person) Greet() string {\n\treturn \"hello \" + p.Name\n}\n\nfunc UseGreeter(g Greeter) string {\n\treturn g.Greet()\n}\n")
}

// hazardMethodNamedInit reproduces the ingestFunc receiver-blindness
// bug (#354, found via a real caddy-6179 bench trajectory): a
// package-level func init() and a METHOD named init() (e.g. func (T)
// init()) in the SAME file were both fed through the SAME "multiple
// init() functions need synthetic disambiguation" counter meant only
// for the package-level case -- a method can never actually collide
// with a package-level function of the same name (the receiver
// already distinguishes it, exactly like any other method), so
// renaming it to "init_1" produced a DB name matching nothing in the
// real source. Downstream, this made every unrelated edit in the file
// fail to match an on-disk declaration, and emit -- unable to place
// "init_1" -- appended a byte-for-byte duplicate of the untouched
// original method, corrupting the file with a genuine "method already
// declared" Go compile error. TestRoundTrip_Hazards's declaration-
// multiset check (assertRoundTrip's own doc comment: "catches missing/
// duplicated decls") would have caught this on every `go test ./...`
// had this shape existed in the hazard set before the bug shipped.
func hazardMethodNamedInit(r *rand.Rand, m *SyntheticModule) {
	src := `package methodinitpkg

var registered bool

func init() {
	registered = true
}

type Widget struct{}

func (w *Widget) init() {
	registered = true
}
`
	m.AddFile("pkg/methodinitpkg/file.go", src)
}

// hazardFieldNamedAfterOwnType reproduces the shape behind #352 (found
// via a real caddy-7870 bench trajectory): Go's own "Foo *Foo"
// self-referencing field idiom -- a struct field sharing its bare Name
// with an unrelated top-level type in the same file (e.g. a health-check
// config's "Upstream *Upstream" field). The confirmed #352 bug was at
// the query/resolution layer (GetDefinitionByName), already fixed and
// covered by dedicated store/mcp tests -- this hazard adds defense-in-
// depth at the ingest/emit round-trip layer, since it's exactly the
// "two things sharing an identifier, distinguished only by kind/
// receiver" pattern this fuzzer exists to stress.
func hazardFieldNamedAfterOwnType(_ *rand.Rand, m *SyntheticModule) {
	src := `package fieldtype

type Upstream struct {
	Dial string
}

type ActiveHealthChecks struct {
	Upstream *Upstream
}

func (a *ActiveHealthChecks) Target() string {
	return a.Upstream.Dial
}
`
	m.AddFile("pkg/fieldtype/hosts.go", src)
}

// hazardMultiNameVarDecl reproduces the prometheus-corpus bug (commit
// 50d14c0): mergeDeclsIntoSource unconditionally bailed on any var/
// const ValueSpec declaring more than one name on a single line (e.g.
// "var a, b, c int"), permanently blocking writes to that file. Already
// covered by dedicated internal/emit and internal/mcp unit tests
// (TestMergeDeclsIntoSource_MultiNameValueSpecMatchesUnderFirstName,
// TestHandleEdit_MultiNameVarSpecInGroupedBlockDoesNotFalselyBlockUnrelatedEdit)
// but never exercised in this fuzzer's mutation-sequence/combined-
// hazard context until now.
func hazardMultiNameVarDecl(_ *rand.Rand, m *SyntheticModule) {
	src := `package multiname

var alpha, beta, gamma int

const low, high = 0, 100

func Sum() int {
	return alpha + beta + gamma + low + high
}
`
	m.AddFile("pkg/multiname/vars.go", src)
}

// hazardTypeAlias exercises `type X = Y` (a type ALIAS, ast.TypeSpec
// with Assign set) alongside an ordinary `type Z Y` (a distinct
// defined type) in the same file. Not grounded in a confirmed defn
// bug -- added from external research into structurally-relevant Go
// gotchas (as opposed to the runtime/semantic gotchas most "Go
// pitfalls" lists cover, which don't apply to a tool that reassembles
// source text rather than executing it). A type alias and a defined
// type produce a different ast.TypeSpec shape (Assign token position
// set vs. unset); untested until now.
func hazardTypeAlias(_ *rand.Rand, m *SyntheticModule) {
	src := `package typealias

type Meters float64

// Kilometers is an ALIAS for Meters -- NOT a distinct type.
type Kilometers = Meters

// Feet is a distinct DEFINED type based on Meters.
type Feet Meters

func ToKilometers(m Meters) Kilometers {
	return Kilometers(m) / 1000
}

func ToFeet(m Meters) Feet {
	return Feet(m) * 3.28084
}
`
	m.AddFile("pkg/typealias/units.go", src)
}

// hazardFieldRefCrossPackageTestVariant is hazardInterfaceDispatchCrossPackageTestVariant's
// field_ref counterpart: collectRefs resolves both a keyed composite
// literal (Type{Field: val}) and a direct selector (x.Field) via the same
// DB-backed lookupFieldDefID -- deliberately NOT via objToDef/object
// identity, for the identical test-variant-identity reason documented on
// both call sites in resolve.go. config has its own _test.go file; caller
// exercises both entry points (a keyed composite literal in BuildConfig,
// a direct selector in UseConfig) against it from a separate, non-test
// package.
func hazardFieldRefCrossPackageTestVariant(_ *rand.Rand, m *SyntheticModule) {
	m.AddFile("pkg/fieldtestvariant/config/config.go", `package config

type Config struct {
	Name string
}
`)
	m.AddFile("pkg/fieldtestvariant/config/config_test.go", `package config

import "testing"

func TestConfig(t *testing.T) {
	c := Config{Name: "test"}
	if c.Name != "test" {
		t.Fail()
	}
}
`)
	m.AddFile("pkg/fieldtestvariant/caller/caller.go", `package caller

import "example.com/synth/pkg/fieldtestvariant/config"

func BuildConfig() config.Config {
	return config.Config{Name: "prod"}
}

func UseConfig(c config.Config) string {
	return c.Name
}
`)
}

// hazardInterfaceDispatchCrossPackageTestVariant exercises collectRefs's
// interface_dispatch fallback (see its doc comment in resolve.go) across
// the specific boundary that used to break it silently: the
// interface-owning package (api) has its own _test.go file, so
// packages.Load(Tests:true)'s FilterPackages prefers a "test variant"
// *types.Object identity for api's own pass, while the non-test caller
// package resolves the SAME interface method through a completely
// different type-checking session's object identity. A naive
// object-identity-keyed dispatch map misses this every time; the fix
// (ifaceMethodKey, string-keyed by pkgPath+ifaceName+methodName) is only
// exercised end-to-end when the interface's own package has _test.go
// files -- no existing hazard combines cross-package interface dispatch
// with a _test.go-having interface-owning package.
func hazardInterfaceDispatchCrossPackageTestVariant(_ *rand.Rand, m *SyntheticModule) {
	m.AddFile("pkg/ifacetestvariant/api/api.go", `package api

type Doer interface {
	Do() string
}
`)
	m.AddFile("pkg/ifacetestvariant/api/api_test.go", `package api

import "testing"

type stubDoer struct{}

func (stubDoer) Do() string { return "stub" }

func TestDoer(t *testing.T) {
	var d Doer = stubDoer{}
	if d.Do() != "stub" {
		t.Fail()
	}
}
`)
	m.AddFile("pkg/ifacetestvariant/worker/worker.go", `package worker

type Worker struct{}

func (Worker) Do() string {
	return "work"
}
`)
	m.AddFile("pkg/ifacetestvariant/caller/caller.go", `package caller

import (
	"example.com/synth/pkg/ifacetestvariant/api"
	"example.com/synth/pkg/ifacetestvariant/worker"
)

func RunDoer() string {
	var d api.Doer = worker.Worker{}
	return d.Do()
}
`)
}
