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
	results := make([]NamedResult, len(sources))
	var wg sync.WaitGroup
	for i, s := range sources {
		results[i].Name = s.Name()
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

	return Build(Input{Results: results, Known: known, Config: cfg, Now: env.Now(), Version: version, Dir: env.Dir})
}

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
				if _, ok := known[m.Token]; ok || seen[m.Token] {
					continue
				}
				if _, ok := Bead(m.Token); !ok {
					continue
				}
				seen[m.Token] = true
				out = append(out, m.Token)
			}
		}
	}
	sort.Strings(out)
	return out
}

func joinDetail(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
