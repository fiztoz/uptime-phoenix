package bootstrap

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestRunGatesEveryAlertDeliveryWriter pins the production wiring for #119.
// Every service that accepts a desired alert_delivery (assignment replace,
// monitor create and clone, config-as-code plan and apply) refuses aggregate
// and both unless it is told the hub's PROBES_AGGREGATE_PAGING_ENABLED value.
// A service Run builds but never hands that flag would silently refuse the
// modes on hubs that enabled them, so each constructed instance must receive
// exactly cfg.ProbesAggregatePagingEnabled.
func TestRunGatesEveryAlertDeliveryWriter(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	constructors := map[string]bool{"NewMonitorService": true, "NewConfigService": true, "NewProbeAssignmentService": true}
	built := map[string]string{} // variable -> constructor
	gated := map[string]int{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range node.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok || i >= len(node.Lhs) {
					continue
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && constructors[sel.Sel.Name] {
					if id, ok := node.Lhs[i].(*ast.Ident); ok {
						built[id.Name] = sel.Sel.Name
					} else {
						t.Errorf("%s result is not held in a variable, so its gate cannot be checked", sel.Sel.Name)
					}
				}
			}
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SetAggregatePagingEnabled" {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || len(node.Args) != 1 {
				t.Errorf("unexpected SetAggregatePagingEnabled call shape")
				return true
			}
			arg, ok := node.Args[0].(*ast.SelectorExpr)
			if base, isIdent := arg.X.(*ast.Ident); !ok || !isIdent || base.Name != "cfg" || arg.Sel.Name != "ProbesAggregatePagingEnabled" {
				t.Errorf("%s.SetAggregatePagingEnabled is not passed cfg.ProbesAggregatePagingEnabled", recv.Name)
			}
			gated[recv.Name]++
		}
		return true
	})
	seen := map[string]bool{}
	for name, ctor := range built {
		seen[ctor] = true
		if gated[name] != 1 {
			t.Errorf("%s (from services.%s) receives the aggregate paging flag %d times, want exactly once", name, ctor, gated[name])
		}
	}
	for ctor := range constructors {
		if !seen[ctor] {
			t.Errorf("Run no longer builds services.%s; update this wiring check", ctor)
		}
	}
}
