package sim

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/crewlet/gitlet/bench/internal/gitenv"
	"github.com/crewlet/gitlet/bench/internal/goedit"
)

// PairTruth is how two tasks of a backlog interact when both are made
// against the base: the ground truth gitlet's notes are judged against.
type PairTruth struct {
	A, B          string
	Conflict      bool // their changes conflict textually
	ConflictLines int
	Break         bool // they merge cleanly and the result does not build
	Dup           bool // one duplicates the other
}

// Interacts reports whether the pair costs anything when done at once.
func (p PairTruth) Interacts() bool { return p.Conflict || p.Break || p.Dup }

// PairTruths applies each task alone to the base and merges every pair. A
// pair that merges cleanly is built only when it can break the build: when
// one task changes the signature of a function the other's new code calls,
// or both change signatures. Other clean pairs only add code that the base
// already builds with, and are taken to build.
func PairTruths(ctx context.Context, env *gitenv.Env, fx *Fixture, work string, tasks []*Task) ([]PairTruth, error) {
	dir := filepath.Join(work, "oracle")
	_ = os.RemoveAll(dir)
	_, _ = env.Git(ctx, fx.Dir, "worktree", "prune")
	if _, err := env.Git(ctx, fx.Dir, "worktree", "add", "-f", "--detach", dir, fx.Base); err != nil {
		return nil, err
	}
	defer func() {
		_, _ = env.Git(ctx, fx.Dir, "worktree", "remove", "-f", dir)
	}()
	heads := make([]string, len(tasks))
	sig := make([]map[string]bool, len(tasks))
	for i, t := range tasks {
		if _, err := env.Git(ctx, dir, "reset", "-q", "--hard", fx.Base); err != nil {
			return nil, err
		}
		if _, err := env.Git(ctx, dir, "clean", "-qfdx"); err != nil {
			return nil, err
		}
		if _, err := goedit.ApplyAll(dir, t.Ops, nil); err != nil {
			return nil, fmt.Errorf("%s: %w", t.ID, err)
		}
		if _, err := env.Git(ctx, dir, "add", "-A"); err != nil {
			return nil, err
		}
		if _, err := env.Git(ctx, dir, "commit", "-q", "--no-verify", "--allow-empty", "-m", t.ID); err != nil {
			return nil, err
		}
		h, err := env.Git(ctx, dir, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		heads[i] = h
		sig[i] = map[string]bool{}
		for _, op := range t.Ops {
			if op.Kind == goedit.AddParam || op.Kind == goedit.RenameFunc {
				sig[i][op.Target] = true
			}
		}
	}
	var out []PairTruth
	for i := range tasks {
		for j := i + 1; j < len(tasks); j++ {
			a, b := tasks[i], tasks[j]
			p := PairTruth{A: a.ID, B: b.ID, Dup: a.DupOf == b.ID || b.DupOf == a.ID || a.DupOf != "" && a.DupOf == b.DupOf}
			tree, cs, err := mergeTree(ctx, env, dir, heads[i], heads[j])
			if err != nil {
				return nil, err
			}
			if cs != nil {
				p.Conflict, p.ConflictLines = true, cs.Lines
			} else if mayBreak(sig[i], b) || mayBreak(sig[j], a) || len(sig[i]) > 0 && len(sig[j]) > 0 {
				if _, err := env.Git(ctx, dir, "read-tree", "-u", "--reset", tree); err != nil {
					return nil, err
				}
				pass, _, err := Build(ctx, env, dir)
				if err != nil {
					return nil, err
				}
				p.Break = !pass
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// mayBreak reports whether t's new code calls a function in sig.
func mayBreak(sig map[string]bool, t *Task) bool {
	for _, c := range t.Calls {
		if sig[c] {
			return true
		}
	}
	return false
}

// Detection is how well a run's notes flagged the pairs that interact.
type Detection struct {
	Concurrent     int `json:"concurrentPairs"`     // pairs in flight at the same time
	Interacting    int `json:"interactingPairs"`    // of those, pairs that conflict, break or duplicate
	ConflictPairs  int `json:"conflictPairs"`       // ...that conflict
	BreakPairs     int `json:"breakPairs"`          // ...that break the build
	DupPairs       int `json:"dupPairs"`            // ...that duplicate
	Flagged        int `json:"flaggedPairs"`        // concurrent pairs any note named
	FlaggedWarn    int `json:"flaggedWarnPairs"`    // ...with a warn or act note
	FlaggedAct     int `json:"flaggedActPairs"`     // ...with an act note
	Hit            int `json:"hitPairs"`            // interacting pairs any note named
	HitWarn        int `json:"hitWarnPairs"`        // ...with a warn or act note
	HitAct         int `json:"hitActPairs"`         // ...with an act note
	HitConflict    int `json:"hitConflictPairs"`    // conflicting pairs named by a warn or act note
	HitBreak       int `json:"hitBreakPairs"`       // breaking pairs named by a warn or act note
	HitDup         int `json:"hitDupPairs"`         // duplicate pairs named by a duplicate note
	LeadRoundsSum  int `json:"leadRoundsSum"`       // rounds from the first warn or act note to the first landing, summed over warned hits
	LeadRoundsHits int `json:"leadRoundsHits"`      // hits the sum covers
	NotesOnQuiet   int `json:"notesOnQuietPairs"`   // notes naming a pair that does not interact
	NotesOnTrouble int `json:"notesOnTroublePairs"` // notes naming a pair that does
}

type pairKey [2]string

func keyOf(a, b string) pairKey {
	if a > b {
		a, b = b, a
	}
	return pairKey{a, b}
}

var sevRank = map[string]int{"info": 1, "warn": 2, "act": 3}

// Detect judges a run's notes against the ground truth.
func Detect(runDir string, truths []PairTruth) (*Detection, error) {
	var res Result
	if err := ReadJSON(filepath.Join(runDir, "result.json"), &res); err != nil {
		return nil, err
	}
	type span struct{ from, to int }
	spans := map[string]span{}
	for _, t := range res.Tasks {
		if t.Outcome == "not_started" {
			continue
		}
		end := res.Rounds
		switch {
		case t.Landed > 0:
			end = t.Landed
		case t.Ended > 0:
			end = t.Ended
		}
		spans[t.ID] = span{t.Started, end}
	}
	landed := map[string]int{}
	for _, t := range res.Tasks {
		if t.Landed > 0 {
			landed[t.ID] = t.Landed
		}
	}
	type flag struct {
		sev, first, firstWarn, notes int
		dup                          bool // a duplicate note named the pair
	}
	flags := map[pairKey]*flag{}
	f, err := os.Open(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev struct {
			Kind     string   `json:"kind"`
			Round    int      `json:"round"`
			Task     string   `json:"task"`
			Severity string   `json:"severity"`
			NoteKind string   `json:"noteKind"`
			Parties  []string `json:"parties"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Kind != "note" || ev.Task == "" {
			continue
		}
		for _, p := range ev.Parties {
			if p == ev.Task || p == "" || p[0] == '?' {
				continue
			}
			k := keyOf(ev.Task, p)
			fl := flags[k]
			if fl == nil {
				fl = &flag{first: ev.Round, firstWarn: -1}
				flags[k] = fl
			}
			fl.notes++
			fl.sev = max(fl.sev, sevRank[ev.Severity])
			fl.dup = fl.dup || ev.NoteKind == "duplicate"
			if sevRank[ev.Severity] >= 2 && fl.firstWarn < 0 {
				fl.firstWarn = ev.Round
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	d := &Detection{}
	for _, p := range truths {
		a, okA := spans[p.A]
		b, okB := spans[p.B]
		// A task that starts in the round another lands started from the
		// main before it, so the two were in flight together.
		if !okA || !okB || a.from > b.to || b.from > a.to {
			continue
		}
		d.Concurrent++
		fl := flags[keyOf(p.A, p.B)]
		in := p.Interacts()
		if in {
			d.Interacting++
		}
		if p.Conflict {
			d.ConflictPairs++
		}
		if p.Break {
			d.BreakPairs++
		}
		if p.Dup {
			d.DupPairs++
		}
		if fl == nil {
			continue
		}
		d.Flagged++
		if in {
			d.Hit++
			d.NotesOnTrouble += fl.notes
		} else {
			d.NotesOnQuiet += fl.notes
		}
		if fl.sev >= 2 {
			d.FlaggedWarn++
			if in {
				d.HitWarn++
				if p.Conflict {
					d.HitConflict++
				}
				if p.Break {
					d.HitBreak++
				}
				first := 0
				for _, id := range []string{p.A, p.B} {
					if l, ok := landed[id]; ok && (first == 0 || l < first) {
						first = l
					}
				}
				if first > 0 {
					d.LeadRoundsSum += first - fl.firstWarn
					d.LeadRoundsHits++
				}
			}
		}
		if fl.sev >= 3 {
			d.FlaggedAct++
			if in {
				d.HitAct++
			}
		}
		if p.Dup && fl.dup {
			d.HitDup++
		}
	}
	return d, nil
}

// SortTruths orders pairs for stable output.
func SortTruths(ps []PairTruth) {
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].A != ps[j].A {
			return ps[i].A < ps[j].A
		}
		return ps[i].B < ps[j].B
	})
}
