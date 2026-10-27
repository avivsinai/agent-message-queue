package board

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Consolidation changes bindings only after the caller reviews this generation.
type Consolidation struct {
	Survivor           string
	Absorb             []string
	Authority          string
	ExpectedGeneration uint64
}
type AliasMove struct {
	Alias, Destination, DonorAuthority, DestinationAuthority string
	Attachments                                              []string
	ExpectedGeneration                                       uint64
}

func requireGeneration(state *Snapshot, expected uint64) error {
	if expected == 0 || state.Generation != expected {
		return fmt.Errorf("board generation changed: expected %d, current %d", expected, state.Generation)
	}
	return nil
}
func chooseAuthority(state *Snapshot, id, authority string) error {
	task := state.Tasks[id]
	if authority == "owner" {
		authority = "operator"
	}
	if authority == "operator" {
		if task.OwnerFact.State == "" {
			task.OwnerFact = Item{Title: task.Title, Source: "operator", State: StatePlanned, Structural: true, Owner: Owner{Kind: OwnerNone}, Reason: Reason{Field: "owner_authority", Text: "Owner authority selected; lifecycle is not recorded", Since: time.Now().UTC()}}
		}
	} else if state.Aliases[authority] != id {
		return errors.New("authority must be an alias bound to the task or owner")
	}
	task.Authority = authority
	state.Tasks[id] = task
	return nil
}
func appendUnique(a []string, values ...string) []string {
	seen := map[string]bool{}
	for _, value := range a {
		seen[value] = true
	}
	for _, value := range values {
		if !seen[value] {
			a = append(a, value)
			seen[value] = true
		}
	}
	sort.Strings(a)
	return a
}

func (s Store) Consolidate(ctx context.Context, request Consolidation) (string, error) {
	canonical := ""
	err := s.update(ctx, func(state *Snapshot) error {
		if err := requireGeneration(state, request.ExpectedGeneration); err != nil {
			return err
		}
		var err error
		canonical, err = state.ResolveTaskID(request.Survivor)
		if err != nil {
			return err
		}
		inputs := map[string]bool{canonical: true}
		for _, id := range request.Absorb {
			target, err := state.ResolveTaskID(id)
			if err != nil {
				return err
			}
			if target == canonical {
				return errors.New("consolidation cannot absorb its survivor")
			}
			inputs[target] = true
		}
		if len(inputs) < 2 {
			return errors.New("consolidation needs at least one distinct task")
		}
		survivor := state.Tasks[canonical]
		for id := range inputs {
			task := state.Tasks[id]
			survivor.Cancelled = survivor.Cancelled || task.Cancelled
			for _, binding := range task.ModelAttachments {
				survivor.ModelAttachments = appendModelAttachment(survivor.ModelAttachments, binding)
			}
			survivor.Attachments = appendUnique(survivor.Attachments, task.Attachments...)
			survivor.AdmissionAnchors = appendUnique(survivor.AdmissionAnchors, task.AdmissionAnchors...)
		}
		state.Tasks[canonical] = survivor
		for alias, id := range state.Aliases {
			if inputs[id] {
				state.Aliases[alias] = canonical
			}
		}
		for anchor, id := range state.Anchors {
			target, err := state.ResolveTaskID(id)
			if err != nil {
				return err
			}
			if inputs[target] {
				state.Anchors[anchor] = canonical
			}
		}
		for id := range inputs {
			if id == canonical {
				continue
			}
			task := state.Tasks[id]
			task.RedirectTo = canonical
			state.Tasks[id] = task
		}
		if err = state.flattenRedirects(); err != nil {
			return err
		}
		state.rebuildMembership()
		if request.Authority == "" {
			return errors.New("consolidation needs explicit --authority")
		}
		return chooseAuthority(state, canonical, request.Authority)
	})
	return canonical, err
}

func (s Store) MoveAlias(ctx context.Context, request AliasMove) (string, error) {
	destination := ""
	err := s.update(ctx, func(state *Snapshot) error {
		if err := requireGeneration(state, request.ExpectedGeneration); err != nil {
			return err
		}
		var err error
		destination, err = state.ResolveTaskID(request.Destination)
		if err != nil {
			return err
		}
		donor, ok := state.Aliases[request.Alias]
		if !ok {
			return errors.New("alias not found")
		}
		if donor == destination {
			return errors.New("alias already belongs to destination")
		}
		from, to := state.Tasks[donor], state.Tasks[destination]
		if from.Authority == request.Alias && request.DonorAuthority == "" {
			return errors.New("moving chosen authority requires explicit replacement for donor")
		}
		for _, key := range request.Attachments {
			found := false
			wholeMessage := false
			for _, attached := range from.Attachments {
				if attached == key {
					found = true
					wholeMessage = true
				}
			}
			for _, binding := range from.ModelAttachments {
				if binding.Unit.MessageKey == key {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("named attachment not on donor: %s", key)
			}
			var kept []string
			for _, attached := range from.Attachments {
				if attached != key {
					kept = append(kept, attached)
				}
			}
			from.Attachments = kept
			if wholeMessage {
				to.Attachments = appendUnique(to.Attachments, key)
			}
			var keptBindings []ModelAttachment
			for _, binding := range from.ModelAttachments {
				if binding.Unit.MessageKey == key {
					to.ModelAttachments = appendModelAttachment(to.ModelAttachments, binding)
				} else {
					keptBindings = append(keptBindings, binding)
				}
			}
			from.ModelAttachments = keptBindings
		}
		state.Tasks[donor], state.Tasks[destination] = from, to
		state.Aliases[request.Alias] = destination
		// Preserve original per-task admission provenance while replay follows the
		// corrected current alias binding.
		if _, ok := state.Anchors[request.Alias]; ok {
			state.Anchors[request.Alias] = destination
			to = state.Tasks[destination]
			to.AdmissionAnchors = appendUnique(to.AdmissionAnchors, request.Alias)
			state.Tasks[destination] = to
		}
		state.rebuildMembership()
		if request.DonorAuthority != "" {
			if err = chooseAuthority(state, donor, request.DonorAuthority); err != nil {
				return err
			}
		}
		if request.DestinationAuthority != "" {
			if err = chooseAuthority(state, destination, request.DestinationAuthority); err != nil {
				return err
			}
		}
		return nil
	})
	return destination, err
}
