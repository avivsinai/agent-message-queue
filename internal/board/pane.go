package board

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const PaneSchema = "amq/board-pane/v1"
const PaneJSONLimit = 4 << 20
const paneWaitingLimit = 100
const paneSourceLimit = 32

type PaneColumn struct {
	ID    Column `json:"id"`
	Total int    `json:"total"`
}
type PaneSource struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Partial bool   `json:"partial"`
}
type PaneStrip struct {
	TaskID  string    `json:"task_id"`
	Title   string    `json:"title"`
	Reason  string    `json:"reason"`
	Since   time.Time `json:"since"`
	Command string    `json:"command"`
}

// PaneModel is a bounded presentation of current tasks, never an evidence export.
type PaneModel struct {
	Schema         string       `json:"schema"`
	Generation     uint64       `json:"generation"`
	GeneratedAt    time.Time    `json:"generated_at"`
	PublishedAt    time.Time    `json:"published_at"`
	Dir            string       `json:"dir"`
	Project        string       `json:"project"`
	Columns        []PaneColumn `json:"columns"`
	CurrentTotal   int          `json:"current_total"`
	Waiting        []PaneStrip  `json:"waiting"`
	WaitingTotal   int          `json:"waiting_total"`
	WaitingOmitted int          `json:"waiting_omitted"`
	Sources        []PaneSource `json:"sources"`
	SourcesOmitted int          `json:"sources_omitted"`
}

func paneText(text string, limit int) string {
	out := make([]rune, 0, limit)
	for _, r := range text {
		switch r {
		case '\t', '\n', '\r', '\v', '\f', '\u2028', '\u2029':
			r = ' '
		default:
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				continue
			}
		}
		if len(out) == limit {
			return string(out[:limit-1]) + "…"
		}
		out = append(out, r)
	}
	return string(out)
}
func paneCommand(command string) bool {
	if strings.TrimSpace(command) == "" || !utf8.ValidString(command) || utf8.RuneCountInString(command) > 4096 {
		return false
	}
	for _, r := range command {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

// ProjectPane retains complete current counts while bounding display rows.
func ProjectPane(model Model) PaneModel {
	p := PaneModel{Schema: PaneSchema, Generation: model.Generation, GeneratedAt: model.GeneratedAt, PublishedAt: model.PublishedAt, Dir: model.Dir, Project: paneText(model.Project, 160), Columns: []PaneColumn{}, Waiting: []PaneStrip{}, Sources: []PaneSource{}}
	for _, column := range model.Columns {
		total := len(column.Cards)
		p.Columns = append(p.Columns, PaneColumn{ID: column.ID, Total: total})
		p.CurrentTotal += total
		if column.ID != ColumnWaiting {
			continue
		}
		p.WaitingTotal += total
		for _, card := range column.Cards {
			if len(p.Waiting) == paneWaitingLimit {
				continue
			}
			strip := PaneStrip{TaskID: card.ID, Title: paneText(card.Title, 160), Reason: paneText(card.Reason.Text, 160), Since: card.Reason.Since}
			for _, action := range card.Actions {
				if action.Purpose != ActionContext || strings.TrimSpace(action.Command) == "" {
					continue
				}
				if paneCommand(action.Command) {
					strip.Command = action.Command
				}
				break
			}
			p.Waiting = append(p.Waiting, strip)
		}
	}
	p.WaitingOmitted = p.WaitingTotal - len(p.Waiting)
	for _, source := range model.Sources {
		if len(p.Sources) == paneSourceLimit {
			break
		}
		p.Sources = append(p.Sources, PaneSource{Name: paneText(source.Name, 40), Status: paneText(string(source.Status), 20), Partial: source.Partial})
	}
	p.SourcesOmitted = len(model.Sources) - len(p.Sources)
	return p
}

// PaneJSON is shared by the CLI and cached HTTP route. Overflow emits no data.
func PaneJSON(model Model) ([]byte, error) {
	data, err := json.Marshal(ProjectPane(model))
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if len(data) > PaneJSONLimit {
		return nil, errors.New("board pane projection exceeds 4 MiB")
	}
	return data, nil
}
