package cli

import "strings"

// FilterOptions defines filter criteria for listing messages.
type FilterOptions struct {
	Priority string
	From     string
	Kind     string
	Labels   []string
}

// FilterMessages returns items that match all provided filter options.
func FilterMessages(items []listItem, opts FilterOptions) []listItem {
	priority := strings.TrimSpace(opts.Priority)
	from := strings.TrimSpace(opts.From)
	kind := strings.TrimSpace(opts.Kind)
	labels := normalizeFilterLabels(opts.Labels)

	if priority == "" && from == "" && kind == "" && len(labels) == 0 {
		return items
	}

	out := make([]listItem, 0, len(items))
	for _, item := range items {
		if priority != "" && item.Priority != priority {
			continue
		}
		if from != "" && item.From != from {
			continue
		}
		if kind != "" && item.Kind != kind {
			continue
		}
		if len(labels) > 0 && !hasAllLabels(item.Labels, labels) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func normalizeFilterLabels(labels []string) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		out = append(out, label)
	}
	if len(out) == 0 {
		return nil
	}
	return dedupeStrings(out)
}

func hasAllLabels(itemLabels, required []string) bool {
	if len(required) == 0 {
		return true
	}
	if len(itemLabels) == 0 {
		return false
	}

	set := make(map[string]struct{}, len(itemLabels))
	for _, label := range itemLabels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		set[label] = struct{}{}
	}

	for _, label := range required {
		if _, ok := set[label]; !ok {
			return false
		}
	}
	return true
}
