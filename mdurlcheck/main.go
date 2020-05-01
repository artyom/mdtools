// Program mdurlcheck checks whether given markdown files have any broken
// relative links to other files.
//
// It takes one or more .md files or directories as its arguments, then finds
// relative links (including image links) to other files/directories in them and
// checks whether such files/directories exist on the filesystem. If argument is
// directory, it recursively traverses this directory in search of .md files,
// while skipping directories with names starting with dot.
//
// Provided with the following file:
//
// 	[Document 1](doc1.md), [document 2](doc2.md), and [another
// 	one](dir/doc.md)
//
//	![program illustration](img/screenshot.jpg "Screenshot")
//
// The program will check whether files doc1.md, doc2.md, dir/doc.md, and
// img/screenshot.jpg exist on disk, relative to the location of provided file.
//
// If markdown file has any embedded html, this tool also parses such html
// taking into account all name or id attributes on html tags.
//
// Program reports any errors on stderr and exits with non-zero exit code.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/gomarkdown/markdown/ast"
	"github.com/gomarkdown/markdown/parser"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("usage: %s file.md|directory ...", filepath.Base(os.Args[0]))
	}
	var exitCode int
	intrefs := make(refMap)
	for _, name := range os.Args[1:] {
		if err := run(name, intrefs); err != nil {
			if err == errDirtyRun {
				exitCode = 1
				continue
			}
			log.Fatal(err)
		}
	}
	os.Exit(exitCode)
}

func run(name string, intrefs refMap) error {
	fi, err := os.Stat(name)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return processFile(name, intrefs)
	}
	var outErr error
	err = filepath.Walk(name, func(name string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if base := filepath.Base(name); fi.IsDir() && base != "." && strings.HasPrefix(base, ".") {
			return filepath.SkipDir
		}
		if fi.IsDir() || !strings.HasSuffix(name, ".md") {
			return nil
		}
		if err = processFile(name, intrefs); err == errDirtyRun {
			outErr = err
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	return outErr
}

func processFile(name string, intrefs refMap) error {
	b, err := ioutil.ReadFile(name)
	if err != nil {
		return err
	}
	doc := parser.NewWithExtensions(extensions).Parse(b)

	idRefs := extractRefs(doc)
	var hadErrors bool
	walkFn := func(node ast.Node, entering bool) ast.WalkStatus {
		if !entering {
			return ast.GoToNext
		}
		var exists func(string) bool = fileOrDirExists
		var dst string
		switch n := node.(type) {
		case *ast.Link:
			dst = string(n.Destination)
		case *ast.Image:
			exists = fileExists
			dst = string(n.Destination)
		default:
			return ast.GoToNext
		}
		if dst == "" {
			log.Printf("%s: empty url", name)
			return ast.GoToNext
		}
		u, err := url.Parse(dst)
		if err != nil {
			log.Printf("%s: %q: %v", name, dst, err)
			hadErrors = true
			return ast.GoToNext
		}
		if u.Scheme == "" && u.Host == "" && u.Path == "" && u.Fragment != "" {
			if _, ok := idRefs[u.Fragment]; !ok {
				hadErrors = true
				log.Printf("%s: %q: broken link", name, dst)
			} else if unstableRef(u.Fragment, idRefs) {
				log.Printf(unstableSlugFormat, name, dst)
			}
		}
		if u.Scheme != "" || u.Host != "" || u.Path == "" {
			return ast.GoToNext
		}

		var filename string
		if strings.HasPrefix(u.Path, "/") && false {
			filename = filepath.Join(".", filepath.FromSlash(u.Path))
		} else {
			filename = filepath.Join(filepath.Dir(name), filepath.FromSlash(u.Path))
		}

		if !exists(filename) {
			hadErrors = true
			log.Printf("%s: %q: broken link", name, dst)
		}
		if u.Fragment != "" && strings.HasSuffix(filename, ".md") {
			okf, okr := intrefs.hasRef(filename, u.Fragment)
			if !okf {
				if r, err := fileRefs(filename); err == nil {
					intrefs.setRefs(filename, r)
					_, okr = r[u.Fragment]
				}
			}
			if !okr {
				hadErrors = true
				log.Printf("%s: %q: broken link (fragment points to non-existent id)", name, dst)
			} else if unstableRef(u.Fragment, intrefs[filename]) {
				log.Printf(unstableSlugFormat, name, dst)
			}
		}
		return ast.GoToNext
	}
	_ = ast.Walk(doc, ast.NodeVisitorFunc(walkFn))
	if hadErrors {
		return errDirtyRun
	}
	return nil
}

// unstableRef checks whether s looks something like "value-1" and allRefs also
// has "value" in them.
func unstableRef(s string, allRefs map[string]struct{}) bool {
	if ok, _ := filepath.Match("*?-[1-9]", s); !ok {
		return false
	}
	_, ok := allRefs[s[:len(s)-2]]
	return ok
}

func extractRefs(doc ast.Node) map[string]struct{} {
	idRefs := make(map[string]struct{})
	_ = ast.Walk(doc, ast.NodeVisitorFunc(func(node ast.Node, entering bool) ast.WalkStatus {
		if !entering {
			return ast.GoToNext
		}
		var htmlData []byte
		switch n := node.(type) {
		case *ast.HTMLSpan:
			htmlData = n.Literal
		case *ast.HTMLBlock:
			htmlData = n.Literal
		}
		if len(htmlData) > 0 {
			for _, s := range extractRefsHTML(htmlData) {
				idRefs[s] = struct{}{}
			}
			return ast.GoToNext
		}
		if n, ok := node.(*ast.Heading); ok && n.HeadingID != "" {
			if _, ok := idRefs[n.HeadingID]; !ok {
				idRefs[n.HeadingID] = struct{}{}
				return ast.GoToNext
			}
			for i := 1; i < 100; i++ {
				s := fmt.Sprintf("%s-%d", n.HeadingID, i)
				if _, ok := idRefs[s]; !ok {
					idRefs[s] = struct{}{}
					return ast.GoToNext
				}
			}
		}
		return ast.GoToNext
	}))
	if len(idRefs) == 0 {
		return nil
	}
	return idRefs
}

func fileRefs(name string) (map[string]struct{}, error) {
	b, err := ioutil.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return extractRefs(parser.NewWithExtensions(extensions).Parse(b)), nil
}

var errDirtyRun = errors.New("some links are not ok")

func fileExists(name string) bool {
	fi, err := os.Stat(name)
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular()
}

func fileOrDirExists(name string) bool {
	fi, err := os.Stat(name)
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular() || fi.IsDir()
}

// refMap is used to cache and resolve links like file.md#header. Top-level keys
// are full filenames, second-level keys are internal ids discovered from
// headers
type refMap map[string]map[string]struct{}

// hasRef returns result of lookup of file and ref inside cache. First bool is
// whether file is known, second bool is whether ref for this file is known.
func (m refMap) hasRef(file, ref string) (bool, bool) {
	r, ok := m[file]
	if !ok {
		return false, false
	}
	_, ok = r[ref]
	return true, ok
}

func (m refMap) setRefs(file string, refs map[string]struct{}) { m[file] = refs }

// extractRefsHTML takes piece of html markup, parses it and returns values of
// any name or id attributes found.
func extractRefsHTML(b []byte) []string {
	if !utf8.Valid(b) {
		return nil
	}
	var out []string
	z := html.NewTokenizer(bytes.NewReader(b))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			for _, hasAttr := z.TagName(); hasAttr; {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				if len(v) == 0 {
					continue
				}
				switch atom.Lookup(k) {
				case atom.Id, atom.Name:
					out = append(out, string(v))
				}
			}
		}
	}
}

const extensions = parser.CommonExtensions | parser.AutoHeadingIDs ^ parser.MathJax

const unstableSlugFormat = "%s: %q: unstable slug reference, may become incorrect on unrelated header changes"

func init() { log.SetFlags(0) }

//go:generate sh -c "go doc >README"
