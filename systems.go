package main

import (
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

// Package describes a single installed package.
type Package struct {
	Name string   // name of the package
	Desc string   // human description of the package
	Size int64    // size of the package in bytes
	Deps []string // list of other packages this package depends on; resolved packages only, no virtual packages here
}

// PackageSystem is the interface that various package managers must implement.
type PackageSystem interface {
	// Packages returns all the installed packages in the system.
	Packages() ([]Package, error)

	// Remove generates a command that removes the specified packages.
	Remove(pkgs []string) []string

	// Remove generates a command that installs the specified packages.
	Install(pkgs []string) []string
}

// NewPackageSystem creates a new PackageSystem based on the files found in the passed in filesystem.
func NewPackageSystem(rootfs fs.FS) (PackageSystem, error) {
	if _, err := fs.Stat(rootfs, "var/lib/pacman/local"); err == nil {
		return archlinux{rootfs}, nil
	}
	if _, err := fs.Stat(rootfs, "var/lib/dpkg/status"); err == nil {
		return debian{rootfs}, nil
	}
	return nil, fmt.Errorf("no supported system detected")
}

type archlinux struct {
	rootfs fs.FS
}

type debian struct {
	rootfs fs.FS
}

func (s archlinux) Remove(pkgs []string) []string {
	return append([]string{"sudo", "pacman", "-R"}, pkgs...)
}

func (s archlinux) Install(pkgs []string) []string {
	return append([]string{"sudo", "pacman", "-S"}, pkgs...)
}

func (s debian) Remove(pkgs []string) []string {
	return append([]string{"sudo", "apt", "remove"}, pkgs...)
}

func (s debian) Install(pkgs []string) []string {
	return append([]string{"sudo", "apt", "install"}, pkgs...)
}

func (s archlinux) Packages() ([]Package, error) {
	pkgfiles, err := fs.Glob(s.rootfs, "var/lib/pacman/local/*/desc")
	if err != nil {
		return nil, fmt.Errorf("glob /var/lib/pacman/local/*/desc: %v", err)
	}
	if len(pkgfiles) == 0 {
		return nil, fmt.Errorf("glob /var/lib/pacman/local/*/desc: no results")
	}

	var (
		pkgs     = make([]Package, 0, 1e4)      // the return value
		depends  = make([]string, 0, 1e4)       // the depends section for each package
		provider = make(map[string]string, 1e4) // for tracking virtual packages
	)

	for _, file := range pkgfiles {
		desc, err := fs.ReadFile(s.rootfs, file)
		if err != nil {
			return nil, err
		}

		var pkg Package
		for _, entry := range strings.Split("\n"+string(desc), "\n%") {
			if entry == "" {
				continue
			}
			hdrname, value, ok := strings.Cut(entry, "%\n")
			if !ok {
				return nil, fmt.Errorf("parse %s: cut %q", file, entry)
			}
			value = strings.TrimSpace(value)
			if hdrname != "NAME" && pkg.Name == "" {
				return nil, fmt.Errorf("parse %s: %q is the first section, want NAME", file, hdrname)
			}
			switch hdrname {
			case "NAME":
				pkg.Name = value
				provider[pkg.Name] = pkg.Name
			case "DESC":
				pkg.Desc, _, _ = strings.Cut(value, "\n")
			case "SIZE":
				pkg.Size, _ = strconv.ParseInt(value, 10, 64)
			case "DEPENDS":
				if len(pkgs) != len(depends) {
					return nil, fmt.Errorf("parse %s: double DEPENDS section", file)
				}
				depends = append(depends, value)
			case "PROVIDES":
				for _, line := range strings.Split(value, "\n") {
					if line == "" {
						continue
					}
					// Remove the version bit from instances like "libargon2.so=1-64".
					line, _, _ = strings.Cut(line, "=")
					provider[line] = pkg.Name
				}
			}
		}
		if len(depends) == len(pkgs) {
			// Add an empty depends sections for packages that didn't have one.
			depends = append(depends, "")
		}
		if pkg.Name == "" {
			return nil, fmt.Errorf("parse %s: no name found", file)
		}
		pkgs = append(pkgs, pkg)
	}

	// Now resolve the dependencies using the provider map.
	deps := make([]string, 0, 32)
	for i := range pkgs {
		deps := deps
		for _, d := range strings.Split(depends[i], "\n") {
			if d == "" {
				continue
			}
			// Remove the version bit from instances like "ca-certificates-utils>=20181109-3" and "libargon2.so=1-64".
			d, _, _ = strings.Cut(d, ">")
			d, _, _ = strings.Cut(d, "=")
			p, ok := provider[d]
			if !ok {
				return nil, fmt.Errorf("resolve %s: no provider found for dependency %s", pkgs[i].Name, d)
			}
			deps = append(deps, p)
		}
		slices.Sort(deps)
		pkgs[i].Deps = slices.Clone(slices.Compact(deps))
	}
	return pkgs, nil
}

func (s debian) Packages() ([]Package, error) {
	statusfile, err := fs.ReadFile(s.rootfs, "var/lib/dpkg/status")
	if err != nil {
		return nil, err
	}

	var (
		curpkg     Package // the current package that is being parsed
		curdepends string  // the current package's dependency section

		pkgs     = make([]Package, 0, 1e4)      // the return value
		depends  = make([]string, 0, 1e4)       // the depends section for each package
		provider = make(map[string]string, 1e4) // for tracking virtual packages
	)

	for _, line := range strings.Split(string(statusfile), "\n") {
		if line == "" {
			if curpkg.Name != "" {
				pkgs, depends = append(pkgs, curpkg), append(depends, curdepends)
				provider[curpkg.Name] = curpkg.Name
			}
			curpkg, curdepends = Package{}, ""
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimSpace(value)
		switch key {
		case "Package":
			curpkg.Name = value
		case "Description":
			curpkg.Desc = value
		case "Installed-Size":
			curpkg.Size, _ = strconv.ParseInt(value, 10, 64)
			curpkg.Size *= 1024
		case "Status":
			if !strings.HasPrefix(value, "install") {
				curpkg.Name = ""
			}
		case "Provides":
			if curpkg.Name == "" {
				break
			}
			for _, p := range strings.Split(value, ",") {
				// Remove the version bit.
				p, _, _ = strings.Cut(p, "(")
				provider[strings.TrimSpace(p)] = curpkg.Name
			}
		case "Depends":
			curdepends = value
		}
	}
	if curpkg.Name != "" {
		pkgs, depends = append(pkgs, curpkg), append(depends, curdepends)
		provider[curpkg.Name] = curpkg.Name
	}

	// Now resolve the dependencies using the provider map.
	deps := make([]string, 0, 32)
	for i := range pkgs {
		deps := deps
		for _, depalternatives := range strings.Split(depends[i], ",") {
			if strings.TrimSpace(depalternatives) == "" {
				continue
			}
			var depprovider string
			for _, d := range strings.Split(depalternatives, "|") {
				d = strings.TrimSpace(d)
				if d == "" {
					continue
				}
				// Cut the version stuff.
				d, _, _ = strings.Cut(d, "(")
				d = strings.TrimSuffix(strings.TrimSpace(d), ":any")
				if p, ok := provider[d]; ok {
					depprovider = p
					break
				}
			}
			if depprovider == "" {
				return nil, fmt.Errorf("resolve %s: no provider found for dependency %s", pkgs[i].Name, depalternatives)
			}
			deps = append(deps, depprovider)
		}
		slices.Sort(deps)
		pkgs[i].Deps = slices.Clone(slices.Compact(deps))
	}
	return pkgs, nil
}
