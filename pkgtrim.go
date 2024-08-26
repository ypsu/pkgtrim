package main

import (
	"cmp"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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

func parseconfig(dst map[string]string, depth int, cfg []byte) error {
	if depth > 10 {
		return fmt.Errorf("too many nested commands")
	}
	curcomment, commentmode := "", false
	for i, line := range strings.Split(string(cfg), "\n") {
		if strings.HasPrefix(line, "#") {
			// Grab the first line of each comment group.
			if !commentmode {
				curcomment, commentmode = strings.TrimSpace(line[1:]), true
			}
			continue
		}
		commentmode = false
		if strings.HasPrefix(line, "!") {
			output, err := exec.Command("sh", "-c", line[1:]).Output()
			if err != nil {
				return fmt.Errorf("execute line %d: %q: %v", i+1, line[1:], err)
			}
			if err := parseconfig(dst, depth+1, output); err != nil {
				return fmt.Errorf("parse line %d: %q: %v", i+1, line[1:], err)
			}
			continue
		}
		pkgs, _, _ := strings.Cut(line, "#") // strip side comments
		for _, pkg := range strings.Fields(pkgs) {
			dst[pkg] = curcomment
		}
	}
	return nil
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
		flagDumpConfig   = flagset.Bool("dump_config", false, "Debug option: if true then dump the parsed config.")
		flagDumpPackages = flagset.Bool("dump_packages", false, "Debug option: if true then dump the list of packages and dependencies pkgtrim detected.")
		flagTrimfile     = flagset.String("trimfile", defaultTrimfile, "The config file.")
	)
	flagset.SetOutput(w)
	if err := flagset.Parse(args); err != nil {
		return err
	}

	system, err := NewPackageSystem(rootfs)
	if err != nil {
		return fmt.Errorf("detect package system: %v", err)
	}
	pkgs, err := system.Packages()
	if err != nil {
		return fmt.Errorf("load packages: %v", err)
	}

	if *flagDumpPackages {
		for _, pkg := range pkgs {
			fmt.Fprintf(w, "%s %d %s\n", pkg.Name, pkg.Size, strings.Join(pkg.Deps, " "))
		}
		return nil
	}

	// Parse ~/.pkgtrim.
	// trimconfig maps each entry in the config to its corresponding group comment.
	trimconfig := map[string]string{}
	trimfileBytes, err := fs.ReadFile(rootfs, abspath(*flagTrimfile))
	if err != nil {
		if *flagTrimfile != defaultTrimfile {
			return fmt.Errorf("open trimfile: %v", err)
		}
	}
	if err := parseconfig(trimconfig, 0, trimfileBytes); err != nil {
		return fmt.Errorf("parse %s: %v", *flagTrimfile, err)
	}
	if *flagDumpConfig {
		// Group by comments.
		bycomments := map[string][]string{}
		for pkg, comment := range trimconfig {
			bycomments[comment] = append(bycomments[comment], pkg)
		}
		for _, comment := range slices.Sorted(maps.Keys(bycomments)) {
			slices.Sort(bycomments[comment])
			if comment != "" {
				fmt.Fprintf(w, "# %s\n", comment)
			}
			fmt.Fprintf(w, "%s\n", strings.Join(bycomments[comment], " "))
		}
		return nil
	}
	intentional := func(pkg string) bool {
		if _, exists := trimconfig[pkg]; exists {
			return true
		}
		return false
	}

	// To keep things efficient, keep things in []int32 arrays.
	type pkgid int32
	var (
		n       = len(pkgs)                 // number of packages
		q       = make([]pkgid, 0, n)       // queue for the breadth first search
		visited = make([]bool, n)           // marker for the bfs
		shared  = make([]bool, n)           // marker for determining the unique size
		deps    = make([][]pkgid, n)        // direct dependencies of a package
		rdeps   = make([][]pkgid, n)        // direct reverse dependencies of a package
		pkgids  = make(map[string]pkgid, n) // map package names to a number
		unique  = make([]int64, n)          // the total unique size used for each package
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

	// Runs breadth first search.
	// q must be initialized with the initial entries.
	// Returns the unique size.
	bfs := func() int64 {
		initialQueueSize := len(q)
		for _, i := range q {
			visited[i] = true
		}
		for qi := 0; qi < len(q); qi++ {
			for _, j := range deps[q[qi]] {
				if !visited[j] {
					visited[j], q = true, append(q, j)
				}
			}
		}

		// Compute the unique size.
		// A package is not unique in the ith package if it has an rdep that is already shared or is outside the visited packages.
		var uniqueSize int64
		for _, j := range q[:initialQueueSize] {
			uniqueSize += pkgs[j].Size
		}
		for _, j := range q[initialQueueSize:] {
			for _, k := range rdeps[j] {
				if shared[k] || !visited[k] {
					shared[j] = true
					break
				}
			}
			if !shared[j] {
				uniqueSize += pkgs[j].Size
			}
		}
		return uniqueSize
	}

	if flagset.NArg() > 0 {
		for _, pkg := range flagset.Args() {
			id, exists := pkgids[pkg]
			if !exists {
				return fmt.Errorf("package %s not installed", pkg)
			}
			q = append(q, id)
		}
		bfs()
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
		deps, rdeps, q = rdeps, deps, q[:flagset.NArg()]
		bfs()
		deps, rdeps = rdeps, deps
		for i, pkg := range pkgs {
			if !visited[i] || len(rdeps[i]) > 0 {
				continue
			}
			if intentional(pkg.Name) {
				intentionalpkgs = append(intentionalpkgs, pkg.Name)
			} else {
				unintentionalpkgs = append(unintentionalpkgs, pkg.Name)
			}
		}

		fmt.Fprintf(w, "shared dependencies (%s): %s\n\n", humanize(sharedsize), strings.Join(sharedpkgs, " "))
		fmt.Fprintf(w, "unique dependencies (%s): %s\n\n", humanize(uniquesize), strings.Join(uniquepkgs, " "))
		fmt.Fprintf(w, "intentional top level rdeps: %s\n\n", strings.Join(intentionalpkgs, " "))
		fmt.Fprintf(w, "unintentional top level rdeps: %s\n\n", strings.Join(unintentionalpkgs, " "))
		return nil
	}

	// No args mode.
	// For each top level undocumented package compute the total and unique usage via a breadth first search.
	for i := range n {
		if len(rdeps[i]) > 0 || intentional(pkgs[i].Name) {
			continue
		}
		q = append(q[:0], pkgid(i))
		unique[i] = bfs()

		// Reset the arrays for the next iteration.
		for _, j := range q {
			visited[j], shared[j] = false, false
		}
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
		if len(rdeps[id]) > 0 || intentional(pkg.Name) {
			continue
		}
		fmt.Fprintf(w, "%s %-24s %s\n", humanize(unique[id]), pkg.Name, pkg.Desc)
	}
	return nil
}
