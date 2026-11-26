package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

// DecisionKey is the header context key that carries the options of a
// question to the owner.
const DecisionKey = "decision"

// Limits of a decision object.
const (
	decisionMinOptions  = 2
	decisionMaxOptions  = 6
	decisionLabelMax    = 40
	decisionSummaryMax  = 200
	decisionPointsMax   = 5
	decisionPointMax    = 120
	decisionIfLaterMax  = 200
	decisionContextPath = "context." + DecisionKey
)

// Decision is one decision a question asks for: its options, the option the
// sender recommends, whether work waits on it, and what happens if the
// answer comes later.
type Decision struct {
	Options     []DecisionOption `json:"options"`
	Recommended string           `json:"recommended,omitempty"`
	Blocking    bool             `json:"blocking"`
	IfLater     string           `json:"if_later,omitempty"`
}

// DecisionOption is one answer the owner can give.
type DecisionOption struct {
	Label   string   `json:"label"`
	Summary string   `json:"summary,omitempty"`
	Pros    []string `json:"pros,omitempty"`
	Cons    []string `json:"cons,omitempty"`
}

// IsControl reports C0, DEL, C1, and the Unicode line and paragraph
// separators: the characters no copy command or terminal line may carry.
func IsControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029
}

var jsonIndexRe = regexp.MustCompile(`\.(\d+)`)

// jsonKindName names a decoded Go kind in JSON terms.
func jsonKindName(k reflect.Kind) string {
	switch k {
	case reflect.String:
		return "text"
	case reflect.Bool:
		return "true or false"
	case reflect.Slice, reflect.Array:
		return "a list"
	case reflect.Struct, reflect.Map:
		return "an object"
	}
	return "another type"
}

// ParseDecision reads context.decision. ok is false when the key is absent.
// A present value that breaks the shape is an error that names the field;
// writers refuse it and readers treat the message as a plain question.
func ParseDecision(context map[string]any) (Decision, bool, error) {
	raw, ok := context[DecisionKey]
	if !ok {
		return Decision{}, false, nil
	}
	object, isObject := raw.(map[string]any)
	if !isObject {
		return Decision{}, true, fmt.Errorf("%s: want an object", decisionContextPath)
	}
	if blocking, present := object["blocking"]; present {
		if _, isBool := blocking.(bool); !isBool {
			return Decision{}, true, fmt.Errorf("%s.blocking: want true or false", decisionContextPath)
		}
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return Decision{}, true, fmt.Errorf("%s: %w", decisionContextPath, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var d Decision
	if err := dec.Decode(&d); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			field := jsonIndexRe.ReplaceAllString(typeErr.Field, "[$1]")
			return Decision{}, true, fmt.Errorf("%s.%s: want %s, got %s", decisionContextPath, field, jsonKindName(typeErr.Type.Kind()), typeErr.Value)
		}
		return Decision{}, true, fmt.Errorf("%s: %s", decisionContextPath, strings.TrimPrefix(err.Error(), "json: "))
	}
	if err := d.validate(); err != nil {
		return Decision{}, true, err
	}
	return d, true, nil
}

func (d Decision) validate() error {
	if n := len(d.Options); n < decisionMinOptions || n > decisionMaxOptions {
		return fmt.Errorf("%s.options: want %d to %d options, got %d", decisionContextPath, decisionMinOptions, decisionMaxOptions, n)
	}
	labels := map[string]bool{}
	folded := map[string]bool{}
	for i, o := range d.Options {
		field := fmt.Sprintf("%s.options[%d]", decisionContextPath, i)
		switch {
		case strings.TrimSpace(o.Label) == "":
			return fmt.Errorf("%s.label: empty", field)
		case strings.TrimSpace(o.Label) != o.Label:
			return fmt.Errorf("%s.label: starts or ends with white space", field)
		case utf8.RuneCountInString(o.Label) > decisionLabelMax:
			return fmt.Errorf("%s.label: longer than %d characters", field, decisionLabelMax)
		case strings.ContainsFunc(o.Label, IsControl):
			return fmt.Errorf("%s.label: contains a control character", field)
		case strings.HasPrefix(o.Label, "-") || strings.HasPrefix(o.Label, "@"):
			// The label is the body argument of its answer command, where a
			// leading - or @ reads stdin or a file.
			return fmt.Errorf("%s.label: starts with - or @", field)
		case folded[strings.ToLower(o.Label)]:
			return fmt.Errorf("%s.label: %q repeats an earlier label", field, o.Label)
		case utf8.RuneCountInString(o.Summary) > decisionSummaryMax:
			return fmt.Errorf("%s.summary: longer than %d characters", field, decisionSummaryMax)
		}
		labels[o.Label] = true
		folded[strings.ToLower(o.Label)] = true
		for _, list := range []struct {
			name   string
			points []string
		}{{"pros", o.Pros}, {"cons", o.Cons}} {
			name, points := list.name, list.points
			if len(points) > decisionPointsMax {
				return fmt.Errorf("%s.%s: more than %d entries", field, name, decisionPointsMax)
			}
			for j, p := range points {
				if utf8.RuneCountInString(p) > decisionPointMax {
					return fmt.Errorf("%s.%s[%d]: longer than %d characters", field, name, j, decisionPointMax)
				}
			}
		}
	}
	if d.Recommended != "" && !labels[d.Recommended] {
		return fmt.Errorf("%s.recommended: %q is not one of the option labels", decisionContextPath, d.Recommended)
	}
	if utf8.RuneCountInString(d.IfLater) > decisionIfLaterMax {
		return fmt.Errorf("%s.if_later: longer than %d characters", decisionContextPath, decisionIfLaterMax)
	}
	return nil
}
