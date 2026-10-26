package board

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// AliasFact contains source observations only. The Aliases map separately
// assigns it to an owner-chosen task; acquisition cannot choose task authority.
type AliasFact struct {
	Alias      string    `json:"alias"`
	Fact       Item      `json:"fact"`
	ObservedAt time.Time `json:"observed_at"`
	Missing    bool      `json:"missing"`
}

type TaskCatalogueEntry struct {
	Inferred         bool              `json:"inferred"`
	ModelAttachments []ModelAttachment `json:"model_attachments,omitempty"`
	ID               string            `json:"id"`
	Title            string            `json:"title"`
	Authority        string            `json:"authority"`
	State            State             `json:"recorded_state"`
	Cancelled        bool              `json:"cancelled"`
	Historical       bool              `json:"historical"`
	Aliases          []string          `json:"aliases"`
	Facts            []AliasFact       `json:"source_facts"`
	Attachments      []string          `json:"attachments"`
	AdmissionAnchors []string          `json:"admission_anchors"`
	CopiedIDs        []string          `json:"copied_ids"`
}

func (s Snapshot) ResolveTaskID(id string) (string, error) {
	seen := map[string]bool{}
	for {
		if seen[id] {
			return "", errors.New("task redirect cycle; recovery required")
		}
		seen[id] = true
		task, ok := s.Tasks[id]
		if !ok {
			return "", fmt.Errorf("task not found: %s", id)
		}
		if task.RedirectTo == "" {
			return id, nil
		}
		id = task.RedirectTo
	}
}
func (s *Snapshot) hydrateAuthorities() {
	for id, task := range s.Tasks {
		if task.Authority == "operator" {
			task.Fact = task.OwnerFact
			task.SourceMissing = false
			task.ObservedAt = time.Time{}
		} else if fact, ok := s.Facts[task.Authority]; ok {
			task.Fact = fact.Fact
			task.ObservedAt = fact.ObservedAt
			task.SourceMissing = fact.Missing
		}
		s.Tasks[id] = task
	}
}
func (s Snapshot) projectionSnapshot() Snapshot {
	copy := s
	copy.Tasks = make(map[string]Task, len(s.Tasks))
	for id, task := range s.Tasks {
		copy.Tasks[id] = task
	}
	copy.hydrateAuthorities()
	return copy
}
func (s Snapshot) redirects() map[string]string {
	out := map[string]string{}
	for id, task := range s.Tasks {
		if task.RedirectTo != "" {
			out[id] = task.RedirectTo
		}
	}
	return out
}
func (s Snapshot) Catalogue() []TaskCatalogueEntry {
	s = s.projectionSnapshot()
	ids := make([]string, 0, len(s.Tasks))
	for id, task := range s.Tasks {
		if task.RedirectTo == "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := make([]TaskCatalogueEntry, 0, len(ids))
	for _, id := range ids {
		task := s.Tasks[id]
		entry := TaskCatalogueEntry{Inferred: task.Inferred, ModelAttachments: task.ModelAttachments, ID: id, Title: task.Title, Authority: task.Authority, State: task.Fact.State, Cancelled: task.Cancelled, Historical: task.SourceMissing, Aliases: task.Aliases, Attachments: task.Attachments, AdmissionAnchors: task.AdmissionAnchors, Facts: []AliasFact{}, CopiedIDs: []string{}}
		for _, alias := range task.Aliases {
			entry.Facts = append(entry.Facts, s.Facts[alias])
		}
		for copied, redirect := range s.redirects() {
			if redirect == id {
				entry.CopiedIDs = append(entry.CopiedIDs, copied)
			}
		}
		sort.Strings(entry.CopiedIDs)
		result = append(result, entry)
	}
	return result
}

func (s *Snapshot) migrateLegacy(data []byte) error {
	var legacy struct {
		Tasks map[string]struct {
			Fact       Item      `json:"source_fact"`
			ObservedAt time.Time `json:"source_observed_at"`
			Missing    bool      `json:"source_missing"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return fmt.Errorf("board migration recovery required: %w", err)
	}
	s.Facts = map[string]AliasFact{}
	for id, task := range s.Tasks {
		if task.RedirectTo != "" {
			return errors.New("legacy redirects unsupported; recovery required")
		}
		old, ok := legacy.Tasks[id]
		if !ok || old.Fact.State == "" {
			return errors.New("legacy task fact missing; recovery required")
		}
		if task.Authority == "operator" {
			if len(task.Aliases) != 0 {
				return errors.New("legacy owner authority has inconsistent aliases; recovery required")
			}
			task.OwnerFact = old.Fact
		} else {
			alias, valid := nativeAlias(old.Fact.Source, old.Fact)
			if !valid || alias != task.Authority || len(task.Aliases) != 1 || task.Aliases[0] != alias || s.Aliases[alias] != id {
				return errors.New("legacy source identity inconsistent; recovery required")
			}
			s.Facts[alias] = AliasFact{Alias: alias, Fact: old.Fact, ObservedAt: old.ObservedAt, Missing: old.Missing}
		}
		for anchor, target := range s.Anchors {
			if target == id {
				task.AdmissionAnchors = append(task.AdmissionAnchors, anchor)
			}
		}
		sort.Strings(task.AdmissionAnchors)
		s.Tasks[id] = task
	}
	if len(s.Facts) != len(s.Aliases) {
		return errors.New("legacy alias fact missing; recovery required")
	}
	s.Schema = SnapshotSchema
	return nil
}

func (s Snapshot) validateIdentityRecords() error {
	memberships := map[string]bool{}
	for id, task := range s.Tasks {
		target, err := s.ResolveTaskID(id)
		if err != nil {
			return err
		}
		if task.RedirectTo != "" {
			if target != task.RedirectTo {
				return errors.New("task redirects must be direct; recovery required")
			}
			continue
		}
		if task.Authority == "operator" {
			if task.OwnerFact.State == "" {
				return errors.New("owner authority missing; recovery required")
			}
		} else {
			if s.Aliases[task.Authority] != id {
				return errors.New("chosen source authority is not bound to task; recovery required")
			}
		}
		for _, alias := range task.Aliases {
			if memberships[alias] || s.Aliases[alias] != id {
				return errors.New("alias membership inconsistent; recovery required")
			}
			memberships[alias] = true
		}
	}
	for alias, id := range s.Aliases {
		target, err := s.ResolveTaskID(id)
		if err != nil || target != id || !memberships[alias] {
			return errors.New("alias binding is not canonical; recovery required")
		}
		fact, ok := s.Facts[alias]
		if !ok || fact.Alias != alias {
			return errors.New("alias fact missing; recovery required")
		}
		key, ok := nativeAlias(fact.Fact.Source, fact.Fact)
		if !ok || key != alias {
			return errors.New("alias fact identity inconsistent; recovery required")
		}
	}
	if len(s.Facts) != len(s.Aliases) {
		return errors.New("unbound source fact; recovery required")
	}
	return nil
}

func (s *Snapshot) rebuildMembership() {
	for id, task := range s.Tasks {
		if task.RedirectTo == "" {
			task.Aliases = []string{}
			s.Tasks[id] = task
		}
	}
	for alias, id := range s.Aliases {
		task := s.Tasks[id]
		task.Aliases = append(task.Aliases, alias)
		s.Tasks[id] = task
	}
	for id, task := range s.Tasks {
		sort.Strings(task.Aliases)
		s.Tasks[id] = task
	}
}
func (s *Snapshot) flattenRedirects() error {
	for id, task := range s.Tasks {
		if task.RedirectTo != "" {
			canonical, err := s.ResolveTaskID(id)
			if err != nil {
				return err
			}
			task.RedirectTo = canonical
			s.Tasks[id] = task
		}
	}
	return nil
}

// Redirects lists retained copied IDs and their direct canonical targets.
func (s Snapshot) Redirects() map[string]string { return s.redirects() }
