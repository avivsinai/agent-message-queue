package board

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// SourceTimeout bounds one source read.
const SourceTimeout = 60 * time.Second

// maxBeadLookups caps the closed-bead lookup per refresh (design 4.1).
const maxBeadLookups = 200

// Run reads every source once, each under its own deadline and error
// boundary, resolves bead mentions, and builds the model. One failing source
// never empties another source's cards.
func Run(ctx context.Context, env Env, sources []Source, cfg Config, version string) Model {
	return Build(Acquire(ctx, env, sources, cfg, version))
}

// Acquire reads source facts without assigning canonical task identities.
func Acquire(ctx context.Context, env Env, sources []Source, cfg Config, version string) Input {
	results := make([]NamedResult, len(sources))
	var wg sync.WaitGroup
	for i, s := range sources {
		results[i].Name = s.Name()
		if _, off := s.(Disabled); off {
			results[i].Result = Result{Status: StatusDisabled, Detail: "disabled in configuration"}
			continue
		}
		if ok, why := s.Discover(env); !ok {
			results[i].Result = Result{Status: StatusUnavailable, Detail: why}
			continue
		}
		wg.Add(1)
		go func(i int, s Source) {
			defer wg.Done()
			results[i].Result = readIsolated(ctx, env, s)
		}(i, s)
	}
	wg.Wait()

	known := map[string]string{}
	var resolver BeadResolver
	resolverAt := -1
	for i, nr := range results {
		for id, status := range nr.Result.BeadIDs {
			known[id] = status
		}
		if r, ok := sources[i].(BeadResolver); ok && nr.Result.Status == StatusOK {
			resolver, resolverAt = r, i
		}
	}
	if resolver != nil {
		candidates := mentionCandidates(results, known)
		over := 0
		if len(candidates) > maxBeadLookups {
			over = len(candidates) - maxBeadLookups
			candidates = candidates[:maxBeadLookups]
		}
		if len(candidates) > 0 {
			lctx, cancel := context.WithTimeout(ctx, SourceTimeout)
			found, err := resolver.ResolveBeads(lctx, env, candidates)
			cancel()
			r := &results[resolverAt].Result
			if err != nil {
				r.Coverage.Unresolved += len(candidates)
				r.Coverage.Detail = joinDetail(r.Coverage.Detail, fmt.Sprintf("%d mentioned bead ids not resolved: %v", len(candidates), err))
			}
			for id, status := range found {
				known[id] = status
			}
		}
		if over > 0 {
			r := &results[resolverAt].Result
			r.Coverage.Unresolved += over
			r.Coverage.Detail = joinDetail(r.Coverage.Detail, fmt.Sprintf("%d mentioned bead ids over the lookup cap", over))
		}
	}

	return Input{Results: results, Known: known, Config: cfg, Now: env.Now(), Version: version, Dir: env.Dir}
}

// Disabled stands in for a source the configuration turns off, so the
// sources strip shows it as disabled instead of leaving it out.
type Disabled struct{ SourceName string }

func (d Disabled) Name() string                   { return d.SourceName }
func (Disabled) Discover(Env) (bool, string)      { return false, "disabled in configuration" }
func (Disabled) Read(context.Context, Env) Result { return Result{Status: StatusDisabled} }

func readIsolated(ctx context.Context, env Env, s Source) (res Result) {
	ctx, cancel := context.WithTimeout(ctx, SourceTimeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			res = Result{Status: StatusDegraded, Detail: fmt.Sprintf("source failed: %v", p)}
		}
	}()
	return s.Read(ctx, env)
}

// mentionCandidates lists candidate bead tokens that are not already known,
// sorted so the cap is deterministic.
func mentionCandidates(results []NamedResult, known map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, nr := range results {
		for _, it := range nr.Result.Items {
			for _, m := range it.Mentions {
				cands := BeadCandidates(m.Token)
				if anyKnown(cands, known) {
					continue
				}
				for _, c := range cands {
					if !seen[c] {
						seen[c] = true
						out = append(out, c)
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func anyKnown(ids []string, known map[string]string) bool {
	for _, id := range ids {
		if _, ok := known[id]; ok {
			return true
		}
	}
	return false
}

func joinDetail(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
