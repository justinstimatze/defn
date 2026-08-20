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

var AllHazards = []Hazard{
	{"colliding_basenames", hazardCollidingBasenames},
	{"scattered_init", hazardScatteredInit},
	{"multi_package", hazardMultiPackage},
	{"grouped_const_iota", hazardGroupedConstIota},
	{"embedded_fields", hazardEmbeddedFields},
	{"split_methods", hazardSplitMethods},
	{"floating_comments", hazardFloatingComments},
	{"blank_imports", hazardBlankImports},
	{"test_file_interleave", hazardTestFileInterleave},
	{"go_embed_directive", hazardGoEmbedDirective},
	{"interface_satisfaction", hazardInterfaceSatisfaction},
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
