// Command rewrite-test-assertions folds one-call testing failure blocks into
// testutil.OnFailure while preserving the original testing call verbatim.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/imports"
)

const testutilImportPath = "github.com/multica-ai/multica/server/internal/testutil"

type stats struct {
	blocks      int
	files       int
	handwritten int
}

type sourceEdit struct {
	start       int
	end         int
	replacement string
}

func main() {
	write := flag.Bool("w", false, "rewrite files in place")
	check := flag.Bool("check", false, "fail when a file would be rewritten")
	maxHandwritten := flag.Int("max-handwritten", -1, "fail when more than this many hand-written assertion blocks remain")
	flag.Parse()
	if *write && *check {
		fmt.Fprintln(os.Stderr, "-w and -check are mutually exclusive")
		os.Exit(2)
	}
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: rewrite-test-assertions [-w|-check] [-max-handwritten N] <file-or-directory>...")
		os.Exit(2)
	}

	result, err := rewritePaths(flag.Args(), *write)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *check && result.blocks > 0 {
		fmt.Fprintf(os.Stderr, "%d collapsible assertion blocks remain in %d files\n", result.blocks, result.files)
		os.Exit(1)
	}
	if *maxHandwritten >= 0 && result.handwritten > *maxHandwritten {
		fmt.Fprintf(os.Stderr, "%d hand-written assertion blocks exceed maximum %d\n", result.handwritten, *maxHandwritten)
		os.Exit(1)
	}
	verb := "found"
	if *write {
		verb = "rewrote"
	}
	fmt.Printf(
		"%s %d collapsible assertion blocks in %d files; %d hand-written blocks total\n",
		verb,
		result.blocks,
		result.files,
		result.handwritten,
	)
}

func rewritePaths(paths []string, write bool) (stats, error) {
	files, err := collectTestFiles(paths)
	if err != nil {
		return stats{}, err
	}
	var result stats
	for _, path := range files {
		source, err := os.ReadFile(path)
		if err != nil {
			return stats{}, fmt.Errorf("read %s: %w", path, err)
		}
		rewritten, blocks, handwritten, err := rewriteSource(path, source)
		if err != nil {
			return stats{}, err
		}
		remaining := handwritten
		if write {
			remaining -= blocks
		}
		result.handwritten += remaining
		if blocks == 0 {
			continue
		}
		result.blocks += blocks
		result.files++
		if !write {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return stats{}, fmt.Errorf("stat %s: %w", path, err)
		}
		if err := os.WriteFile(path, rewritten, info.Mode().Perm()); err != nil {
			return stats{}, fmt.Errorf("write %s: %w", path, err)
		}
	}
	return result, nil
}

func collectTestFiles(paths []string) ([]string, error) {
	seen := map[string]struct{}{}
	for _, root := range paths {
		info, err := os.Stat(root)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", root, err)
		}
		if !info.IsDir() {
			if strings.HasSuffix(root, "_test.go") {
				seen[filepath.Clean(root)] = struct{}{}
			}
			continue
		}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if path != root && (entry.Name() == "vendor" || strings.HasPrefix(entry.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(entry.Name(), "_test.go") {
				seen[filepath.Clean(path)] = struct{}{}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", root, err)
		}
	}
	files := make([]string, 0, len(seen))
	for path := range seen {
		files = append(files, path)
	}
	sort.Strings(files)
	return files, nil
}

func rewriteSource(filename string, source []byte) ([]byte, int, int, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, source, parser.ParseComments)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("parse %s: %w", filename, err)
	}
	handwritten := countHandwrittenAssertions(file)
	alias, imported, err := importAlias(file)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("inspect imports in %s: %w", filename, err)
	}
	if !imported {
		alias = unusedAlias(file, "testassert")
	}

	var edits []sourceEdit
	astutil.Apply(file, func(cursor *astutil.Cursor) bool {
		node := cursor.Node()
		statement, ok := collapsibleAssertion(file, node)
		if !ok {
			return true
		}
		ifStatement := node.(*ast.IfStmt)
		if parent, ok := cursor.Parent().(*ast.IfStmt); ok && parent.Else == ifStatement {
			return false
		}
		receiver := statement.X.(*ast.CallExpr).Fun.(*ast.SelectorExpr).X.(*ast.Ident)
		replacement := fmt.Sprintf(
			"%s.OnFailure(%s, %s, func() { %s })",
			alias,
			receiver.Name,
			sourceSlice(fset, source, ifStatement.Cond),
			sourceSlice(fset, source, statement.X),
		)
		if _, parseErr := parser.ParseExpr(replacement); parseErr != nil {
			err = fmt.Errorf("build replacement in %s: %w", filename, parseErr)
			return false
		}
		edits = append(edits, sourceEdit{
			start:       fset.PositionFor(ifStatement.Pos(), false).Offset,
			end:         fset.PositionFor(ifStatement.End(), false).Offset,
			replacement: replacement,
		})
		return false
	}, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	if len(edits) == 0 {
		return source, 0, handwritten, nil
	}

	var rewritten strings.Builder
	last := 0
	for _, edit := range edits {
		rewritten.Write(source[last:edit.start])
		rewritten.WriteString(edit.replacement)
		last = edit.end
	}
	rewritten.Write(source[last:])
	formatted, err := format.Source([]byte(rewritten.String()))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("format %s: %w", filename, err)
	}
	if imported {
		return formatted, len(edits), handwritten, nil
	}

	fset = token.NewFileSet()
	file, err = parser.ParseFile(fset, filename, formatted, parser.ParseComments)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("parse rewritten %s: %w", filename, err)
	}
	if !astutil.AddNamedImport(fset, file, alias, testutilImportPath) {
		return nil, 0, 0, fmt.Errorf("add %s import to %s", testutilImportPath, filename)
	}
	withImport, err := nodeBytes(fset, file)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("render %s: %w", filename, err)
	}
	formatted, err = imports.Process(filename, withImport, nil)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("format %s: %w", filename, err)
	}
	return formatted, len(edits), handwritten, nil
}

func sourceSlice(fset *token.FileSet, source []byte, node ast.Node) string {
	start := fset.PositionFor(node.Pos(), false).Offset
	end := fset.PositionFor(node.End(), false).Offset
	return string(source[start:end])
}

func collapsibleAssertion(file *ast.File, node ast.Node) (*ast.ExprStmt, bool) {
	statement, ok := node.(*ast.IfStmt)
	if !ok || statement.Init != nil || statement.Else != nil || len(statement.Body.List) != 1 {
		return nil, false
	}
	for _, comment := range file.Comments {
		if comment.Pos() > statement.Body.Lbrace && comment.End() < statement.Body.Rbrace {
			return nil, false
		}
	}
	return assertionStatement(statement)
}

func countHandwrittenAssertions(file *ast.File) int {
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		statement, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		if _, ok := assertionStatement(statement); ok {
			count++
		}
		return true
	})
	return count
}

func assertionStatement(statement *ast.IfStmt) (*ast.ExprStmt, bool) {
	if len(statement.Body.List) != 1 {
		return nil, false
	}
	expression, ok := statement.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return nil, false
	}
	call, ok := expression.X.(*ast.CallExpr)
	if !ok {
		return nil, false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !isTestingFailure(selector.Sel.Name) {
		return nil, false
	}
	receiver, ok := selector.X.(*ast.Ident)
	if !ok || (receiver.Name != "t" && receiver.Name != "tb") {
		return nil, false
	}
	return expression, true
}

func isTestingFailure(name string) bool {
	switch name {
	case "Fatal", "Fatalf", "Error", "Errorf":
		return true
	default:
		return false
	}
}

func importAlias(file *ast.File) (string, bool, error) {
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return "", false, err
		}
		if path != testutilImportPath {
			continue
		}
		if spec.Name == nil {
			return "testutil", true, nil
		}
		if spec.Name.Name == "." || spec.Name.Name == "_" {
			return "", false, fmt.Errorf("unsupported import name %q", spec.Name.Name)
		}
		return spec.Name.Name, true, nil
	}
	return "", false, nil
}

func unusedAlias(file *ast.File, base string) string {
	used := map[string]struct{}{}
	ast.Inspect(file, func(node ast.Node) bool {
		if identifier, ok := node.(*ast.Ident); ok {
			used[identifier.Name] = struct{}{}
		}
		return true
	})
	for suffix := 1; ; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = fmt.Sprintf("%s%d", base, suffix)
		}
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
}

func nodeBytes(fset *token.FileSet, node ast.Node) ([]byte, error) {
	var output strings.Builder
	if err := format.Node(&output, fset, node); err != nil {
		return nil, err
	}
	return []byte(output.String()), nil
}
