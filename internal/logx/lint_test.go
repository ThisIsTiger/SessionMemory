package logx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoRawLogAttrs enforces spec §13.1 over internal/: free-form slog
// attributes and ad-hoc printing are not allowed outside logx.
func TestNoRawLogAttrs(t *testing.T) {
	root := filepath.Join("..")
	banned := map[string]bool{"slog.String": true, "slog.Any": true, "slog.Group": true,
		"log.Print": true, "log.Printf": true, "log.Println": true, "log.Fatal": true, "log.Fatalf": true,
		"fmt.Print": true, "fmt.Printf": true, "fmt.Println": true}
	fset := token.NewFileSet()
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		slash := filepath.ToSlash(p)
		if strings.Contains(slash, "/logx/") || strings.Contains(slash, "/p0/") || strings.Contains(slash, "/testutil/") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && banned[id.Name+"."+sel.Sel.Name] {
					t.Errorf("%s: forbidden call %s.%s", fset.Position(call.Pos()), id.Name, sel.Sel.Name)
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "logx" && sel.Sel.Name == "Name" && len(call.Args) == 2 {
					switch a := call.Args[1].(type) {
					case *ast.BasicLit:
					case *ast.Ident, *ast.SelectorExpr:
						_ = a // named constants are accepted; variables cannot be told apart statically
					default:
						t.Errorf("%s: logx.Name second argument must be a literal or constant", fset.Position(call.Pos()))
					}
				}
			}
			return true
		})
		return nil
	})
}

func TestIDRedaction(t *testing.T) {
	if a := ID("k", "/Users/me/secret"); a.Value.String() != "<invalid>" {
		t.Errorf("path accepted as id: %v", a)
	}
	long := "rev_" + strings.Repeat("a", 64)
	if a := ID("k", long); a.Value.String() != "rev_aaaaaaaaaaaa" {
		t.Errorf("long id not shortened: %v", a)
	}
	if a := ID("k", "ss_"+strings.Repeat("a", 26)); !strings.HasPrefix(a.Value.String(), "ss_") {
		t.Errorf("session id rejected")
	}
}
