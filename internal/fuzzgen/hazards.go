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
