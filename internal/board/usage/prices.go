package usage

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ListPrice is the cost kind of a figure computed from the price table: the
// list-price equivalent in US dollars, a measure for comparison, not a bill.
const ListPrice = "list-price equivalent"

// Price is the list price of the models whose id starts with Match, in US
// dollars per million tokens of each class.
type Price struct {
	Match                                string
	Input, CacheRead, CacheWrite, Output float64
	Source, Note                         string
}

// Prices is the price table. The longest Match that prefixes a model id
// prices it; a model no entry matches has no price.
type Prices struct {
	AsOf   string
	Models []Price
}

//go:embed prices.json
var shippedJSON []byte

// shipped is the table built into the binary: entries read from the
// vendors' own pricing pages, each with its source.
var shipped = mustPrices(shippedJSON)

func mustPrices(data []byte) Prices {
	var file struct {
		AsOf   string            `json:"as_of"`
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		panic("usage: prices.json: " + err.Error())
	}
	p := Prices{AsOf: file.AsOf}
	for i, raw := range file.Models {
		e, err := parsePrice(raw)
		if err != nil {
			panic(fmt.Sprintf("usage: prices.json: entry %d: %v", i, err))
		}
		p.Models = append(p.Models, e)
	}
	return p
}

// parsePrice reads one price entry. Every class needs a price of zero or
// more: a missing class would read as free.
func parsePrice(raw json.RawMessage) (Price, error) {
	var e struct {
		Match      string   `json:"match"`
		Input      *float64 `json:"input"`
		CacheRead  *float64 `json:"cache_read"`
		CacheWrite *float64 `json:"cache_write"`
		Output     *float64 `json:"output"`
		Source     string   `json:"source"`
		Note       string   `json:"note"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return Price{}, err
	}
	if strings.TrimSpace(e.Match) == "" {
		return Price{}, errors.New("match is empty")
	}
	for _, c := range []struct {
		name string
		v    *float64
	}{{"input", e.Input}, {"cache_read", e.CacheRead}, {"cache_write", e.CacheWrite}, {"output", e.Output}} {
		if c.v == nil || *c.v < 0 {
			return Price{}, fmt.Errorf("%s needs a price of zero or more", c.name)
		}
	}
	return Price{Match: e.Match, Input: *e.Input, CacheRead: *e.CacheRead, CacheWrite: *e.CacheWrite, Output: *e.Output, Source: e.Source, Note: e.Note}, nil
}

// with returns the table with the user's entries: an entry replaces the one
// with the same Match, or is added.
func (p Prices) with(user []Price) Prices {
	out := Prices{AsOf: p.AsOf, Models: append([]Price(nil), p.Models...)}
	for _, u := range user {
		replaced := false
		for i := range out.Models {
			if out.Models[i].Match == u.Match {
				out.Models[i], replaced = u, true
			}
		}
		if !replaced {
			out.Models = append(out.Models, u)
		}
	}
	return out
}

func (p Prices) lookup(model string) (Price, bool) {
	var best Price
	found := false
	for _, e := range p.Models {
		if strings.HasPrefix(model, e.Match) && (!found || len(e.Match) > len(best.Match)) {
			best, found = e, true
		}
	}
	return best, found
}

// price gives an event its cost: a cost the harness recorded stays, a
// priced model gets its list-price equivalent, and the tokens of a model
// with no price are counted as unpriced.
func (p Prices) price(model string, c Counts) Counts {
	if c.Cost != nil {
		return c
	}
	e, ok := p.lookup(model)
	if !ok || model == "" {
		c.Unpriced = c.In + c.CacheRead + c.CacheWrite + c.Out
		return c
	}
	cost := (float64(c.In)*e.Input + float64(c.CacheRead)*e.CacheRead + float64(c.CacheWrite)*e.CacheWrite + float64(c.Out)*e.Output) / 1e6
	c.Cost, c.CostKind = &cost, ListPrice
	return c
}
