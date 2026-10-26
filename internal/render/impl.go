package render

// TODO(implement): docs/SPEC.md, section 4.2.

import (
	"github.com/crewlet/gitlet/internal/api"
	"github.com/crewlet/gitlet/internal/model"
)

func text(r *api.Result, mode Mode) string                        { return "" }
func note(n model.Note, mode Mode) string                         { return "" }
func notes_(heading string, notes []model.Note, mode Mode) string { return "" }
func option(o model.Option, mode Mode) string                     { return "" }
func quote(q model.Quote) string                                  { return "" }
