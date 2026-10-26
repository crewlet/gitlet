package mcpserver

// TODO(implement)

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type impl struct{}

func newServer(opt Options) *Server { return &Server{} }
func (s *Server) mcp() *mcp.Server  { return nil }
func (s *Server) run(ctx context.Context, t mcp.Transport) error {
	return errors.New("mcpserver: not implemented")
}
func (s *Server) close() error { return nil }
