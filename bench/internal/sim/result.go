package sim

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Result is one run's outcome.
type Result struct {
	Name       string         `json:"name"`
	Mode       Mode           `json:"mode"`
	Agents     int            `json:"agents"`
	Seed       uint64         `json:"seed"`
	TasksFile  string         `json:"tasksFile"`
	Compliance float64        `json:"compliance"`
	DupJudge   string         `json:"dupJudge,omitempty"`
	Overlap    string         `json:"overlap,omitempty"`
	Rounds     int            `json:"rounds"`
	WallSecs   float64        `json:"wallSecs"`
	CIRuns     int            `json:"ciRuns"`
	CISecs     float64        `json:"ciSecs"`
	MainVetOK  bool           `json:"mainVetOK"`
	MainVet    []string       `json:"mainVet,omitempty"`
	Tasks      []*TaskRun     `json:"tasks"`
	Gitlet     *GitletSummary `json:"gitlet,omitempty"`
	Outcomes   map[string]int `json:"outcomes"`
	Totals     map[string]int `json:"totals"`
	RemoteRefs map[string]int `json:"remoteRefs,omitempty"`
}

// GitletSummary is the overhead of gitlet in a run.
type GitletSummary struct {
	Calls     map[string]int     `json:"calls"`
	Refused   map[string]int     `json:"refused"`
	Errors    map[string]int     `json:"errors"`
	P50       map[string]float64 `json:"p50"`
	P95       map[string]float64 `json:"p95"`
	TotalSecs float64            `json:"totalSecs"`
	Notes     int                `json:"notes"`
	NoteChars int                `json:"noteChars"`
}

func (s *Sim) result(ctx context.Context) *Result {
	r := &Result{Name: s.cfg.Name, Mode: s.cfg.Mode, Agents: s.cfg.Agents, Seed: s.cfg.Seed, TasksFile: s.cfg.TasksFile, Compliance: s.cfg.Compliance,
		DupJudge: s.cfg.DupJudge, Overlap: s.cfg.Overlap,
		Rounds: s.round + 1, WallSecs: time.Since(s.start).Seconds(), CIRuns: s.ciRuns, CISecs: s.ciTime.Seconds(), Outcomes: map[string]int{}, Totals: map[string]int{}}
	for _, t := range s.tasks {
		tr := s.runs[t.ID]
		if tr == nil {
			tr = &TaskRun{Task: t, Outcome: "not_started"}
		}
		r.Tasks = append(r.Tasks, tr)
		r.Outcomes[tr.Outcome]++
		for k, v := range map[string]int{
			"workLines": tr.WorkLines, "conflicts": tr.Conflicts, "conflictFiles": tr.ConflictFiles, "conflictHunks": tr.ConflictHunks,
			"conflictLines": tr.ConflictLines, "buildBreaks": tr.BuildBreaks, "fixLines": tr.FixLines, "mergeConflicts": tr.MergeConflicts,
			"ciFailures": tr.CIFailures, "earlyFixLines": tr.EarlyFixLines, "deferredLines": tr.DeferredLines, "waitRounds": tr.WaitRounds,
			"busyRounds": tr.BusyRounds, "blockedRounds": tr.BlockedRounds, "coordCalls": tr.CoordCalls, "defers": tr.Defers, "submits": tr.Submits,
		} {
			r.Totals[k] += v
		}
	}
	// The final main must still pass the full check.
	if _, err := s.env.Git(ctx, s.ciDir, "read-tree", "-u", "--reset", s.main); err == nil {
		ok, errs, err := CI(ctx, s.env, s.ciDir)
		r.MainVetOK = err == nil && ok
		r.MainVet = errs[:min(5, len(errs))]
	}
	if s.cfg.Mode == ModeGitlet {
		g := &GitletSummary{Calls: s.stats.Calls, Refused: s.stats.Refused, Errors: s.stats.Errors, P50: map[string]float64{}, P95: map[string]float64{}, Notes: s.stats.Notes, NoteChars: s.stats.NoteChars}
		for verb, ds := range s.stats.Durations {
			sort.Float64s(ds)
			g.P50[verb] = ds[len(ds)/2]
			g.P95[verb] = ds[min(len(ds)-1, len(ds)*95/100)]
			for _, d := range ds {
				g.TotalSecs += d
			}
		}
		r.Gitlet = g
		if out, err := s.env.Git(ctx, s.remote, "for-each-ref", "--format=%(refname)"); err == nil {
			r.RemoteRefs = map[string]int{}
			for _, ref := range strings.Split(out, "\n") {
				parts := strings.SplitN(ref, "/", 4)
				if len(parts) >= 3 {
					r.RemoteRefs[parts[1]+"/"+parts[2]]++
				}
			}
		}
	}
	return r
}

// Summary is a one-paragraph report of a run.
func (r *Result) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d rounds, %.0fs wall, outcomes %v, main vet ok=%v\n", r.Name, r.Rounds, r.WallSecs, r.Outcomes, r.MainVetOK)
	keys := make([]string, 0, len(r.Totals))
	for k := range r.Totals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s=%d", k, r.Totals[k])
	}
	if r.Gitlet != nil {
		fmt.Fprintf(&b, "\n  gitlet: %.1fs in %v calls, notes=%d (%d chars), refused=%v errors=%v", r.Gitlet.TotalSecs, r.Gitlet.Calls, r.Gitlet.Notes, r.Gitlet.NoteChars, r.Gitlet.Refused, r.Gitlet.Errors)
	}
	return b.String()
}
