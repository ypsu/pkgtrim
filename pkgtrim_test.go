// Note that most tests are done in dump.go.
// This contains effect tests for simpler functionality.

package main

import (
	"os"
	"testing"

	"github.com/ypsu/efftesting"
)

func TestAbspath(t *testing.T) {
	et := efftesting.New(t)
	wd = "/home/user/"
	et.Expect("empty", abspath(""), "home/user")
	et.Expect("root", abspath("/"), "")
	et.Expect("abs under wd", abspath("/home/user/.pkgtrim"), "home/user/.pkgtrim")
	et.Expect("abs not under wd", abspath("/var/lib/package"), "var/lib/package")
	et.Expect("relative under wd", abspath("settings/pkgtrim"), "home/user/settings/pkgtrim")
	et.Expect("relative not under wd", abspath("../../var/lib/package"), "var/lib/package")
}

func TestHumanize(t *testing.T) {
	et := efftesting.New(t)
	et.Expect("", humanize(0), "    0.0 MB")
	et.Expect("", humanize(-1), "   -0.0 MB")
	et.Expect("", humanize(-123456), "   -0.1 MB")
	et.Expect("", humanize(1), "    0.0 MB")
	et.Expect("", humanize(12), "    0.0 MB")
	et.Expect("", humanize(123), "    0.0 MB")
	et.Expect("", humanize(1234), "    0.0 MB")
	et.Expect("", humanize(12345), "    0.0 MB")
	et.Expect("", humanize(123456), "    0.1 MB")
	et.Expect("", humanize(1234567), "    1.2 MB")
	et.Expect("", humanize(12345678), "   12.3 MB")
	et.Expect("", humanize(123456789), "  123.5 MB")
	et.Expect("", humanize(1234567890), " 1234.6 MB")
	et.Expect("", humanize(12345678901), "12345.7 MB")
	et.Expect("", humanize(123456789012), "123456.8 MB")
	et.Expect("", humanize(1234567890123), "1234567.9 MB")
	et.Expect("", humanize(12345678901234), "12345678.9 MB")
}

func TestGlobs(t *testing.T) {
	et := efftesting.New(t)
	et.Expect("", makeRE(), "^()$")
	et.Expect("", makeRE("a"), "^(a)$")
	et.Expect("", makeRE("a*b"), "^(a.*b)$")
	et.Expect("", makeRE("a", "b", "c"), "^(a|b|c)$")
	et.Expect("", makeRE("a", "b*", "c"), "^(a|b.*|c)$")
}

func TestMain(m *testing.M) {
	os.Exit(efftesting.Main(m))
}
