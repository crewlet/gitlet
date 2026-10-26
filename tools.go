//go:build tools

// Package gitlet pins dependencies used by packages that are not written yet,
// so go.mod and go.sum stay stable while they are implemented in parallel.
package gitlet

import (
	_ "github.com/modelcontextprotocol/go-sdk/mcp"
	_ "github.com/spf13/cobra"
	_ "github.com/tree-sitter/go-tree-sitter"
	_ "github.com/tree-sitter/tree-sitter-go/bindings/go"
	_ "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	_ "github.com/tree-sitter/tree-sitter-python/bindings/go"
	_ "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	_ "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
	_ "modernc.org/sqlite"
)
