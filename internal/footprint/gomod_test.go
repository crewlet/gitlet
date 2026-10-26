package footprint

import "testing"

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
