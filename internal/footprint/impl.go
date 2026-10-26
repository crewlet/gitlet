package footprint

// TODO(implement): docs/SPEC.md, sections 9.2 to 9.6.

import (
	"context"
	"errors"

	"github.com/crewlet/gitlet/internal/model"
	"github.com/crewlet/gitlet/internal/symbols"
)

func compute(ctx context.Context, src Source, base, head string, opt Options) (Result, error) {
	return Result{}, errors.New("footprint: not implemented")
}
func diffSymbols(old, new *symbols.File) []model.SymbolChange                { return nil }
func diffMoved(old, new *symbols.File) []model.SymbolChange                  { return nil }
func classify(old, new model.Symbol) bool                                    { return false }
func resolve(f *symbols.File, refs []model.Ref, goModule string) []model.Ref { return refs }
