package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

var featureFiles = []string{"identity-oauth.md", "conversations-messages.md", "inbox.md", "files.md", "lifecycle.md"}
var headings = []string{"Sub-features", "How to get to it (user POV)", "Driving it with verify-messagehub", "Gotchas"}
var markdownLink = regexp.MustCompile(`\[[^\]]*\]\(([^\s)]+)\)`)

func verifyModule(root string) error {
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return errors.New("module go.mod unavailable")
	}
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "module" {
			if len(f) != 2 || f[1] != moduleID || found {
				return errors.New("wrong module identity")
			}
			found = true
		}
	}
	if !found {
		return errors.New("module identity missing")
	}
	return nil
}
func readOperations(path string) ([]operation, error) {
	tree, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, errors.New("operation source cannot be parsed")
	}
	var ops []operation
	names, paths := map[string]bool{}, map[string]bool{}
	invalid := false
	ast.Inspect(tree, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := call.Fun
		switch v := fn.(type) {
		case *ast.IndexExpr:
			fn = v.X
		case *ast.IndexListExpr:
			fn = v.X
		}
		name, ok := fn.(*ast.Ident)
		if !ok || name.Name != "typedOperation" {
			return true
		}
		if len(call.Args) < 2 {
			invalid = true
			return false
		}
		values := make([]string, 2)
		for i := range values {
			lit, ok := call.Args[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				invalid = true
				return false
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				invalid = true
				return false
			}
			values[i] = value
		}
		if values[0] == "" || !strings.HasPrefix(values[1], "/v1/") || names[values[0]] || paths[values[1]] {
			invalid = true
			return false
		}
		names[values[0]] = true
		paths[values[1]] = true
		ops = append(ops, operation{Name: values[0], Path: values[1]})
		return true
	})
	if invalid || len(ops) == 0 {
		return nil, errors.New("invalid or duplicate source operations")
	}
	return ops, nil
}
func readAdminCommands(path string) ([]string, error) {
	tree, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, errors.New("admin source cannot be parsed")
	}
	commands := []string{}
	for _, decl := range tree.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Run" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, v := range clause.List {
				lit, ok := v.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING {
					s, err := strconv.Unquote(lit.Value)
					if err == nil && !slices.Contains(commands, s) {
						commands = append(commands, s)
					}
				}
			}
			return true
		})
	}
	if len(commands) == 0 {
		return nil, errors.New("admin command cases missing")
	}
	return commands, nil
}
func checkFeatureMap(root string) ([]operation, error) {
	if err := verifyModule(root); err != nil {
		return nil, err
	}
	for _, p := range []string{"scripts/go", "cmd/messagehub/main.go", "internal/server/handler.go", "internal/server/oauth.go", "internal/config/config.go"} {
		st, err := os.Stat(filepath.Join(root, p))
		if err != nil || !st.Mode().IsRegular() {
			return nil, errors.New("source entry missing")
		}
	}
	ops, err := readOperations(filepath.Join(root, "internal/server/operations.go"))
	if err != nil {
		return nil, err
	}
	commands, err := readAdminCommands(filepath.Join(root, "internal/admin/admin.go"))
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "skills/verify-messagehub/features")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.New("feature map missing")
	}
	if len(entries) != len(featureFiles)+1 {
		return nil, errors.New("feature map has missing or extra files")
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || (entry.Name() != "README.md" && !slices.Contains(featureFiles, entry.Name())) {
			return nil, errors.New("feature map contains unexpected entry")
		}
	}
	index, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		return nil, errors.New("feature index missing")
	}
	links := map[string]int{}
	for _, match := range markdownLink.FindAllStringSubmatch(string(index), -1) {
		link := strings.TrimPrefix(match[1], "./")
		if strings.HasSuffix(link, ".md") || strings.Contains(link, ".md#") {
			if !slices.Contains(featureFiles, link) {
				return nil, errors.New("feature index has dead or external document link")
			}
			links[link]++
		}
	}
	docs := []string{}
	for _, name := range featureFiles {
		if links[name] != 1 {
			return nil, errors.New("feature index has duplicate or missing link")
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, errors.New("feature document missing")
		}
		var hs []string
		h1 := 0
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "# ") {
				h1++
			}
			if strings.HasPrefix(line, "## ") {
				hs = append(hs, strings.TrimSpace(strings.TrimPrefix(line, "## ")))
			}
		}
		if h1 != 1 || !slices.Equal(hs, headings) {
			return nil, errors.New("feature document heading contract violated")
		}
		docs = append(docs, string(body))
	}
	for _, op := range ops {
		found := false
		for _, doc := range docs {
			if strings.Contains(doc, op.Name) && strings.Contains(doc, op.Path) {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("unmapped operation %s", op.Name)
		}
	}
	all := strings.Join(docs, "\n")
	for _, cmd := range commands {
		if !strings.Contains(all, cmd) {
			return nil, fmt.Errorf("unmapped admin command %s", cmd)
		}
	}
	return ops, nil
}

// Hash build inputs with path and length framing. Skill/docs edits are not product changes.
func sourceHash(root string) (string, error) {
	paths := []string{"go.mod", "go.sum"}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("symlink in source")
			}
			if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				paths = append(paths, rel)
			}
			return nil
		})
		if err != nil {
			return "", errors.New("source hash traversal failed")
		}
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return "", errors.New("source hash read failed")
		}
		fmt.Fprintf(h, "%d:%s:%d:", len(path), path, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
