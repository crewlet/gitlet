package footprint

import (
	"slices"
	"testing"
)

func TestGoModulePath(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "module example.com/m\n\ngo 1.26\n", "example.com/m"},
		{"comments first", "// Code owners: core\n\n// More.\nmodule example.com/m\n", "example.com/m"},
		{"trailing comment", "module example.com/m // deprecated: use v2\n", "example.com/m"},
		{"quoted", "module \"example.com/m\"\n", "example.com/m"},
		{"quoted with comment", "module \"example.com/m\" // c\n", "example.com/m"},
		{"raw string", "module `example.com/m`\n", "example.com/m"},
		{"tab", "module\texample.com/m\n", "example.com/m"},
		{"block", "module (\n\t// the path\n\texample.com/m\n)\n", "example.com/m"},
		{"byte order mark", byteOrderMark + "module example.com/m\n", "example.com/m"},
		{"crlf", "module example.com/m\r\ngo 1.26\r\n", "example.com/m"},
		{"indented", "  module example.com/m\n", "example.com/m"},
		{"first directive wins", "module a.com/x\nmodule b.com/y\n", "a.com/x"},
		{"after other directives", "go 1.26\nrequire x.com/y v1.0.0\nmodule example.com/m\n", "example.com/m"},
		{"not a directive", "modulex example.com/m\n", ""},
		{"commented out", "// module example.com/m\n", ""},
		{"unterminated quote", "module \"example.com/m\n", ""},
		{"bad escape", "module \"example.com/\\q\"\n", ""},
		{"no path", "module\n", ""},
		{"none", "go 1.26\n", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := goModulePath([]byte(tt.in)); got != tt.want {
				t.Errorf("goModulePath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseGoModDirectories(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		uses     []string
		replaces []goReplace
	}{
		{"use", "go 1.26\n\nuse ./svc\nuse \"./lib\" // c\n", []string{"./svc", "./lib"}, nil},
		{"use block", "use (\n\t./svc\n\t// gone\n\t.\n)\n", []string{"./svc", "."}, nil},
		{"replace with versions", "replace example.com/a v1.0.0 => ../a\n", nil, []goReplace{{"example.com/a", "../a"}}},
		{"replace without versions", "replace example.com/a => ./a\n", nil, []goReplace{{"example.com/a", "./a"}}},
		{"replace block", "replace (\n\texample.com/a => ../a\n\t\"example.com/b\" v2.0.0 => `./b`\n)\n", nil,
			[]goReplace{{"example.com/a", "../a"}, {"example.com/b", "./b"}}},
		{"module target ignored", "replace example.com/a => example.com/fork v1.2.0\n", nil, nil},
		{"absolute target ignored", "replace example.com/a => /src/a\n", nil, nil},
		{"no arrow", "replace example.com/a ../a\n", nil, nil},
		{"no target", "replace example.com/a =>\n", nil, nil},
		{"bad quote", "replace \"example.com/a => ../a\n", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := parseGoMod([]byte(tt.in))
			if !slices.Equal(f.uses, tt.uses) || !slices.Equal(f.replaces, tt.replaces) {
				t.Errorf("parseGoMod(%q) = uses %q replaces %q, want %q %q", tt.in, f.uses, f.replaces, tt.uses, tt.replaces)
			}
		})
	}
}

func TestRepoDir(t *testing.T) {
	tests := []struct {
		base, dir, want string
		ok              bool
	}{
		{".", "./svc", "svc", true},
		{"svc", "../shared", "shared", true},
		{"svc", ".", "svc", true},
		{"svc/x", "../../lib", "lib", true},
		{"svc", "../../shared", "", false},
		{".", "..", "", false},
		{"svc", "/src/a", "", false},
	}
	for _, tt := range tests {
		if got, ok := repoDir(tt.base, tt.dir); got != tt.want || ok != tt.ok {
			t.Errorf("repoDir(%q, %q) = %q, %v, want %q, %v", tt.base, tt.dir, got, ok, tt.want, tt.ok)
		}
	}
}
