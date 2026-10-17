package cql

func Rename(e Expr, fn func(string) string) Expr {
	switch v := e.(type) {
	case nil:
		return nil
	case *Compare:
		return &Compare{Prop: fn(v.Prop), Op: v.Op, Value: v.Value}
	case *Between:
		return &Between{Prop: fn(v.Prop), Lo: v.Lo, Hi: v.Hi, Not: v.Not}
	case *In:
		return &In{Prop: fn(v.Prop), Values: v.Values, Not: v.Not}
	case *Like:
		return &Like{Prop: fn(v.Prop), Pattern: v.Pattern, Not: v.Not}
	case *IsNull:
		return &IsNull{Prop: fn(v.Prop), Not: v.Not}
	case andExpr:
		return andExpr{Rename(v.left, fn), Rename(v.right, fn)}
	case orExpr:
		return orExpr{Rename(v.left, fn), Rename(v.right, fn)}
	case notExpr:
		return notExpr{Rename(v.inner, fn)}
	}
	return e
}
