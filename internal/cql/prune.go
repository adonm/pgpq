package cql

func MayMatch(e Expr, leaf func(Expr) bool) bool {
	switch x := e.(type) {
	case nil:
		return true
	case andExpr:
		return MayMatch(x.left, leaf) && MayMatch(x.right, leaf)
	case orExpr:
		return MayMatch(x.left, leaf) || MayMatch(x.right, leaf)
	case notExpr:
		return true
	}
	return leaf(e)
}
