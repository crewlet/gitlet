// Package e2e holds gitlet's end-to-end tests (docs/SPEC.md, section 17):
// two clones of one bare remote act as two machines, each with its own MCP
// server, and agents drive them the way a real client does, by calling
// tools by name and reading the results. The package has no code of its
// own; everything is in the _test.go files.
package e2e
