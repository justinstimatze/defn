package astutil

import "go/ast"

// BareReceiverName extracts a method receiver's bare type name from its
// AST expression, stripping pointer and generic type-param syntax:
// "*Widget" -> "*Widget", "Stack[T]" -> "Stack", "*Pair[K, V]" -> "*Pair".
//
// This exact ~15-line helper was independently reimplemented three times
// (internal/emit's recvTypeName, internal/ingest's receiverTypeName,
// cmd/defn's receiverExprName) after each was found missing the
// *ast.IndexListExpr case (2+ type params) and silently producing an
// empty or wrong string for a generic receiver -- the same bug, found
// and fixed three separate times because there was no single source of
// truth. Every receiver-qualified lookup downstream (Definition.Receiver
// storage, FuncIdentity matching during emit, resolve.go's AST-based
// method lookup) needs this exact bare form; a bracket-inclusive or
// package-qualified receiver string silently breaks all of them.
func BareReceiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + BareReceiverName(t.X)
	case *ast.IndexExpr:
		return BareReceiverName(t.X)
	case *ast.IndexListExpr:
		return BareReceiverName(t.X)
	}
	return ""
}
