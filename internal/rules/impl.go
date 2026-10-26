package rules

// TODO(implement): docs/SPEC.md, sections 9.6, 10 and 11.

import "github.com/crewlet/gitlet/internal/model"

func evaluate(v View) []model.Note                                             { return nil }
func threads(v View) []model.ThreadState                                       { return nil }
func decisions(v View) []Decision                                              { return nil }
func autoClaims(v View) []AutoClaim                                            { return nil }
func releases(v View) []Release                                                { return nil }
func similarity(a, b string, corpus []string) (float64, int)                   { return 0, 0 }
func matchRef(ref model.Ref, sym model.Symbol, uniqueMethod bool) (bool, bool) { return false, false }
func hotFile(path string) string                                               { return "" }
func migrationSeq(path string, extraGlobs []string) (string, string, bool)     { return "", "", false }
