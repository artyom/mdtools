// Command mdmovearound helps when reorganizing set of cross-referencing
// markdown (.md) files. It should be run once before moving/renaming files, and
// then run again to update broken cross-links.
//
// It works by first recursively traversing directory making list of all
// non-hidden files along with hashes of their contents. Once such list is built
// and saved, on next call it loads list from previous run, builds list once
// again for current directory state, then checks each markdown file (.md) from
// this new list, trying to reconstruct broken links by finding file renames via
// old and new hash lists. Upon successful completion it updates saved list with
// current state.
//
// Because it works by figuring out renames looking at file content hashes, it
// only works for files that were NOT modified between calls to this program.
//
// Since it may potentially update multiple files, the whole operation is not
// atomic, so it is advisable to only run it over files versioned by VCS, so
// that in case of any errors original files can be easily restored.
//
// Currently only inline links like [link](dst.md) are supported; links like
// [link][id] are NOT supported. The reason for this is that links are updated
// by substring replacements inside text, this may lead to some invalid
// replacements, and handling only inline links reduces risk of invalid
// replacements. Please check results before committing them.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/artyom/autoflags"
	"github.com/gomarkdown/markdown/ast"
	"github.com/gomarkdown/markdown/parser"
)

func main() {
	args := runArgs{Dir: "."}
	autoflags.Parse(&args)
	if err := run(args); err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}

type runArgs struct {
	Name string `flag:"f,file to save state"`
	Dir  string `flag:"dir,directory to scan"`
}

func run(args runArgs) error {
	if args.Name == "" {
		return fmt.Errorf("state file should be set")
	}
	hh, err := loadHashes(args.Name)
	if os.IsNotExist(err) {
		log.Printf("file %q not found, building one", args.Name)
		hh, err := buildHashes(args.Dir)
		if err != nil {
			return err
		}
		if err := saveHashes(hh, args.Name); err != nil {
			return err
		}
		log.Println("state saved, move some files around and then run program with the same flags")
		return nil
	}
	if err != nil {
		return err
	}
	hh2, err := buildHashes(args.Dir)
	if err != nil {
		return err
	}
	didUpdates, err := fixDocuments(args.Dir, hh, hh2)
	if err != nil {
		return err
	}
	if !didUpdates {
		return nil
	}
	// need to rebuild because of applied updates
	if hh2, err = buildHashes(args.Dir); err != nil {
		return err
	}
	if err := saveHashes(hh2, args.Name); err != nil {
		return fmt.Errorf("state update: %w", err)
	}
	log.Println("state updated; run program with the same flags next time you move files around without modifying them")
	return nil
}

func loadHashes(name string) ([]fileHash, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []fileHash
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.SplitN(sc.Text(), " ", 2)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid line: %q", sc.Text())
		}
		out = append(out, fileHash{
			Hash: strings.TrimSpace(fields[0]),
			Name: strings.TrimSpace(fields[1]),
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func saveHashes(hh []fileHash, name string) error {
	tf, err := ioutil.TempFile(filepath.Dir(name), ".mdmovearound-")
	if err != nil {
		return err
	}
	defer tf.Close()
	defer os.Remove(tf.Name())
	for _, fh := range hh {
		if _, err := fmt.Fprintln(tf, fh); err != nil {
			return err
		}
	}
	if err := tf.Close(); err != nil {
		return err
	}
	return os.Rename(tf.Name(), name)
}

func buildHashes(dir string) ([]fileHash, error) {
	if dir == "" {
		return nil, fmt.Errorf("dir must not be empty")
	}
	var out []fileHash
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		base := filepath.Base(path)
		if info.IsDir() && base != "." && base != ".." && strings.HasPrefix(base, ".") {
			return filepath.SkipDir
		}
		if !info.Mode().IsRegular() || strings.HasPrefix(base, ".") {
			return nil
		}
		h, err := buildFileHash(path)
		if err != nil {
			return err
		}
		out = append(out, fileHash{Name: path, Hash: hex.EncodeToString(h)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type fileHash struct {
	Name string
	Hash string
}

func (fh fileHash) String() string { return fh.Hash + "  " + fh.Name }

func buildFileHash(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func fixDocuments(dir string, oldHashes, newHashes []fileHash) (bool, error) {
	oldFileToHash := make(map[string]string, len(oldHashes))
	oldHashToFile := make(map[string]string, len(oldHashes))
	for _, fh := range oldHashes {
		oldFileToHash[fh.Name] = fh.Hash
		oldHashToFile[fh.Hash] = fh.Name
	}
	hashToFile := make(map[string]string, len(newHashes))
	for _, fh := range newHashes {
		hashToFile[fh.Hash] = fh.Name
	}
	var didUpdates bool
	for _, fh := range newHashes {
		if !strings.HasSuffix(fh.Name, ".md") {
			continue
		}
		ok, err := processFile(fh.Name, oldFileToHash, oldHashToFile, hashToFile)
		if err != nil {
			return didUpdates, err
		}
		if ok {
			didUpdates = true
		}
	}
	return didUpdates, nil
}

func processFile(name string, oldFileToHash, oldHashToFile, hashToFile map[string]string) (bool, error) {
	b, err := ioutil.ReadFile(name)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(b)
	selfHash := hex.EncodeToString(sum[:])
	oldName := oldHashToFile[selfHash]
	if oldName == "" {
		log.Printf("cannot figure out old name for %q (%s), skipping", name, selfHash)
		return false, nil
	}
	var repl []string
	doc := parser.NewWithExtensions(extensions).Parse(b)
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
			return ast.GoToNext
		}
		u, err := url.Parse(dst)
		if err != nil {
			return ast.GoToNext
		}
		if !(u.Path != "" && u.Host == "" && u.Scheme == "") {
			return ast.GoToNext
		}
		filename := filepath.Join(filepath.Dir(name), filepath.FromSlash(u.Path))
		if exists(filename) {
			return ast.GoToNext
		}
		oldFilename := filepath.Join(filepath.Dir(oldName), filepath.FromSlash(u.Path))
		h, ok := oldFileToHash[oldFilename]
		if !ok {
			return ast.GoToNext
		}
		candidate, ok := hashToFile[h]
		if !ok {
			return ast.GoToNext
		}
		newName, err := filepath.Rel(filepath.Dir(name), candidate)
		if err != nil {
			log.Printf("%s: filepath.Rel(%q, %q): %v", name, filepath.Dir(name), candidate, err)
			return ast.GoToNext
		}
		u2 := &url.URL{Fragment: u.Fragment, Path: filepath.ToSlash(newName)}
		// below dst is used instead of u.String() because we need to
		// keep exact same way link is written in text
		repl = append(repl, "("+escaper.Replace(dst)+")", "("+escaper.Replace(u2.String())+")")
		log.Printf("%s: broken link replacement: %q -> %q", name, u, u2)
		return ast.GoToNext
	}
	_ = ast.Walk(doc, ast.NodeVisitorFunc(walkFn))
	if len(repl) == 0 {
		return false, nil
	}
	r := strings.NewReplacer(repl...)
	return true, ioutil.WriteFile(name, []byte(r.Replace(string(b))), 0666)
}

func fileOrDirExists(name string) bool {
	fi, err := os.Stat(name)
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular() || fi.IsDir()
}

func fileExists(name string) bool {
	fi, err := os.Stat(name)
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular()
}

var escaper = strings.NewReplacer(
	`(`, `\(`,
	`)`, `\)`,
)

const extensions = parser.CommonExtensions | parser.AutoHeadingIDs ^ parser.MathJax

func init() {
	log.SetFlags(0)
}

//go:generate sh -c "go doc >README"
//go:generate usagegen -autohelp
