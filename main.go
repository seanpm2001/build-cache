// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Peter Mattis (peter.mattis@gmail.com)

package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"hash"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

var raceF = flag.Bool("race", false, "")

type packageList []*Package

func (p packageList) Len() int {
	return len(p)
}

func (p packageList) Less(i, j int) bool {
	return p[i].ImportPath < p[j].ImportPath
}

func (p packageList) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

// Package represents the info output by "go list -json". The
// structure definition is taken verbatim from "go help list".
type Package struct {
	Dir           string // directory containing package sources
	ImportPath    string // import path of package in dir
	ImportComment string // path in import comment on package statement
	Name          string // package name
	Doc           string // package documentation string
	Target        string // install path
	Goroot        bool   // is this package in the Go root?
	Standard      bool   // is this package part of the standard Go library?
	Stale         bool   // would 'go install' do anything for this package?
	Root          string // Go root or Go path dir containing this package

	// Source files
	GoFiles        []string // .go source files (excluding CgoFiles, TestGoFiles, XTestGoFiles)
	CgoFiles       []string // .go sources files that import "C"
	IgnoredGoFiles []string // .go sources ignored due to build constraints
	CFiles         []string // .c source files
	CXXFiles       []string // .cc, .cxx and .cpp source files
	MFiles         []string // .m source files
	HFiles         []string // .h, .hh, .hpp and .hxx source files
	SFiles         []string // .s source files
	SwigFiles      []string // .swig files
	SwigCXXFiles   []string // .swigcxx files
	SysoFiles      []string // .syso object files to add to archive

	// Cgo directives
	CgoCFLAGS    []string // cgo: flags for C compiler
	CgoCPPFLAGS  []string // cgo: flags for C preprocessor
	CgoCXXFLAGS  []string // cgo: flags for C++ compiler
	CgoLDFLAGS   []string // cgo: flags for linker
	CgoPkgConfig []string // cgo: pkg-config names

	// Dependency information
	Imports []string // import paths used by this package
	Deps    []string // all (recursively) imported dependencies

	// Error information
	Incomplete bool            // this package or a dependency has an error
	Error      *PackageError   // error loading package
	DepsErrors []*PackageError // errors loading dependencies

	TestGoFiles  []string // _test.go files in package
	TestImports  []string // imports from TestGoFiles
	XTestGoFiles []string // _test.go files outside package
	XTestImports []string // imports from XTestGoFiles

	fingerprint *string
}

// PackageError represents an error in loading a package. The
// structure definition is taken from the "go" tool source code.
type PackageError struct {
	ImportStack []string // shortest path from package named on command line to this one
	Pos         string   // position of error
	Err         string   // the error itself
}

func (p *Package) addFile(h hash.Hash, file string) {
	_, err := h.Write([]byte(file))
	if err != nil {
		log.Fatal(err)
	}
	f, err := os.Open(filepath.Join(p.Dir, file))
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		log.Fatal(err)
	}
}

func (p *Package) addFiles(h hash.Hash, files []string) {
	for _, file := range files {
		p.addFile(h, file)
	}
}

func (p *Package) addFlags(h hash.Hash, flags []string) {
	for _, flag := range flags {
		_, err := h.Write([]byte(flag))
		if err != nil {
			log.Fatal(err)
		}
	}
}

// Fingerprint the package returning a digest that changes if any of
// the sources of the packages or its dependencies change.
func (p *Package) Fingerprint(pkgs map[string]*Package) string {
	if p.fingerprint != nil {
		return *p.fingerprint
	}

	h := sha1.New()
	// TODO(pmattis): I need to add the output of "go version", not the
	// version/GOOS/GOARCH that build-cache was compiled with.
	p.addFlags(h, []string{
		runtime.Version(),
		runtime.GOOS,
		runtime.GOARCH,
		p.ImportPath})
	if *raceF {
		p.addFlags(h, []string{"race"})
	}
	p.addFiles(h, p.GoFiles)
	p.addFiles(h, p.CgoFiles)
	p.addFiles(h, p.CFiles)
	p.addFiles(h, p.CXXFiles)
	p.addFiles(h, p.MFiles)
	p.addFiles(h, p.HFiles)
	p.addFiles(h, p.SFiles)
	p.addFiles(h, p.SwigFiles)
	p.addFiles(h, p.SwigCXXFiles)
	p.addFiles(h, p.SysoFiles)
	p.addFlags(h, p.CgoCFLAGS)
	p.addFlags(h, p.CgoCPPFLAGS)
	p.addFlags(h, p.CgoCXXFLAGS)
	p.addFlags(h, p.CgoLDFLAGS)
	p.addFlags(h, p.CgoPkgConfig)
	for _, dep := range p.Deps {
		if !*raceF && isStdLib(dep) {
			continue
		}
		pkg, ok := pkgs[dep]
		if !ok {
			log.Fatalf("%s not found!", dep)
		}
		fp := pkg.Fingerprint(pkgs)
		if fp == "" {
			p.fingerprint = &fp
			return *p.fingerprint
		}
		_, err := h.Write([]byte(fp))
		if err != nil {
			log.Fatal(err)
		}
	}
	s := hex.EncodeToString(h.Sum(nil))
	p.fingerprint = &s
	return *p.fingerprint
}

func prettyJSON(v interface{}) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	return string(b)
}

func goList(dir string) (*Package, error) {
	args := []string{"list", "-json"}
	if *raceF {
		args = append(args, "-race")
		args = append(args, "-installsuffix=race")
	}
	args = append(args, dir)
	c := exec.Command("go", args...)
	output, err := c.CombinedOutput()
	if err != nil {
		log.Fatalf("%s\n%s", err, output)
	}
	pkg := &Package{}
	if err := json.Unmarshal(output, pkg); err != nil {
		return nil, err
	}
	return pkg, nil
}

func isStdLib(pkgName string) bool {
	dot := strings.IndexByte(pkgName, '.')
	if dot == -1 {
		return true
	}
	slash := strings.IndexByte(pkgName, '/')
	return dot > slash
}

func exists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

func cacheDir() string {
	d := os.Getenv("CACHE")
	if d == "" {
		d = os.ExpandEnv("${HOME}/buildcache")
	}
	return d
}

func linkOrCopy(src, dst string) error {
	if exists(dst) {
		return nil
	}
	if err := os.Link(src, dst); err == nil || os.IsExist(err) {
		return nil
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()
	if err := dstFile.Chmod(srcInfo.Mode() & os.ModePerm); err != nil {
		_ = os.Remove(dst)
		return err
	}
	_, err = io.Copy(dstFile, srcFile)
	return err
}

func loadPackages(pkgs map[string]*Package, importPath string) *Package {
	if pkg := pkgs[importPath]; pkg != nil {
		return pkg
	}
	pkg, err := goList(importPath)
	if err != nil {
		log.Fatal(err)
	}
	pkgs[pkg.ImportPath] = pkg
	for _, dep := range pkg.Deps {
		if !*raceF && isStdLib(dep) {
			continue
		}
		loadPackages(pkgs, dep)
	}
	return pkg
}

func load(dir string) (map[string]*Package, []*Package) {
	pkgMap := map[string]*Package{}
	root := loadPackages(pkgMap, dir)

	var rootPkgs []*Package
	for importPath, pkg := range pkgMap {
		if !strings.HasPrefix(importPath, root.ImportPath) {
			continue
		}
		rootPkgs = append(rootPkgs, pkg)
	}
	for _, pkg := range rootPkgs {
		for _, dep := range pkg.TestImports {
			if !*raceF && isStdLib(dep) {
				continue
			}
			loadPackages(pkgMap, dep)
		}
	}

	var pkgList []*Package
	for _, pkg := range pkgMap {
		pkgList = append(pkgList, pkg)
	}
	sort.Sort(packageList(pkgList))
	return pkgMap, pkgList
}

func save(args []string) {
	if len(args) > 2 {
		log.Fatalf("usage: %s save [package-path]", os.Args[0])
	}
	path := "."
	if len(args) == 1 {
		path = args[0]
	}

	dir := cacheDir()
	log.Printf("saving %s to %s", path, dir)
	if err := os.Mkdir(dir, 0755); err != nil && !os.IsExist(err) {
		log.Fatal(err)
	}

	pkgMap, pkgList := load(path)
	for _, pkg := range pkgList {
		if pkg.Stale || !exists(pkg.Target) {
			log.Printf("%-40s  %s (%s)", "-", pkg.ImportPath, pkg.Target)
		} else {
			fp := pkg.Fingerprint(pkgMap)
			tag := "*"
			if err := linkOrCopy(pkg.Target, filepath.Join(dir, fp)); err != nil {
				if !os.IsExist(err) {
					log.Fatal(err)
				}
				tag = " "
			}
			log.Printf("%-40s %s%s (%s)", fp, tag, pkg.ImportPath, pkg.Target)
		}
	}
}

func restore(args []string) {
	if len(args) > 2 {
		log.Fatalf("usage: %s restore [package-path]", os.Args[0])
	}
	path := "."
	if len(args) == 1 {
		path = args[0]
	}

	dir := cacheDir()
	if !exists(dir) {
		log.Printf("%s does not exist", dir)
		os.Exit(0)
	}
	log.Printf("restoring %s from %s", path, dir)

	pkgMap, pkgList := load(path)
	now := time.Now()
	for _, pkg := range pkgList {
		fp := pkg.Fingerprint(pkgMap)
		src := filepath.Join(dir, fp)
		if !exists(src) {
			log.Printf("%-40s  %s (%s:%s)", "-", pkg.ImportPath, fp, pkg.Target)
		} else {
			log.Printf("%-40s  %s (%s)", fp, pkg.ImportPath, pkg.Target)
			_ = os.Remove(pkg.Target)
			_ = os.MkdirAll(filepath.Dir(pkg.Target), 0755)
			if err := linkOrCopy(src, pkg.Target); err != nil {
				log.Fatal(err)
			}
			if err := os.Chtimes(pkg.Target, now, now); err != nil {
				log.Fatal(err)
			}
		}
	}
}

func clear(args []string) {
	// TODO(pmattis): Instead of removing everything, only clear entries
	// that are older than a day or week.
	dir := cacheDir()
	log.Printf("clearing %s", dir)
	if err := os.RemoveAll(dir); err != nil {
		log.Fatal(err)
	}
}

func main() {
	log.SetFlags(0)

	flag.Parse()
	args := flag.Args()

	if len(args) >= 1 {
		switch args[0] {
		case "save":
			save(args[1:])
			return
		case "restore":
			restore(args[1:])
			return
		case "clear":
			clear(args[1:])
			return
		}
		log.Printf("unknown command \"%s\"\n\n", args[0])
	}

	log.Printf("usage: %s [save|restore|clear]", os.Args[0])
	os.Exit(1)
}
