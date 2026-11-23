// Command notices checks the licenses of everything a dun release ships and
// writes the third-party notices that go in the release (aw4 issue
// i_1m5r9bxcp1g4ayg6hx407n).
//
// A release binary links Go modules and embeds the web app (mobile/) with its
// npm packages. MIT and BSD require their copyright notice in every copy,
// binaries included, and Apache-2.0 also requires each NOTICE file; a minified
// bundle keeps none of them. So a release carries THIRD_PARTY_NOTICES.md, and
// a dependency under a license that is not on the allow-list fails the build.
//
// Run from the repo root, after `pnpm install --frozen-lockfile` in mobile/:
//
//	go run ./tools/notices -o THIRD_PARTY_NOTICES.md
//
// Go modules: the union over the release platforms of the modules the
// release-tagged cmd/dun links, listed with GOWORK=off (the release builds
// without a go.work). npm: the production dependencies of mobile/ and their
// transitive production dependencies, walked from pnpm-lock.yaml; dev
// dependencies build the bundle and are not shipped. The output is sorted and
// carries no dates, so the same inputs give the same bytes.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// allowed are the licenses dun (MIT) can ship without further terms.
var allowed = map[string]bool{
	"MIT": true, "BSD-2-Clause": true, "BSD-3-Clause": true, "Apache-2.0": true,
	"ISC": true, "0BSD": true, "Unlicense": true, "CC0-1.0": true, "Public-Domain": true,
	// Permissive, found in the npm tree (2026-11-23): BlueOak (graceful-fs,
	// minimatch, sax), Python-2.0 (argparse), CC-BY-4.0 (caniuse-lite's data;
	// attribution only, which the notices carry). MPL-2.0 and other copyleft
	// stay off until the owner decides.
	"BlueOak-1.0.0": true, "Python-2.0": true, "CC-BY-4.0": true,
}

// reviewed are license files that classify as unknown and were read by hand.
// Keyed by module path and file name, so a new unknown file fails until
// someone reads it. The value is an allowed license, or any other text for a
// file that is not a license (it then settles nothing).
var reviewed = map[string]string{
	"modernc.org/sqlite LICENSE-SQLITE": "Public-Domain", // SQLite's public-domain dedication
	"modernc.org/memory LICENSE-LOGO":   "attribution for a logo image; not linked code",
	// An index of the driver's components and their licenses, not a license.
	// It quotes MPL-2.0 in full, whose "Secondary License" definition names the
	// GNU Affero GPL, so the AGPL rule matches it. Every component linked into
	// a program is MIT, BSD-3-Clause or public domain; MPL-2.0 appears only in
	// its section 3. Re-read it when the driver moves.
	"modernc.org/sqlite LICENSE-3RD-PARTY.md": "index of component licenses; not a license",
}

// exceptions are packages (Go module path or npm name) the owner approved
// although their license is not on the allow-list, from the exceptions file:
// one "<name> <reason>" per line, # for comments. An empty file approves none.
var exceptions = map[string]string{}

func loadExceptions(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, reason, _ := strings.Cut(line, " ")
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("%s: %q has no reason", path, name)
		}
		exceptions[name] = reason
	}
	return nil
}

// rules classify license text. Order matters: copyleft first, so a text that
// quotes another license is caught by the stricter one.
var rules = []struct {
	name string
	rx   *regexp.Regexp
}{
	{"AGPL", regexp.MustCompile(`(?i)GNU AFFERO GENERAL PUBLIC LICENSE`)},
	{"LGPL", regexp.MustCompile(`(?i)GNU LESSER GENERAL PUBLIC LICENSE|GNU LIBRARY GENERAL PUBLIC`)},
	{"GPL", regexp.MustCompile(`(?i)GNU GENERAL PUBLIC LICENSE`)},
	{"MPL-2.0", regexp.MustCompile(`(?i)Mozilla Public License,? (Version|v\.) ?2\.0`)},
	{"Apache-2.0", regexp.MustCompile(`(?i)Apache License,? Version 2\.0`)},
	{"BSD-3-Clause", regexp.MustCompile(`(?i)Neither the name|names of (its|the) contributors may be used`)},
	{"BSD-2-Clause", regexp.MustCompile(`(?i)Redistributions in binary form must reproduce`)},
	{"MIT", regexp.MustCompile(`(?i)Permission is hereby granted, free of charge`)},
	{"ISC", regexp.MustCompile(`(?i)Permission to use, copy, modify, and(/or)? distribute this software for any`)},
	{"Unlicense", regexp.MustCompile(`(?i)This is free and unencumbered software`)},
	{"CC0-1.0", regexp.MustCompile(`(?i)CC0 1\.0`)},
}

// classify names the license a text is, "" when no rule matches.
func classify(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	for _, r := range rules {
		if r.rx.MatchString(t) {
			return r.name
		}
	}
	return ""
}

var licenseFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|unlicense)`)
var noticeFile = regexp.MustCompile(`(?i)^notice`)

// releasePlatforms are the GOOS/GOARCH pairs `make release` builds.
var releasePlatforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"}

// pkg is one shipped dependency.
type pkg struct {
	Kind, Name, Version, License string
	Texts                        []text
}

type text struct{ File, Body string }

func main() {
	out := flag.String("o", "", "write the notices here (default: check only)")
	mobile := flag.String("mobile", "mobile", "the web app's directory (pnpm-lock.yaml, node_modules)")
	exc := flag.String("exceptions", "tools/notices/exceptions.txt", "packages the owner approved despite their license")
	flag.Parse()
	if err := loadExceptions(*exc); err != nil {
		fail(err)
	}
	var pkgs []pkg
	var problems []string
	gp, gprob, err := goModules("./cmd/dun")
	if err != nil {
		fail(err)
	}
	pkgs, problems = append(pkgs, gp...), append(problems, gprob...)
	np, nprob, err := npmPackages(*mobile)
	if err != nil {
		fail(err)
	}
	pkgs, problems = append(pkgs, np...), append(problems, nprob...)
	if len(problems) > 0 {
		sort.Strings(problems)
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "notices:", p)
		}
		fail(fmt.Errorf("%d dependency license problem(s)", len(problems)))
	}
	counts := map[string]int{}
	kinds := map[string]int{}
	for _, p := range pkgs {
		counts[p.License]++
		kinds[p.Kind]++
	}
	fmt.Fprintf(os.Stderr, "notices: %d go modules, %d npm packages, all allowed: %v\n", kinds["go"], kinds["npm"], counts)
	if *out != "" {
		if err := os.WriteFile(*out, render(pkgs), 0o644); err != nil {
			fail(err)
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "notices:", err)
	os.Exit(1)
}

// goModules is every module the main package links on any release platform,
// but the main module.
func goModules(mainPkg string) ([]pkg, []string, error) {
	seen := map[string]bool{}
	var lines []string
	for _, plat := range releasePlatforms {
		goos, goarch, _ := strings.Cut(plat, "/")
		cmd := exec.Command("go", "list", "-e", "-deps", "-tags", "release", "-f",
			"{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}{{end}}", mainPkg)
		cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
		cmd.Stderr = os.Stderr
		b, err := cmd.Output()
		if err != nil {
			return nil, nil, fmt.Errorf("go list (%s): %w", plat, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line != "" && !seen[line] {
				seen[line] = true
				lines = append(lines, line)
			}
		}
	}
	var pkgs []pkg
	var problems []string
	for _, line := range lines {
		f := strings.Split(line, "\t")
		if len(f) != 3 || f[2] == "" {
			problems = append(problems, "go module with no source directory: "+line)
			continue
		}
		p, prob := goModule(f[0], f[1], f[2])
		pkgs, problems = append(pkgs, p), append(problems, prob...)
	}
	return pkgs, problems, nil
}

// goModule reads one module's license and NOTICE files. Every license file
// must classify as allowed or be reviewed; the module's license is its first
// classified one.
func goModule(path, version, dir string) (pkg, []string) {
	p := pkg{Kind: "go", Name: path, Version: version}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return p, []string{fmt.Sprintf("%s: %v", path, err)}
	}
	var problems []string
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !(licenseFile.MatchString(name) || noticeFile.MatchString(name)) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s %s: %v", path, name, err))
			continue
		}
		p.Texts = append(p.Texts, text{File: name, Body: string(b)})
		if noticeFile.MatchString(name) {
			continue
		}
		// A reviewed file is settled by the review, not the rules: some are
		// texts no rule recognises, and some quote a license they are not.
		if r, ok := reviewed[path+" "+name]; ok {
			if allowed[r] && p.License == "" {
				p.License = r
			}
			continue
		}
		lic := classify(string(b))
		if lic == "" {
			problems = append(problems, fmt.Sprintf("%s %s: license text not recognised; read it and add it to reviewed", path, name))
			continue
		}
		if _, ok := exceptions[path]; !ok && !allowed[lic] {
			problems = append(problems, fmt.Sprintf("%s %s: %s is not on the allow-list", path, name, lic))
		}
		if p.License == "" {
			p.License = lic
		}
	}
	if p.License == "" && len(problems) == 0 {
		problems = append(problems, path+": no license file")
	}
	return p, problems
}

// lockSnapshot is one pnpm-lock.yaml snapshot's production edges.
type lockSnapshot struct{ deps []string }

// parseLock reads the two parts of pnpm-lock.yaml the walk needs: the root
// importer's production dependencies, and every snapshot's dependencies and
// optionalDependencies (peers are resolved into dependencies there). The file
// is machine-written YAML of fixed shape, so this reads it by indentation.
func parseLock(data string) (roots []string, snaps map[string]*lockSnapshot) {
	snaps = map[string]*lockSnapshot{}
	unq := func(s string) string { return strings.Trim(strings.TrimSpace(s), `'"`) }
	section, importer, group := "", "", ""
	var cur *lockSnapshot
	var depName string
	for _, line := range strings.Split(data, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		t := strings.TrimSpace(line)
		switch {
		case indent == 0:
			section = strings.TrimSuffix(t, ":")
			importer, group, cur = "", "", nil
		case section == "importers" && indent == 2:
			importer = strings.TrimSuffix(t, ":")
		case section == "importers" && importer == "." && indent == 4:
			group = strings.TrimSuffix(t, ":")
		case section == "importers" && importer == "." && indent == 6 && (group == "dependencies" || group == "optionalDependencies"):
			depName = unq(strings.TrimSuffix(t, ":"))
		case section == "importers" && importer == "." && indent == 8 && (group == "dependencies" || group == "optionalDependencies") && strings.HasPrefix(t, "version:"):
			roots = append(roots, depKey(depName, unq(strings.TrimPrefix(t, "version:"))))
		case section == "snapshots" && indent == 2:
			cur = &lockSnapshot{}
			snaps[unq(strings.TrimSuffix(t, ":"))] = cur
			group = ""
		case section == "snapshots" && indent == 4 && cur != nil:
			group = strings.TrimSuffix(t, ":")
		case section == "snapshots" && indent == 6 && cur != nil && (group == "dependencies" || group == "optionalDependencies"):
			name, ver, ok := strings.Cut(t, ": ")
			if ok {
				cur.deps = append(cur.deps, depKey(unq(name), unq(ver)))
			}
		}
	}
	return roots, snaps
}

// depKey is the snapshot key of a dependency edge: name@version, or the
// target of an npm: alias.
func depKey(name, version string) string {
	if strings.HasPrefix(version, "npm:") {
		return strings.TrimPrefix(version, "npm:")
	}
	return name + "@" + version
}

// keyName splits a snapshot key (name@version(peers)) into name and version
// without the peer suffix.
func keyName(key string) (name, version string) {
	k, _, _ := strings.Cut(key, "(")
	i := strings.LastIndex(k, "@")
	return k[:i], k[i+1:]
}

// npmPackages is mobile/'s production dependency tree: what the bundle can
// contain. Dev tools (jest, codegen, tsc) build it and are not shipped.
func npmPackages(dir string) ([]pkg, []string, error) {
	lock, err := os.ReadFile(filepath.Join(dir, "pnpm-lock.yaml"))
	if err != nil {
		return nil, nil, err
	}
	roots, snaps := parseLock(string(lock))
	if len(roots) == 0 || len(snaps) == 0 {
		return nil, nil, fmt.Errorf("%s/pnpm-lock.yaml: no production dependencies read; the file's shape changed", dir)
	}
	// Where each name@version is installed. mobile/.npmrc hoists
	// (node-linker=hoisted): top-level node_modules, with a nested
	// node_modules where a version differs.
	installed := map[string]string{}
	if err := scanModules(filepath.Join(dir, "node_modules"), installed); err != nil {
		return nil, nil, fmt.Errorf("%s (run pnpm install --frozen-lockfile in %s first): %w", dir, dir, err)
	}
	find := func(name, version string) string { return installed[name+"@"+version] }
	seen := map[string]bool{}
	var pkgs []pkg
	var problems []string
	var visit func(key string)
	visit = func(key string) {
		name, version := keyName(key)
		id := name + "@" + version
		if seen[id] {
			return
		}
		seen[id] = true
		s := snaps[key]
		if s != nil {
			for _, d := range s.deps {
				visit(d)
			}
		}
		path := find(name, version)
		if path == "" {
			// pnpm does not install an optional package built for another
			// platform; the app does not ship it from this host either.
			if isPlatformOptional(name) {
				return
			}
			problems = append(problems, "npm package not installed: "+id)
			return
		}
		p := pkg{Kind: "npm", Name: name, Version: version}
		b, err := os.ReadFile(filepath.Join(path, "package.json"))
		if err != nil {
			problems = append(problems, "npm "+id+": "+err.Error())
			return
		}
		var pj struct {
			License  any `json:"license"`
			Licenses []struct {
				Type string `json:"type"`
			} `json:"licenses"`
		}
		if err := json.Unmarshal(b, &pj); err != nil {
			problems = append(problems, "npm "+id+": package.json: "+err.Error())
			return
		}
		p.License = spdx(pj.License)
		if p.License == "" && len(pj.Licenses) > 0 {
			var ts []string
			for _, l := range pj.Licenses {
				ts = append(ts, l.Type)
			}
			p.License = "(" + strings.Join(ts, " OR ") + ")"
		}
		if es, err := os.ReadDir(path); err == nil {
			for _, e := range es {
				if !e.IsDir() && (licenseFile.MatchString(e.Name()) || noticeFile.MatchString(e.Name())) {
					if b, err := os.ReadFile(filepath.Join(path, e.Name())); err == nil {
						p.Texts = append(p.Texts, text{File: e.Name(), Body: string(b)})
					}
				}
			}
		}
		if _, ok := exceptions[name]; !ok && !npmAllowed(p.License) {
			problems = append(problems, fmt.Sprintf("npm %s: license %q is not on the allow-list", id, p.License))
		}
		pkgs = append(pkgs, p)
	}
	for _, r := range roots {
		visit(r)
	}
	return pkgs, problems, nil
}

// scanModules records every package under a node_modules directory, and
// under the node_modules nested in each, as name@version -> directory. The
// first directory found for a name@version wins; the copies are the same.
func scanModules(nm string, out map[string]string) error {
	ents, err := os.ReadDir(nm)
	if err != nil {
		return err
	}
	var pkgDirs []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".") || !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			continue
		}
		if strings.HasPrefix(n, "@") {
			sub, err := os.ReadDir(filepath.Join(nm, n))
			if err != nil {
				continue
			}
			for _, se := range sub {
				pkgDirs = append(pkgDirs, filepath.Join(nm, n, se.Name()))
			}
			continue
		}
		pkgDirs = append(pkgDirs, filepath.Join(nm, n))
	}
	for _, d := range pkgDirs {
		b, err := os.ReadFile(filepath.Join(d, "package.json"))
		if err != nil {
			continue
		}
		var pj struct{ Name, Version string }
		if json.Unmarshal(b, &pj) == nil && pj.Name != "" && pj.Version != "" {
			if _, ok := out[pj.Name+"@"+pj.Version]; !ok {
				out[pj.Name+"@"+pj.Version] = d
			}
		}
		if _, err := os.Stat(filepath.Join(d, "node_modules")); err == nil {
			if err := scanModules(filepath.Join(d, "node_modules"), out); err != nil {
				return err
			}
		}
	}
	return nil
}

// platformOptional names the native-binary packages pnpm skips on a host they
// do not fit (<tool>-<os>-<arch>: esbuild, swc, lightningcss and the like).
var platformOptional = regexp.MustCompile(`-(darwin|linux|win32|android|freebsd|openbsd|netbsd|sunos|aix|openharmony|wasm32)(-|$)`)

func isPlatformOptional(name string) bool { return platformOptional.MatchString(name) }

// spdx is a package.json license field as one string.
func spdx(v any) string {
	switch l := v.(type) {
	case string:
		return l
	case map[string]any:
		s, _ := l["type"].(string)
		return s
	}
	return ""
}

// npmAllowed accepts an SPDX expression whose every alternative of an OR, or
// every part of an AND, is allowed.
func npmAllowed(expr string) bool {
	e := strings.Trim(strings.TrimSpace(expr), "()")
	if e == "" {
		return false
	}
	if parts := strings.Split(e, " OR "); len(parts) > 1 {
		for _, p := range parts {
			if npmAllowed(p) {
				return true
			}
		}
		return false
	}
	for _, p := range strings.Split(e, " AND ") {
		if !allowed[strings.Trim(strings.TrimSpace(p), "()")] {
			return false
		}
	}
	return true
}

func render(pkgs []pkg) []byte {
	sort.Slice(pkgs, func(i, j int) bool {
		a, b := pkgs[i], pkgs[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Version < b.Version
	})
	var b bytes.Buffer
	b.WriteString("# Third-party notices\n\n")
	b.WriteString("dun is MIT licensed (LICENSE). This release also contains the software\n")
	b.WriteString("below, each under its own license, reproduced here as those licenses require.\n")
	b.WriteString("Generated by tools/notices.\n\n")
	for _, p := range pkgs {
		fmt.Fprintf(&b, "- %s %s %s — %s\n", p.Kind, p.Name, p.Version, p.License)
	}
	for _, p := range pkgs {
		fmt.Fprintf(&b, "\n---\n\n## %s %s (%s)\n", p.Name, p.Version, p.License)
		if len(p.Texts) == 0 {
			fmt.Fprintf(&b, "\nLicense: %s (declared in package.json; no license file shipped)\n", p.License)
		}
		for _, t := range p.Texts {
			fmt.Fprintf(&b, "\n### %s\n\n```\n%s\n```\n", t.File, strings.TrimRight(t.Body, "\n"))
		}
	}
	return b.Bytes()
}
