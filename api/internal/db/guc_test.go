package db_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// gucAllowed are the only files that may set the app.* GUCs that
// app_user() and app_organizer() trust (spec D12: Postgres can't restrict
// who sets a custom GUC, so the code that does is kept this small).
var gucAllowed = map[string]string{
	"internal/db/scope.go":              "the scope helper: every request's user, key and organizer",
	"internal/organizers/organizers.go": "create: a new organizer has no scope yet, so it sets app.organizer_id itself, with a bind parameter",
	"internal/system/system.go":         "resetScope: ts_system clears the user and key and names the organizer, with a bind parameter",
}

var (
	// Any 'app. or "app. literal: set_config('app.x', ...) itself, or a GUC
	// name kept in a Go string and passed in as a parameter.
	gucSet   = regexp.MustCompile(`(?i)set\s+local\s+app\.|['"]app\.`)
	sqlVerbs = regexp.MustCompile(`(?i)\b(select|update|insert|delete)\s|set_config`)
)

// gucViolations walks the non-test Go files under root and lists every file
// outside gucAllowed that names an app.* GUC, and every fmt.Sprintf whose
// format string looks like SQL (SQL is built with bind parameters only).
func gucViolations(t *testing.T, root string) []string {
	t.Helper()
	var bad []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, ok := gucAllowed[rel]; !ok && gucSet.Match(src) {
			bad = append(bad, rel+": names an app.* GUC")
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Sprintf" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
				return true
			}
			if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil && sqlVerbs.MatchString(s) {
					bad = append(bad, fset.Position(call.Pos()).String()+": SQL built with fmt.Sprintf")
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return bad
}

// Spec D12: only db/scope.go, organizers.Create and system.Run set the
// GUCs, and no SQL is put together with fmt.Sprintf. A SQL injection is
// what would cross tenants, so this is the guard on both.
func TestOnlyScopeSetsTheGUCs(t *testing.T) {
	root, err := filepath.Abs("../..") // api/
	if err != nil {
		t.Fatal(err)
	}
	if bad := gucViolations(t, root); len(bad) > 0 {
		t.Fatalf("GUC or SQL rule broken:\n%s", strings.Join(bad, "\n"))
	}
	for rel := range gucAllowed {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || !gucSet.Match(src) {
			t.Errorf("%s is allowed to set the GUCs but no longer does (%v): drop it from the list", rel, err)
		}
	}

	// Spec test 15: the staff scope GUC is narrower still. Only WithStaff
	// names it; the other allowed files may set app.* but never this one.
	staffGUC := []byte("app.staff_session_id")
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if inScope := filepath.ToSlash(rel) == "internal/db/scope.go"; bytes.Contains(src, staffGUC) != inScope {
			t.Errorf("%s: names app.staff_session_id %v, want only internal/db/scope.go to", rel, !inScope)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The walk catches both, anywhere else.
	scratch := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scratch, "internal/other"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{
		"other.go": "package other\n\nimport \"fmt\"\n\nconst q = `select set_config('app.x', $1, true)`\n\n" +
			"func f(id string) string { return fmt.Sprintf(\"SELECT\\n* FROM t WHERE id = '%s'\", id) }\n",
		"name.go": "package other\n\nconst guc = \"app.organizer_id\" // handed to set_config($1, ...) elsewhere\n",
	} {
		if err := os.WriteFile(filepath.Join(scratch, "internal/other", name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if bad := gucViolations(t, scratch); len(bad) != 3 {
		t.Fatalf("scratch files: %v, want both GUC files and the Sprintf caught", bad)
	}
}
