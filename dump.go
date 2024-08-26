//go:build test

// Run this effdump like this:
//
//	go run -tags=test github.com/ypsu/pkgtrim -force

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	_ "embed"

	"github.com/ypsu/effdump"
	"github.com/ypsu/textar"
)

func dump(ctx context.Context) error {
	d := effdump.New("pkgtrim")
	d.RegisterFlags(flag.CommandLine)
	flagFS := flag.String("fs", "", "Override testdata to this textar filesystem.")
	flag.Parse()

	var rootfs fs.FS
	var testfile string
	add := func(name string, args ...string) {
		w := &bytes.Buffer{}
		err := Pkgtrim(w, rootfs, args)
		result := make([]textar.File, 2)
		result[0] = textar.File{"pkgtrim " + strings.Join(args, " "), w.Bytes()}
		if err == nil {
			result[1].Name = "result: success"
		} else {
			result[1].Name = "result: fail"
			result[1].Data = []byte(err.Error() + "\n")
		}
		d.Add(testfile+"/"+name, textar.Format(result))
	}

	wd = "/home/user"
	var testfiles []string
	if *flagFS == "" {
		testfiles, _ = filepath.Glob("testdata/*.textar")
	} else {
		testfiles = strings.Split(*flagFS, ",")
	}
	for _, filename := range testfiles {
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		testfile = strings.TrimSuffix(filepath.Base(filename), ".textar")
		rootfs = textar.FS(textar.Parse(data))
		add("help", "-help")
		add("badflag", "-blah")
		add("badflagorder", "glibc", "-dump_packages")
		add("noargs")
		add("packages", "-dump_packages")

		if testfile == "archsmall" {
			add("cfgbadarg", "-f=nonexistent_pkgtrim", "-dump_config")
			add("cfgparse", "-f=tricky_pkgtrim", "-dump_config")
			add("trim1", "fancyapp")
			add("trim2", "glibc")
			add("trim3", "fancyapp", "glibc")
			add("trim4", "fancyapp", "glibc", "nonexistent")
			add("multiaction1", "-install", "-remove")
			add("multiaction2", "-install", "-trace")
			add("multiaction3", "-remove", "-trace")
			add("install", "-install", "-dryrun", "-f=tricky_pkgtrim")
			add("removeall", "-remove", "-dryrun")
			add("remove", "-remove", "-dryrun", "fancylib")
		}
		if testfile == "archlarge" {
			add("removeall", "-remove", "-dryrun")
			add("remove", "-remove", "-dryrun", "-f=pkgtrim.config")
			add("remove1", "-remove", "-dryrun", "-f=pkgtrim.config", "clang")
			add("trimmed", "-f=pkgtrim.config")
			add("trimmed2", "-f=pkgtrim.config", "gmp")
			add("trim1", "clang")
			add("tracebad0", "-trace")
			add("tracebad1", "-trace", "gmp")
			add("tracebad2", "-trace", "gmp", "gdb")
			add("tracebad3", "-trace", "gdb", "gmp", "gmp")
			add("tracebadpkg", "-trace", "gdb", "gxx")
			add("traceok", "-trace", "gdb", "gmp")
		}
	}

	d.Run(ctx)
	return nil
}

func main() {
	if err := dump(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v.", err)
		os.Exit(1)
	}
}
