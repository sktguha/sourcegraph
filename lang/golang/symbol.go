package golang

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/build"
	"go/doc"
	"go/parser"
	"go/token"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/golang/groupcache/lru"

	"sourcegraph.com/sourcegraph/sourcegraph/lang/golang/internal/refs"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/cache"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/cmdutil"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/jsonrpc2"
	"sourcegraph.com/sourcegraph/sourcegraph/pkg/lsp"
)

var pkgVendorRe = regexp.MustCompile("(^|.*/)(vendor|Godeps)(/.*|$)")

func (h *Handler) handleSymbol(ctx context.Context, req *jsonrpc2.Request, params lsp.WorkspaceSymbolParams) ([]lsp.SymbolInformation, error) {
	q := parseSymbolQuery(params.Query)
	pkgs, err := expandPackages(ctx, h.goEnv(), q.Tokens)
	if err != nil {
		return nil, err
	}
	isStdlib := len(q.Tokens) == 1 && q.Tokens[0] == "github.com/golang/go/..."

	// HACK(slimsag): The langserver needs to know the repository through more
	// canonical means
	//
	// h.init.RootPath looks like:
	//
	//  /Users/stephen/.sourcegraph/workspace/go/github.com/slimsag/mux/780415097119f6f61c55475fe59b66f3c3e9ea53/workspace
	//  /sourcegraph/workspace/go/github.com/slimsag/mux/780415097119f6f61c55475fe59b66f3c3e9ea53/workspace
	//
	// so hack the repository out.
	split := strings.Split(h.init.RootPath, "workspace/go")[1:]
	split = strings.Split(filepath.Join(split...), "/")
	repo := ""
	if len(split) >= 2 { // For tests.
		repo = filepath.Join(split[:len(split)-2]...)
	}

	symbols := []lsp.SymbolInformation{}
	var failed int
	emit := func(name, container string, kind lsp.SymbolKind, fs *token.FileSet, pos token.Pos) {
		if q.Type == queryTypeExported && !isExported(name, container) {
			return
		}
		start := fs.Position(pos)
		end := fs.Position(pos + token.Pos(len(name)) - 1)
		uri, err := h.fileURI(start.Filename)
		if err != nil {
			failed++
			return
		}
		symbols = append(symbols, lsp.SymbolInformation{
			Name: name,
			Kind: kind,
			Location: lsp.Location{
				URI: uri,
				Range: lsp.Range{
					Start: lsp.Position{Line: start.Line - 1, Character: start.Column - 1},
					End:   lsp.Position{Line: end.Line - 1, Character: end.Column - 1},
				},
			},
			ContainerName: container,
		})
	}
	buildCtx := build.Default
	buildCtx.GOPATH = h.filePath("gopath")
	buildCtx.CgoEnabled = false
	if isStdlib {
		buildCtx.GOROOT = h.filePath("gopath/src/github.com/golang/go")
	}
	for _, pkg := range pkgs {
		// Exclude vendored from symbols
		if pkgVendorRe.MatchString(pkg) {
			continue
		}
		// Internal packages are not exported
		if q.Type == queryTypeExported && strings.Contains(pkg, "/internal/") {
			continue
		}
		emitForPkg := func(name, container string, kind lsp.SymbolKind, fs *token.FileSet, pos token.Pos) {
			if pos != 0 {
				emit(name, container, kind, fs, pos)
				return
			}
			// We have to special case the pkg symbol since it
			// doesn't have a parsed position
			path := filepath.Join(buildCtx.GOPATH, "src", pkg)
			if isStdlib {
				path = filepath.Join(buildCtx.GOPATH, "src/github.com/golang/go/src", pkg)
			}
			uri, err := h.fileURI(path)
			if err != nil {
				failed++
				return
			}
			symbols = append(symbols, lsp.SymbolInformation{
				Name: name,
				Kind: kind,
				Location: lsp.Location{
					URI: uri,
				},
				ContainerName: container,
			})
		}
		includeTests := q.Type != queryTypeExported
		fn := symbolDo
		if q.Type == queryTypeExternalRef {
			fn = h.externalRefs
		}
		err := fn(buildCtx, repo, pkg, includeTests, emitForPkg)
		if err != nil {
			return nil, err
		}
	}
	if failed > 0 {
		log.Printf("WARNING: failed to create %d symbols", failed)
	}
	return symbols, nil
}

type emitFunc func(name, container string, kind lsp.SymbolKind, fs *token.FileSet, pos token.Pos)

func (h *Handler) externalRefs(buildCtx build.Context, repo, pkgPath string, includeTests bool, emit emitFunc) error {
	// Import the package.
	bpkg, err := buildCtx.Import(pkgPath, "", 0)
	if err != nil {
		return err
	}

	pkgRepoRoot, _, err := gitRevParse(context.TODO(), filepath.Join(bpkg.SrcRoot, bpkg.ImportPath))
	if err != nil {
		return err
	}

	// Formulate a list of all files we want to consider for external references.
	fileNames := bpkg.GoFiles
	if includeTests {
		fileNames = append(fileNames, bpkg.TestGoFiles...)
	}
	fileNames = append(fileNames, bpkg.CgoFiles...)

	cfg := refs.Default()

	// Parse files.
	var files []*ast.File
	for _, fileName := range fileNames {
		// Make filename absolute, then parse it.
		fileName = filepath.Join(bpkg.SrcRoot, bpkg.ImportPath, fileName)
		f, err := parser.ParseFile(cfg.FileSet, fileName, nil, 0)
		if err != nil {
			// Only log the error, in hope that it's limited to just this one file.
			log.Println("externalRefs:", err)
		}
		files = append(files, f)
	}

	// Compute external references.
	stdlib := listGoStdlibPackages(context.TODO())
	resolveImportCache := make(map[string]string, 1000)
	err = cfg.Refs(bpkg.ImportPath, files, func(r *refs.Ref) {
		var repoRoot string
		if _, isStdlib := stdlib[r.Def.ImportPath]; isStdlib {
			repoRoot = "github.com/golang/go"
		} else {
			// This could be a dependency that we have cloned via 'go get', so
			// consult git in order to find the repository root (because e.g.
			// importPath could be a subpackage inside the repo).

			// First, find out if the importPath is vendored or not.
			resolvedImportPath, ok := resolveImportCache[r.Def.ImportPath]
			if !ok {
				impPkg, err := buildCtx.Import(r.Def.ImportPath, filepath.Join(bpkg.SrcRoot, pkgPath), build.FindOnly)
				if err != nil {
					log.Println("externalRefs:", err)
					return
				}
				resolvedImportPath = impPkg.ImportPath
				resolveImportCache[r.Def.ImportPath] = resolvedImportPath
			}

			repoRoot, _, err = gitRevParse(context.TODO(), filepath.Join(h.filePath("gopath/src"), resolvedImportPath))
			if err != nil {
				log.Println("externalRefs:", err)
				return
			}
			repoRoot = strings.TrimPrefix(repoRoot, h.filePath("gopath/src")+"/")
		}

		// If the definition being referenced is defined within this
		// repository, exclude it (to avoid bloating the database).
		if pathHasPrefix(repoRoot, repo) {
			return
		}

		// The filename which we emit (pointing to where the reference, not
		// def, is located) should be relative to the repository.
		repoRelFile, err := filepath.Rel(pkgRepoRoot, r.Position.Filename)
		if err != nil {
			log.Println("externalRefs:", err)
			return
		}

		var name string
		var path []string
		if fields := strings.Fields(r.Def.Path); len(fields) > 0 {
			name = fields[0]
			path = fields[1:]
		}

		containerName, err := json.Marshal(&struct {
			DefPath                   []string
			DefImportPath, Repo, File string
			Line, Column              int
		}{
			DefPath:       path,
			DefImportPath: r.Def.ImportPath,
			Repo:          repoRoot,
			File:          repoRelFile,
			Line:          r.Position.Line,
			Column:        r.Position.Column,
		})
		if err != nil {
			log.Println("externalRefs:", err)
			return
		}

		emit(name, string(containerName), lsp.SKProperty, cfg.FileSet, token.Pos(r.Position.Offset))
	})
	if err != nil {
		// Only log the error, in hope that it's limited to just a few refs or files.
		log.Println("externalRefs:", err)
	}
	return nil
}

var gitRevParseCache = cache.Sync(lru.New(2000))

// TODO: almost a 1:1 copy of the version in langprocessor-go.go except it uses
// cmdutil.Output not langp.CmdOutput. Find a way to unify them.
//
// TODO: OpenTracing
func gitRevParse(ctx context.Context, dir string) (repoPath, commit string, err error) {
	if v, found := gitRevParseCache.Get(dir); found {
		// This is cache to avoid running git rev-parse below
		lines := v.([]string)
		return lines[0], lines[1], nil
	}

	cmd := exec.Command("git", "rev-parse", "--show-toplevel", "HEAD")
	cmd.Dir = dir
	out, err := cmdutil.Output(cmd)
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) != 3 {
		return "", "", errors.New("unexpected number of lines from git rev-parse")
	}
	gitRevParseCache.Add(dir, lines)
	return lines[0], lines[1], nil
}

var (
	goStdlibPackages = make(map[string]struct{})
	listGoStdlibOnce sync.Once
)

// TODO: almost a 1:1 copy of the version in langprocessor-go.go except it uses
// cmdutil.Output not langp.CmdOutput. Find a way to unify them.
//
// TODO: OpenTracing
func listGoStdlibPackages(ctx context.Context) map[string]struct{} {
	listGoStdlibOnce.Do(func() {
		// Just so that we don't have to hard-code a list of stdlib packages.
		out, err := cmdutil.Output(exec.Command("go", "list", "std"))
		if err != nil {
			// Not fatal because this list is not 100% important.
			log.Println("WARNING:", err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if line != "" {
				goStdlibPackages[line] = struct{}{}
			}
		}
	})
	return goStdlibPackages
}

// pathHasPrefix tells if the specified path has the specified prefix by
// performing element-wise comparison. For example:
//
//  pathHasPrefix("github.com/golang/go-tools", "github.com/golang/go") == false
//  pathHasPrefix("github.com/golang/go/tools", "github.com/golang/go") == true
//
func pathHasPrefix(path, prefix string) bool {
	splitPath := strings.Split(path, "/")
	for i, splitPrefix := range strings.Split(prefix, "/") {
		if i > len(splitPath) {
			return false
		}
		if splitPath[i] != splitPrefix {
			return false
		}
	}
	return true
}

// recursiveScopeLookup attempts to lookup the name in the given scope, or it's
// outer scope (recursively) until it is found or nil is ultimately returned.
func recursiveScopeLookup(s *ast.Scope, name string) *ast.Object {
	if v := s.Lookup(name); v != nil {
		return v
	}
	if s.Outer != nil {
		return recursiveScopeLookup(s.Outer, name)
	}
	return nil
}

func symbolDo(buildCtx build.Context, _, pkgPath string, includeTests bool, emit emitFunc) error {
	// Package must be importable.
	bpkg, err := buildCtx.Import(pkgPath, "", 0)
	if err != nil {
		return err
	}
	pkg, err := parsePackage(bpkg, includeTests)
	if pkg == nil || err != nil {
		return err
	}

	// TODO
	// * go/doc doesn't parse out Fields of structs
	// * v.Decl.TokPos is not correct
	emit(pkg.build.ImportPath, "", lsp.SKPackage, pkg.fs, 0)
	for _, t := range pkg.doc.Types {
		emit(t.Name, pkg.build.ImportPath, lsp.SKClass, pkg.fs, t.Decl.TokPos)

		for _, v := range t.Funcs {
			emit(v.Name, pkg.build.ImportPath, lsp.SKFunction, pkg.fs, v.Decl.Name.NamePos)
		}
		for _, v := range t.Methods {
			emit(v.Name, pkg.build.ImportPath+" "+t.Name, lsp.SKMethod, pkg.fs, v.Decl.Name.NamePos)
		}
		for _, v := range t.Consts {
			for _, name := range v.Names {
				emit(name, pkg.build.ImportPath, lsp.SKConstant, pkg.fs, v.Decl.TokPos)
			}
		}
		for _, v := range t.Vars {
			for _, name := range v.Names {
				emit(name, pkg.build.ImportPath, lsp.SKVariable, pkg.fs, v.Decl.TokPos)
			}
		}
	}
	for _, v := range pkg.doc.Consts {
		for _, name := range v.Names {
			emit(name, pkg.build.ImportPath, lsp.SKConstant, pkg.fs, v.Decl.TokPos)
		}
	}
	for _, v := range pkg.doc.Vars {
		for _, name := range v.Names {
			emit(name, pkg.build.ImportPath, lsp.SKVariable, pkg.fs, v.Decl.TokPos)
		}
	}
	for _, v := range pkg.doc.Funcs {
		emit(v.Name, pkg.build.ImportPath, lsp.SKFunction, pkg.fs, v.Decl.Name.NamePos)
	}

	return nil
}

type parsedPackage struct {
	name  string // Package name, json for encoding/json.
	doc   *doc.Package
	build *build.Package
	fs    *token.FileSet // Needed for printing.
}

// parsePackage turns the build package we found into a parsed package
// we can then use to generate documentation.
func parsePackage(pkg *build.Package, includeTests bool) (*parsedPackage, error) {
	fs := token.NewFileSet()
	// include tells parser.ParseDir which files to include.
	// That means the file must be in the build package's GoFiles or CgoFiles
	// list only (no tag-ignored files, tests, swig or other non-Go files).
	include := func(info os.FileInfo) bool {
		for _, name := range pkg.GoFiles {
			if name == info.Name() {
				return true
			}
		}
		if !includeTests {
			return false
		}
		for _, name := range pkg.TestGoFiles {
			if name == info.Name() {
				return true
			}
		}
		for _, name := range pkg.XTestGoFiles {
			if name == info.Name() {
				return true
			}
		}
		return false
	}
	pkgs, err := parser.ParseDir(fs, pkg.Dir, include, 0)
	if err != nil {
		return nil, err
	}
	astPkg, ok := pkgs[pkg.Name]
	if !ok {
		// This happens in the case of pkgs which only include tests
		return nil, nil
	}

	docPkg := doc.New(astPkg, pkg.ImportPath, doc.AllDecls)

	return &parsedPackage{
		name:  pkg.Name,
		doc:   docPkg,
		build: pkg,
		fs:    fs,
	}, nil
}

// isExporter checks that the underlying symbol for (name, containerName) is
// exported in Go. The reason we can't just check name is we need to ensure
// that if it is part of a type, that the type is exported as well.
func isExported(name, containerName string) bool {
	if !ast.IsExported(name) {
		return false
	}
	// Ensure if we are part of a type, that the type is also exported
	split := strings.Fields(containerName)
	if len(split) == 2 {
		return ast.IsExported(split[1])
	}
	return true
}

func expandPackages(ctx context.Context, env, pkgs []string) ([]string, error) {
	isStdlib := false
	if len(pkgs) == 1 && pkgs[0] == "github.com/golang/go/..." {
		isStdlib = true
		pkgs = []string{"github.com/golang/go/src/..."}
	}
	args := append([]string{"list", "-e"}, pkgs...)
	b, err := cmdOutput(ctx, env, exec.Command("go", args...))
	if err != nil {
		return nil, err
	}
	expanded := strings.Fields(string(b))
	if isStdlib {
		filtered := make([]string, 0, len(expanded))
		for _, p := range expanded {
			p = p[len("github.com/golang/go/src/"):]
			if p == "builtin" {
				continue
			}
			filtered = append(filtered, p)
		}
		return filtered, nil
	}
	return expanded, nil
}

type queryType int

const (
	queryTypeAll queryType = iota
	queryTypeExported
	queryTypeExternalRef
)

type symbolQuery struct {
	// Type is the type of symbol query we are performing.
	Type queryType

	// Tokens is tokens which make up the query, in order they appear.
	Tokens []string
}

func parseSymbolQuery(q string) *symbolQuery {
	types := map[string]queryType{
		"is:all":          queryTypeAll,
		"is:exported":     queryTypeExported,
		"is:external-ref": queryTypeExternalRef,
	}
	tokens := strings.Fields(q)
	sq := &symbolQuery{
		Type:   queryTypeAll,
		Tokens: make([]string, 0, len(tokens)),
	}
	for _, tok := range tokens {
		if t, ok := types[tok]; ok {
			sq.Type = t
		} else {
			sq.Tokens = append(sq.Tokens, tok)
		}
	}
	return sq
}
