package board

import (
	"fmt"
	"time"
)

// FreshnessWindow supplies the documented default when a pure view fixture
// does not pass board settings. Loaded settings keep their explicit bound.
func FreshnessWindow(cfg Config) time.Duration {
	if cfg.LivenessWindow == 0 {
		return DefaultConfig().LivenessWindow
	}
	return cfg.LivenessWindow
}

// CheckFreshness is check's publication guard. Retro shares it exactly;
// read-only views instead expose the same verdict and continue as history.
// Sources are never read or retried here.
func CheckFreshness(s Snapshot, cfg Config, now time.Time) error {
	if s.PublishedAt.IsZero() {
		return fmt.Errorf("the board has no published snapshot; run amq-board sync --init")
	}
	if elapsed := now.Sub(s.PublishedAt); elapsed > FreshnessWindow(cfg) {
		return fmt.Errorf("the board snapshot is %s old; run amq-board sync first", age(elapsed))
	}
	return nil
}

// StalenessText is the prominent old-fact marker for text and HTML views.
// Sources might remain healthy; only this publication is old.
func StalenessText(m Model) string {
	if m.PublishedAt.IsZero() || nowAge(m) <= FreshnessWindow(m.Config) {
		return ""
	}
	published := nowAge(m)
	age := age(published)
	if published >= time.Hour && published < 48*time.Hour {
		age = fmt.Sprintf("%dh", int(published.Hours()))
	}
	return fmt.Sprintf("STALE: published %s ago (%s). Every number below is from then. Run amq-board sync.", age, m.PublishedAt.UTC().Format(time.RFC3339))
}

func nowAge(m Model) time.Duration {
	if d := m.GeneratedAt.Sub(m.PublishedAt); d > 0 {
		return d
	}
	return 0
}

// MarkStale keeps every publication age explicit, including a live view that
// remains open across its freshness bound. No configuration retry belongs here.
func MarkStale(m *Model) {
	if m == nil {
		return
	}
	m.PublishedAgeSeconds = max(0, int64(nowAge(*m)/time.Second))
	m.Stale = StalenessText(*m) != ""
}
