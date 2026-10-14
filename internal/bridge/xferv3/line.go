package xferv3

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

const (
	linePrefix     = "amq1:"
	FingerprintLen = 26
)

type Line struct {
	Host   Host
	Record string
	Ticket string
	X25519 [32]byte
}

func FormatLine(host Host, record, ticket string, xPublic [32]byte) (string, error) {
	if host.s == "" || strings.TrimSpace(record) == "" {
		return "", ErrName
	}
	if strings.ContainsAny(record, "\n;") || strings.ContainsAny(ticket, "\n;") {
		return "", ErrName
	}
	line := linePrefix + host.s + ":" + strings.TrimSpace(record) + ";x25519=" + hex.EncodeToString(xPublic[:])
	if ticket != "" {
		line += ";pipe=" + ticket
	}
	return line, nil
}

func ParseLine(raw string) (Line, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.ReplaceAll(raw, "\n", ";")
	if !strings.HasPrefix(raw, linePrefix) {
		return Line{}, ErrName
	}
	rest := strings.TrimPrefix(raw, linePrefix)
	hostName, rest, ok := strings.Cut(rest, ":")
	if !ok {
		return Line{}, ErrName
	}
	host, err := ParseHost(hostName)
	if err != nil {
		return Line{}, err
	}
	parts := strings.Split(rest, ";")
	if len(parts) == 0 || parts[0] == "" {
		return Line{}, ErrName
	}
	record := parts[0]
	var ticket string
	var xRaw []byte
	for _, part := range parts[1:] {
		key, value, cut := strings.Cut(part, "=")
		if !cut || value == "" {
			return Line{}, ErrName
		}
		switch key {
		case "pipe":
			ticket = value
		case "x25519":
			xRaw, err = hex.DecodeString(value)
			if err != nil || len(xRaw) != 32 || value != hex.EncodeToString(xRaw) {
				return Line{}, ErrName
			}
		default:
			return Line{}, ErrName
		}
	}
	if len(xRaw) != 32 {
		return Line{}, ErrName
	}
	var xPublic [32]byte
	copy(xPublic[:], xRaw)
	return Line{Host: host, Record: record, Ticket: ticket, X25519: xPublic}, nil
}

func (l Line) Fingerprint() string {
	sum := sha256.Sum256([]byte(l.Host.s + "\n" + l.Record + "\n" + hex.EncodeToString(l.X25519[:])))
	text := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))
	if len(text) < FingerprintLen {
		return text
	}
	return text[:FingerprintLen]
}
