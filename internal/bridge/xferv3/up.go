package xferv3

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	ErrEmptyLedger = fmt.Errorf("xferv3: empty ledger beside a non-empty Maildir")
	ErrUpRunning   = fmt.Errorf("xferv3: up is already running")
)

type OwedInfo struct {
	Peer   string
	ID     string
	Reason string
	Age    time.Duration
	Next   time.Time
}

type Report struct {
	Owed        []OwedInfo
	OldestAge   time.Duration
	LedgerBytes int64
	Uncertain   int
	Unhealthy   bool
}

func CheckEmptyLedger(root string, acknowledge bool) error {
	mail, err := mailPresent(root)
	if err != nil {
		return err
	}
	ledger, err := ledgerPresent(root)
	if err != nil {
		return err
	}
	if !mail || ledger {
		return nil
	}
	ackPath := emptyLedgerAckPath(root)
	if acknowledge {
		if err := os.MkdirAll(filepath.Dir(ackPath), 0o700); err != nil {
			return err
		}
		return os.WriteFile(ackPath, []byte("acknowledged\n"), 0o600)
	}
	if _, err := os.Lstat(ackPath); err == nil {
		return nil
	}
	return ErrEmptyLedger
}

func CollectStatus(root string) (Report, error) {
	var report Report
	peers, err := ListPeers(root)
	if err != nil {
		return Report{}, err
	}
	now := time.Now()
	for _, peer := range peers {
		owed, err := ListTx(root, peer.Host())
		if err != nil {
			return Report{}, err
		}
		for _, item := range owed {
			info, err := os.Lstat(item.Path)
			if err != nil {
				return Report{}, err
			}
			reason := readNoReason(item.Path)
			next := nextTry(root, peer.Host(), item.Signed.ID())
			age := now.Sub(info.ModTime())
			if age < 0 {
				age = 0
			}
			report.Owed = append(report.Owed, OwedInfo{
				Peer:   peer.Host().String(),
				ID:     item.Signed.ID().String(),
				Reason: reason,
				Age:    age,
				Next:   next,
			})
			if age > report.OldestAge {
				report.OldestAge = age
			}
			if age >= 14*24*time.Hour {
				report.Unhealthy = true
			}
		}
	}
	bytes, uncertain, err := ledgerStats(root)
	if err != nil {
		return Report{}, err
	}
	report.LedgerBytes = bytes
	report.Uncertain = uncertain
	if uncertain > 0 || bytes > 64<<20 {
		report.Unhealthy = true
	}
	return report, nil
}

func Discard(root string, peer Host, id ID) error {
	if peer.s == "" || id == (ID{}) {
		return ErrName
	}
	return withIDLock(root, id, func() error {
		if err := appendFact(root, Fact{State: StateDiscard, Peer: peer, ID: id}); err != nil {
			return err
		}
		path := filepath.Join(root, "bridge", txDirName, peer.s, id.String())
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = os.Remove(path + ".no")
		return nil
	})
}

func SetBackoff(root string, peer Host, id ID, reason string, now time.Time) error {
	path := backoffPath(root, peer, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	delay := 5 * time.Second
	if raw, err := os.ReadFile(path); err == nil {
		if n, conv := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); conv == nil && n > 0 {
			prev := time.Unix(n, 0)
			if prev.After(now) {
				delay = prev.Sub(now) * 2
			}
		}
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	body := strconv.FormatInt(now.Add(delay).Unix(), 10) + " " + reason + "\n"
	return os.WriteFile(path, []byte(body), 0o600)
}

func Due(root string, peer Host, id ID, now time.Time) bool {
	next := nextTry(root, peer, id)
	if next.IsZero() {
		return true
	}
	return !now.Before(next)
}

func LaunchdPlist(bin, root string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>io.github.avivsinai.amq-bridge-up</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + bin + `</string>
		<string>up</string>
		<string>--root</string>
		<string>` + root + `</string>
	</array>
	<key>KeepAlive</key>
	<true/>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`
}

func pidPath(root string) string {
	return filepath.Join(root, "bridge", "up.pid")
}

func emptyLedgerAckPath(root string) string {
	return filepath.Join(root, "bridge", "empty-ledger.ack")
}

func backoffPath(root string, peer Host, id ID) string {
	return filepath.Join(root, "bridge", "backoff", peer.s, id.String())
}

func nextTry(root string, peer Host, id ID) time.Time {
	raw, err := os.ReadFile(backoffPath(root, peer, id))
	if err != nil {
		return time.Time{}
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return time.Time{}
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func readNoReason(path string) string {
	raw, err := os.ReadFile(path + ".no")
	if err != nil {
		return ""
	}
	_, reason, err := parseNo(raw)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return reason
}

func mailPresent(root string) (bool, error) {
	agents := filepath.Join(root, "agents")
	found := false
	err := filepath.WalkDir(agents, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(filepath.Dir(path))
		if (base == "new" || base == "cur") && strings.HasSuffix(d.Name(), ".md") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found, err
}

func ledgerPresent(root string) (bool, error) {
	dir := filepath.Join(root, "bridge", "ledger", "v3")
	found := false
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasSuffix(d.Name(), ".lock") || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		found = true
		return fs.SkipAll
	})
	return found, err
}

func ledgerStats(root string) (int64, int, error) {
	dir := filepath.Join(root, "bridge", "ledger", "v3")
	var bytes int64
	var uncertain int
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasSuffix(d.Name(), ".lock") || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		bytes += info.Size()
		facts, _, readErr := readLedger(path)
		if readErr != nil {
			uncertain++
			return nil
		}
		if foldState(facts) == StateUncertain {
			uncertain++
		}
		return nil
	})
	return bytes, uncertain, err
}
