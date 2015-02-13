package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"go/ast"
	"go/build"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"golang.org/x/tools/go/ast/astutil"
	_ "golang.org/x/tools/go/gcimporter"
	"golang.org/x/tools/go/types"

	"github.com/peterh/liner"
)

var debug = false

func debugf(format string, args ...interface{}) {
	if !debug {
		return
	}

	_, file, line, ok := runtime.Caller(1)
	if ok {
		format = fmt.Sprintf("%s:%d %s", filepath.Base(file), line, format)
	}

	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func errorf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
}

var gorootSrc = filepath.Join(filepath.Clean(runtime.GOROOT()), "src")

func completeImport(prefix string) []string {
	result := []string{}
	seen := map[string]bool{}

	d, fn := path.Split(prefix)
	for _, srcDir := range build.Default.SrcDirs() {
		dir := filepath.Join(srcDir, d)

		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			if err != nil && !os.IsNotExist(err) {
				errorf("Stat %s: %s", dir, err)
			}
			continue
		}

		entries, err := ioutil.ReadDir(dir)
		if err != nil {
			errorf("ReadDir %s: %s", dir, err)
			continue
		}
		for _, fi := range entries {
			if !fi.IsDir() {
				continue
			}

			name := fi.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" {
				continue
			}

			if strings.HasPrefix(name, fn) {
				r := path.Join(d, name)
				if srcDir != gorootSrc {
					// append "/" if this directory is not a repository
					// e.g. does not have VCS directory such as .git or .hg
					// TODO do not append "/" to subdirectories of repos
					var isRepo bool
					for _, vcsDir := range []string{".git", ".hg", ".svn", ".bzr"} {
						_, err := os.Stat(filepath.Join(srcDir, filepath.FromSlash(r), vcsDir))
						if err == nil {
							isRepo = true
							break
						}
					}
					if !isRepo {
						r = r + "/"
					}
				}

				if !seen[r] {
					result = append(result, r)
					seen[r] = true
				}
			}
		}
	}

	return result
}

const (
	promptDefault  = "gore> "
	promptContinue = "..... "
)

type contLiner struct {
	*liner.State
	buffer string
}

func newContLiner() *contLiner {
	rl := liner.NewLiner()
	return &contLiner{State: rl}
}

func (cl *contLiner) promptString() string {
	if cl.buffer != "" {
		return promptContinue
	}

	return promptDefault
}

func (cl *contLiner) Prompt() (string, error) {
	line, err := cl.State.Prompt(cl.promptString())
	if err == io.EOF {
		if cl.buffer != "" {
			// cancel line continuation
			cl.Accepted()
			fmt.Println()
			err = nil
		}
	} else if err == nil {
		if cl.buffer != "" {
			cl.buffer = cl.buffer + "\n" + line
		} else {
			cl.buffer = line
		}
	}

	return cl.buffer, err
}

func (cl *contLiner) Accepted() {
	cl.State.AppendHistory(cl.buffer)
	cl.buffer = ""
}

func main() {
	s := NewSession()

	rl := newContLiner()
	defer rl.Close()

	// TODO: set up completion for:
	// - methods/fields using gocode?
	rl.SetWordCompleter(func(line string, pos int) (string, []string, string) {
		if strings.HasPrefix(line, ":") && !strings.Contains(line[0:pos], " ") {
			pre, post := line[0:pos], line[pos:]

			result := []string{}
			for _, command := range []string{":import", ":print"} {
				if strings.HasPrefix(command, pre) {
					if !strings.HasPrefix(post, " ") {
						command = command + " "
					}
					result = append(result, command)
				}
			}
			return "", result, post
		} else if strings.HasPrefix(line, ":import ") && pos >= len(":import ") {
			return ":import ", completeImport(line[len(":import "):pos]), ""
		}

		return "", nil, ""
	})

	for {
		in, err := rl.Prompt()
		if err != nil {
			if err == io.EOF {
				break
			}
			fmt.Fprintf(os.Stderr, "fatal: %s", err)
			os.Exit(1)
		}

		if in == "" {
			continue
		}

		err = s.Run(in)
		if err != nil {
			if err == ErrContinue {
				continue
			}
			fmt.Println(err)
		}
		rl.Accepted()
	}
}

type Session struct {
	FilePath string
	File     *ast.File
	Fset     *token.FileSet

	mainBody         *ast.BlockStmt
	storedBodyLength int
}

const initialSource = `
package main

import "fmt"

func p(xx ...interface{}) {
	for _, x := range xx {
		fmt.Printf("%#v\n", x)
	}
}

func main() {
}
`

func NewSession() *Session {
	var err error

	s := &Session{}
	s.Fset = token.NewFileSet()

	s.FilePath, err = tempFile()
	if err != nil {
		panic(err)
	}

	s.File, err = parser.ParseFile(s.Fset, "gore_session.go", initialSource, parser.Mode(0))
	if err != nil {
		panic(err)
	}

	mainFunc := s.File.Scope.Lookup("main").Decl.(*ast.FuncDecl)
	s.mainBody = mainFunc.Body

	return s
}

func (s *Session) BuildRunFile() error {
	f, err := os.Create(s.FilePath)
	if err != nil {
		return err
	}

	err = printer.Fprint(f, s.Fset, s.File)
	if err != nil {
		return err
	}

	return goRun(s.FilePath)
}

func tempFile() (string, error) {
	dir, err := ioutil.TempDir("", "")
	if err != nil {
		return "", err
	}

	err = os.MkdirAll(dir, 0755)
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, "gore_session.go"), nil
}

func goRun(file string) error {
	debugf("go run %s", file)

	cmd := exec.Command("go", "run", file)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (s *Session) injectExpr(in string) error {
	expr, err := parser.ParseExpr(in)
	if err != nil {
		return err
	}

	normalizeNode(expr)

	stmt := &ast.ExprStmt{
		X: &ast.CallExpr{
			Fun:  ast.NewIdent("p"), // TODO remove this after evaluation
			Args: []ast.Expr{expr},
		},
	}

	s.appendStatements(stmt)

	return nil
}

func (s *Session) injectStmt(in string) error {
	src := fmt.Sprintf("package P; func F() { %s }", in)
	f, err := parser.ParseFile(s.Fset, "stmt.go", src, parser.Mode(0))
	if err != nil {
		return err
	}

	enclosingFunc := f.Scope.Lookup("F").Decl.(*ast.FuncDecl)
	s.appendStatements(enclosingFunc.Body.List...)

	return nil
}

func (s *Session) appendStatements(stmts ...ast.Stmt) {
	s.mainBody.List = append(s.mainBody.List, stmts...)
}

type Error string

const (
	ErrContinue Error = "<continue input>"
)

func (e Error) Error() string {
	return string(e)
}

func (s *Session) handleImport(in string) bool {
	if !strings.HasPrefix(in, ":import ") {
		return false
	}

	path := in[len(":import "):]
	path = strings.Trim(path, `"`)

	astutil.AddImport(s.Fset, s.File, path)

	return true
}

// TODO after :print do not run
// TODO complete :print with not trailing space
func (s *Session) handlePrint(in string) bool {
	if strings.TrimSpace(in) != ":print" {
		return false
	}

	var buf bytes.Buffer
	err := printer.Fprint(&buf, s.Fset, s.File)
	if err == nil {
		fmt.Println(buf.String())
	} else {
		errorf("%s", err)
	}

	return true
}

var (
	rxDeclaredNotUsed = regexp.MustCompile(`^([a-zA-Z0-9_]+) declared but not used`)
	rxImportedNotUsed = regexp.MustCompile(`^(".+") imported but not used`)
)

// quickFixFile tries to fix the source AST so that it compiles well.
func (s *Session) quickFixFile() error {
	const maxAttempts = 10

	for i := 0; i < maxAttempts; i++ {
		_, err := types.Check("_quickfix", s.Fset, []*ast.File{s.File})
		if err == nil {
			break
		}

		debugf("quickFix :: err = %#v", err)

		if err, ok := err.(types.Error); ok && err.Soft {
			// Handle these situations:
			// - "%s declared but not used"
			// - "%q imported but not used"
			if m := rxDeclaredNotUsed.FindStringSubmatch(err.Msg); m != nil {
				ident := m[1]
				debugf("quickFix :: declared but not used -> %s", ident)
				// insert "_ = x" to supress "declared but not used" error
				// TODO: remove this statement after evaluation
				stmt := &ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent("_")},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{ast.NewIdent(ident)},
				}
				s.appendStatements(stmt)
			} else if m := rxImportedNotUsed.FindStringSubmatch(err.Msg); m != nil {
				path := m[1] // quoted string, but it's okay because this will be compared to ast.BasicLit.Value.
				debugf("quickFix :: imported but not used -> %s", path)

				for _, imp := range s.File.Imports {
					debugf("%s vs %s", imp.Path.Value, path)
					if imp.Path.Value == path {
						// make this import spec anonymous one
						imp.Name = ast.NewIdent("_")
						break
					}
				}
			} else {
				debugf("quickFix :: give up")
				break
			}
		} else {
			return err
		}
	}

	return nil
}

func (s *Session) clearQuickFix() {
	// make all import specs explicit (i.e. no "_").
	for _, imp := range s.File.Imports {
		imp.Name = nil
	}
}

func (s *Session) Run(in string) error {
	debugf("run >>> %q", in)

	s.clearQuickFix()

	if !s.handleImport(in) && !s.handlePrint(in) {
		if err := s.injectExpr(in); err != nil {
			debugf("expr :: err = %s", err)

			err := s.injectStmt(in)
			if err != nil {
				debugf("stmt :: err = %s", err)

				if _, ok := err.(scanner.ErrorList); ok {
					return ErrContinue
				}
			}
		}
	}

	s.quickFixFile()

	err := s.BuildRunFile()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			// if failed with status 2, remove the last statement
			if st, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok {
				if st.ExitStatus() == 2 {
					debugf("got exit status 2, popping out last input")
					s.RecallCode()
				}
			}
		}
	} else {
		s.RememberCode()
	}

	return err
}

// RememberCode stores current state of code so that it can be restored
// actually it saves the length of statements inside main()
func (s *Session) RememberCode() {
	s.storedBodyLength = len(s.mainBody.List)
}

func (s *Session) RecallCode() {
	s.mainBody.List = s.mainBody.List[0:s.storedBodyLength]
}

func normalizeNode(node ast.Node) {
	// TODO remove token.Pos information
}
