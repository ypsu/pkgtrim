package main

import (
	"bytes"
	"cmp"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/ypsu/textar"
)

func humanize(sz int64) string {
	return fmt.Sprintf("%7.1f MB", float64(sz)/1e6)
}

func getwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "/"
	}
	return wd
}

var wd = getwd()

// abspath makes a path into an absolute path.
// The leading / is removed so that it can be used with fs.FS.
func abspath(p string) string {
	if strings.HasPrefix(p, "/") {
		return p[1:]
	}
	return filepath.Join(wd, p)[1:]
}

func parseconfig(found map[string]struct{}, depth int, cfg []byte) error {
	if depth > 10 {
		return fmt.Errorf("too many nested commands")
	}
	for i, line := range strings.Split(string(cfg), "\n") {
		if strings.HasPrefix(line, "!") {
			output, err := exec.Command("sh", "-c", line[1:]).Output()
			if err, ok := err.(*exec.ExitError); ok {
				return fmt.Errorf("execute line %d: %q: %v, stderr: %s", i+1, line[1:], err, bytes.TrimSpace(err.Stderr))
			}
			if err != nil {
				return fmt.Errorf("execute line %d: %q: %v", i+1, line[1:], err)
			}
			if err := parseconfig(found, depth+1, output); err != nil {
				return fmt.Errorf("parse line %d: %q: %v", i+1, line[1:], err)
			}
			continue
		}
		pkgs, _, _ := strings.Cut(line, "#") // strip comments
		for _, pkg := range strings.Fields(pkgs) {
			found[pkg] = struct{}{}
		}
	}
	return nil
}

// makeRE makes a single regex from a set of globs.
func makeRE(globs ...string) *regexp.Regexp {
	expr := &strings.Builder{}
	expr.WriteString("^(")
	for i, glob := range globs {
		if i != 0 {
			expr.WriteByte('|')
		}
		parts := strings.Split(glob, "*")
		for i, part := range parts {
			parts[i] = regexp.QuoteMeta(part)
		}
		expr.WriteString(strings.Join(parts, ".*"))
	}
	expr.WriteString(")$")
	return regexp.MustCompile(expr.String())
}

func tonumber(v bool) int {
	if v {
		return 1
	}
	return 0
}

// Pkgtrim implements the tool's main functionality.
func Pkgtrim(w io.Writer, rootfs fs.FS, args []string) error {
	// Define and parse flags.
	argmode := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			argmode = true
		}
		if argmode && strings.HasPrefix(arg, "-") {
			return fmt.Errorf("flags must come before args and must have the -flag=value form")
		}
	}
	defaultTrimfile := filepath.Join(os.Getenv("HOME"), ".pkgtrim")
	var (
		flagset          = flag.NewFlagSet("pkgtrim", flag.ContinueOnError)
		flagDryrun       = flagset.Bool("dryrun", false, "Don't execute the -remove or -install commands.")
		flagDumpConfig   = flagset.Bool("dump_config", false, "Debug option: if true then dump the parsed config.")
		flagDumpPackages = flagset.Bool("dump_packages", false, "Debug option: if true then dump the list of packages and dependencies pkgtrim detected.")
		flagGraph        = flagset.Bool("graph", false, "Show the dependency graph of the arguments. Pipe the output to 'dot -Tx11' to visualize the graph.")
		flagInstall      = flagset.Bool("install", false, "Install the packages specified in .pkgtrim.")
		flagRemove       = flagset.Bool("remove", false, "Remove the selected packages and their unique dependencies or all unintentional packages and their dependencies if no arguments.")
		flagTestFS       = flagset.String("testfs", "", "Mock the filesystem with this textar file instead of using the real filesystem.")
		flagTrace        = flagset.Bool("trace", false, "If true, there must be two arguments, [package] and [dependency] and pkgtrim generates a dependency graph between the two. Pipe the output to 'dot -Tx11' to visualize the graph.")
		flagTrimfile     = flagset.String("f", defaultTrimfile, "The config file.")
	)
	flagset.SetOutput(w)
	if err := flagset.Parse(args); err != nil {
		return err
	}

	if tonumber(*flagInstall)+tonumber(*flagRemove)+tonumber(*flagTrace) >= 2 {
		return fmt.Errorf("only one action allowed")
	}

	if *flagTestFS != "" {
		data, err := fs.ReadFile(rootfs, abspath(*flagTestFS))
		if err != nil {
			return fmt.Errorf("load testfs: %v", err)
		}
		rootfs = textar.FS(textar.Parse(data))
	}

	system, err := NewPackageSystem(rootfs)
	if err != nil {
		return fmt.Errorf("detect package system: %v", err)
	}
	pkgs, err := system.Packages()
	if err != nil {
		return fmt.Errorf("load packages: %v", err)
	}
	slices.SortFunc(pkgs, func(a, b Package) int { return cmp.Compare(a.Name, b.Name) })

	if *flagDumpPackages {
		for _, pkg := range pkgs {
			fmt.Fprintf(w, "%s %d %s\n", pkg.Name, pkg.Size, strings.Join(pkg.Deps, " "))
		}
		return nil
	}

	// Parse ~/.pkgtrim.
	foundPackages := map[string]struct{}{}
	trimfileBytes, err := fs.ReadFile(rootfs, abspath(*flagTrimfile))
	if err != nil {
		if *flagTrimfile != defaultTrimfile {
			return fmt.Errorf("open trimfile: %v", err)
		}
	}
	if err := parseconfig(foundPackages, 0, trimfileBytes); err != nil {
		return fmt.Errorf("parse %s: %v", *flagTrimfile, err)
	}
	if *flagDumpConfig {
		fmt.Fprintln(w, strings.Join(slices.Sorted(maps.Keys(foundPackages)), "\n"))
		return nil
	}
	intentionalRE := makeRE(slices.Collect(maps.Keys(foundPackages))...)

	// To keep things efficient, keep things in []int32 arrays.
	type pkgid int32
	var (
		n        = len(pkgs)                 // number of packages
		toporder = make([]pkgid, 0, n)       // the topological order of the packages, built by traverse
		visited  = make([]bool, n)           // marker for traverse
		shared   = make([]bool, n)           // marker for determining the unique size
		deps     = make([][]pkgid, n)        // direct dependencies of a package
		rdeps    = make([][]pkgid, n)        // direct reverse dependencies of a package
		pkgids   = make(map[string]pkgid, n) // map package names to a number
		unique   = make([]int64, n)          // the total unique size used for each package
	)

	// Compute deps and rdeps.
	for i, p := range pkgs {
		pkgids[p.Name] = pkgid(i)
	}
	for i, p := range pkgs {
		deps[i] = make([]pkgid, len(p.Deps))
		for j, d := range p.Deps {
			deps[i][j] = pkgids[d]
			rdeps[pkgids[d]] = append(rdeps[pkgids[d]], pkgid(i))
		}
	}

	// Handle -install.
	if *flagInstall {
		ignored := make([]string, 0, 64)
		toinstall := make([]string, 0, 64)
		for _, pkg := range slices.Sorted(maps.Keys(foundPackages)) {
			if _, exists := pkgids[pkg]; exists {
				continue
			}
			if strings.IndexByte(pkg, '*') == -1 {
				toinstall = append(toinstall, pkg)
			} else {
				ignored = append(ignored, pkg)
			}
		}
		if len(ignored) > 0 {
			fmt.Fprintf(w, "Warning, ignoring globs: %s.\n", strings.Join(ignored, " "))
		}
		if len(toinstall) == 0 {
			fmt.Fprintln(w, "Nothing new to install.")
			return nil
		}
		argv := system.Install(toinstall)
		fmt.Fprintln(w, strings.Join(argv, " "))
		if *flagDryrun {
			return nil
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("install packages: %v", err)
		}
		return nil
	}

	// Runs a depth first search from a given node and builds toporder.
	var traverse func(pkgid)
	traverse = func(u pkgid) {
		if visited[u] {
			return
		}
		visited[u] = true
		for _, dep := range deps[u] {
			traverse(dep)
		}
		toporder = append(toporder, u)
	}

	// Computes the shared array and returns the unique size.
	// Should be called after traverse().
	computeUnique := func(seed ...pkgid) int64 {
		// A package is not unique in the ith package if it has an rdep that is already shared or is outside the visited packages.
		slices.Reverse(toporder)
		var uniqueSize int64
		for _, i := range toporder {
			if slices.Contains(seed, i) {
				uniqueSize += pkgs[i].Size
				continue
			}
			for _, j := range rdeps[i] {
				if shared[j] || !visited[j] {
					shared[i] = true
					break
				}
			}
			if !shared[i] {
				uniqueSize += pkgs[i].Size
			}
		}
		return uniqueSize
	}

	// Removes these packages but keeps the intentional ones.
	remove := func(toremove []string) error {
		toporder = toporder[:0]
		for i := range n {
			visited[i] = false
		}
		for _, pkg := range toremove {
			if intentionalRE.MatchString(pkg) {
				traverse(pkgids[pkg])
			}
		}
		tokeep := make([]string, 0, 64)
		toremove = slices.DeleteFunc(toremove, func(pkg string) bool {
			if visited[pkgids[pkg]] {
				tokeep = append(tokeep, pkg)
				return true
			}
			return false
		})
		if len(tokeep) > 0 {
			fmt.Fprintf(w, "Keeping packages intended directly or indirectly by %s: %s.\n\n", *flagTrimfile, strings.Join(tokeep, " "))
		}
		if len(toremove) == 0 {
			return fmt.Errorf("nothing to remove")
		}
		argv := system.Remove(toremove)
		fmt.Fprintln(w, strings.Join(argv, " "))
		if *flagDryrun {
			return nil
		}
		fmt.Fprintln(w)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("remove selected packages: %v", err)
		}
		return nil
	}

	if *flagGraph {
		if flagset.NArg() == 0 {
			return fmt.Errorf("-graph requires some arguments, got none")
		}
		for _, pkg := range flagset.Args() {
			p, exists := pkgids[pkg]
			if !exists {
				return fmt.Errorf("package %s not found", pkg)
			}
			traverse(p)
		}
		fmt.Fprintln(w, "digraph {")
		for _, arg := range flagset.Args() {
			fmt.Fprintf(w, "  \"%s\" [style=filled fillcolor=lightgray]\n", arg)
		}
		for i := range n {
			if !visited[i] {
				continue
			}
			visited[i] = false
			for _, j := range deps[i] {
				fmt.Fprintf(w, "  \"%s\" -> \"%s\"\n", pkgs[i].Name, pkgs[j].Name)
			}
		}
		deps, rdeps, toporder = rdeps, deps, toporder[:0]
		for _, pkg := range flagset.Args() {
			traverse(pkgids[pkg])
		}
		for i := range n {
			if !visited[i] {
				continue
			}
			for _, j := range deps[i] {
				fmt.Fprintf(w, "  \"%s\" -> \"%s\"\n", pkgs[j].Name, pkgs[i].Name)
			}
		}
		fmt.Fprintln(w, "}")
		return nil
	}

	if *flagTrace {
		if flagset.NArg() != 2 {
			return fmt.Errorf("-trace requires exactly 2 arguments, got %d", flagset.NArg())
		}
		src, srcExists := pkgids[flagset.Arg(0)]
		dst, dstExists := pkgids[flagset.Arg(1)]
		if !srcExists {
			return fmt.Errorf("package %s not found", flagset.Arg(0))
		}
		if !dstExists {
			return fmt.Errorf("package %s not found", flagset.Arg(1))
		}
		traverse(src)
		if !visited[dst] {
			return fmt.Errorf("package %s is not a dependency of %s", flagset.Arg(1), flagset.Arg(0))
		}
		fmt.Fprintf(w, "strict digraph {\n  \"%s\" [style=filled fillcolor=lightgray]\n  \"%s\" [style=filled fillcolor=lightgray]\n", flagset.Arg(0), flagset.Arg(1))
		path := make([]string, 0, 64)
		var findpaths func(pkgid)
		findpaths = func(pkg pkgid) {
			if pkg == src {
				slices.Reverse(path)
				fmt.Fprintf(w, "  \"%s\"\n", strings.Join(path, "\" -> \""))
				slices.Reverse(path)
				return
			}
			for _, rdep := range rdeps[pkg] {
				if visited[rdep] {
					path = append(path, pkgs[rdep].Name)
					findpaths(rdep)
					path = path[:len(path)-1]
				}
			}
		}
		path = append(path, flagset.Arg(1))
		findpaths(dst)
		fmt.Fprintln(w, "}")
		return nil
	}

	if flagset.NArg() > 0 {
		seed := make([]pkgid, flagset.NArg())
		for i, pkg := range flagset.Args() {
			id, exists := pkgids[pkg]
			if !exists {
				return fmt.Errorf("package %s not installed", pkg)
			}
			traverse(id)
			seed[i] = id
		}
		computeUnique(seed...)
		var (
			sharedsize        int64
			uniquesize        int64
			sharedpkgs        = make([]string, 0, n) // dependencies that other packages also have
			uniquepkgs        = make([]string, 0, n) // dependencies unique to the arguments
			intentionalpkgs   = make([]string, 0, n) // top level rdeps that are present in .pkgtrim
			unintentionalpkgs = make([]string, 0, n) // top level rdeps that are not present in .pkgtrim
		)
		for i, pkg := range pkgs {
			if shared[i] {
				sharedsize += pkg.Size
				sharedpkgs = append(sharedpkgs, pkg.Name)
			} else if visited[i] {
				uniquesize += pkg.Size
				uniquepkgs = append(uniquepkgs, pkg.Name)
			}
			shared[i], visited[i] = false, false
		}

		// Compute top level rdeps by running bfs in reverse.
		deps, rdeps, toporder = rdeps, deps, toporder[:0]
		for _, i := range seed {
			traverse(i)
		}
		deps, rdeps, toporder = rdeps, deps, toporder[:0]
		for i, pkg := range pkgs {
			if !visited[i] || len(rdeps[i]) > 0 {
				continue
			}
			if intentionalRE.MatchString(pkg.Name) {
				intentionalpkgs = append(intentionalpkgs, pkg.Name)
			} else {
				unintentionalpkgs = append(unintentionalpkgs, pkg.Name)
			}
		}

		fmt.Fprintf(w, "shared dependencies (%s): %s\n\n", humanize(sharedsize), strings.Join(sharedpkgs, " "))
		fmt.Fprintf(w, "unique dependencies (%s): %s\n\n", humanize(uniquesize), strings.Join(uniquepkgs, " "))
		fmt.Fprintf(w, "intentional top level rdeps: %s\n\n", strings.Join(intentionalpkgs, " "))
		fmt.Fprintf(w, "unintentional top level rdeps: %s\n\n", strings.Join(unintentionalpkgs, " "))

		if *flagRemove {
			return remove(uniquepkgs)
		}
		return nil
	}

	// No args mode.
	// For each top level undocumented package compute the total and unique usage via a breadth first search.
	cnt := 0
	for i := range n {
		if len(rdeps[i]) > 0 || intentionalRE.MatchString(pkgs[i].Name) {
			continue
		}
		cnt++
		traverse(pkgid(i))
		unique[i] = computeUnique(pkgid(i))

		// Reset the arrays for the next iteration.
		for _, j := range toporder {
			visited[j], shared[j] = false, false
		}
		toporder = toporder[:0]
	}
	if cnt == 0 && !*flagRemove {
		fmt.Fprintln(w, "No unintenional packages found. Use `-f /dev/null` to print all.")
		return nil
	}

	sizeorder := make([]pkgid, n)
	for i := range n {
		sizeorder[i] = pkgid(i)
	}
	slices.SortFunc(sizeorder, func(a, b pkgid) int {
		return cmp.Compare(unique[a], unique[b])
	})

	for _, id := range sizeorder {
		pkg := pkgs[id]
		if len(rdeps[id]) > 0 || intentionalRE.MatchString(pkg.Name) {
			continue
		}
		fmt.Fprintf(w, "%s %-24s %s\n", humanize(unique[id]), pkg.Name, pkg.Desc)
	}

	if *flagRemove {
		for i, pkg := range pkgs {
			if len(rdeps[i]) == 0 && !intentionalRE.MatchString(pkg.Name) {
				traverse(pkgid(i))
			}
		}
		computeUnique()
		toremove := make([]string, 0, 64)
		for _, i := range toporder {
			if !shared[i] {
				toremove = append(toremove, pkgs[i].Name)
			}
		}
		slices.Sort(toremove)
		return remove(toremove)
	}
	return nil
}
