package usage

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// ListPrice is the cost kind of a figure computed from the price table: the
// list-price equivalent in US dollars, a measure for comparison, not a bill.
const ListPrice = "list-price equivalent"

// maxRate bounds a price in dollars per million tokens: no list price is
// near it, and a sum of rates below it stays finite.
const maxRate = 1_000_000

// Rates are the list prices of each token class, in US dollars per million
// tokens. CacheWrite1h prices the cache writes a harness recorded as 1-hour
// writes; the rest of the writes are priced at CacheWrite.
type Rates struct {
	Input, CacheRead, CacheWrite, CacheWrite1h, Output float64
}

// Price is the list price of the models whose id starts with Match. A
// request whose prompt (input, cache reads and cache writes) is over
// OverPromptTokens is priced whole at Over.
type Price struct {
	Match string
	Rates
	OverPromptTokens int64
	Over             *Rates
	Source, Note     string
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

type rawRates struct {
	Input        *float64 `json:"input"`
	CacheRead    *float64 `json:"cache_read"`
	CacheWrite   *float64 `json:"cache_write"`
	CacheWrite1h *float64 `json:"cache_write_1h"`
	Output       *float64 `json:"output"`
}

// rates checks each class: every class but cache_write_1h needs a price
// from 0 to maxRate, since a missing class would read as free; a missing
// cache_write_1h prices 1-hour writes at cache_write.
func (r rawRates) rates(prefix string) (Rates, error) {
	if r.CacheWrite1h == nil {
		r.CacheWrite1h = r.CacheWrite
	}
	for _, c := range []struct {
		name string
		v    *float64
	}{{"input", r.Input}, {"cache_read", r.CacheRead}, {"cache_write", r.CacheWrite}, {"cache_write_1h", r.CacheWrite1h}, {"output", r.Output}} {
		if c.v == nil || math.IsNaN(*c.v) || *c.v < 0 || *c.v > maxRate {
			return Rates{}, fmt.Errorf("%s%s needs a price from 0 to %d", prefix, c.name, maxRate)
		}
	}
	return Rates{Input: *r.Input, CacheRead: *r.CacheRead, CacheWrite: *r.CacheWrite, CacheWrite1h: *r.CacheWrite1h, Output: *r.Output}, nil
}

// parsePrice reads one price entry.
func parsePrice(raw json.RawMessage) (Price, error) {
	var e struct {
		Match string `json:"match"`
		rawRates
		OverPromptTokens int64     `json:"over_prompt_tokens"`
		Over             *rawRates `json:"over"`
		Source           string    `json:"source"`
		Note             string    `json:"note"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return Price{}, err
	}
	if strings.TrimSpace(e.Match) == "" {
		return Price{}, errors.New("match is empty")
	}
	r, err := e.rates("")
	if err != nil {
		return Price{}, err
	}
	p := Price{Match: e.Match, Rates: r, Source: e.Source, Note: e.Note}
	switch {
	case e.OverPromptTokens < 0 || (e.OverPromptTokens > 0) != (e.Over != nil):
		return Price{}, errors.New("over_prompt_tokens and over go together")
	case e.Over != nil:
		over, err := e.Over.rates("over.")
		if err != nil {
			return Price{}, err
		}
		p.OverPromptTokens, p.Over = e.OverPromptTokens, &over
	}
	return p, nil
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
// priced model gets its list-price equivalent at the tier of the request's
// prompt size, and the tokens of a model with no price are counted as
// unpriced.
func (p Prices) price(model string, prompt int64, c Counts) Counts {
	if c.Cost != nil {
		return c
	}
	e, ok := p.lookup(model)
	if !ok || model == "" {
		c.Unpriced = c.In + c.CacheRead + c.CacheWrite + c.Out
		return c
	}
	r := e.Rates
	if e.Over != nil && prompt > e.OverPromptTokens {
		r = *e.Over
	}
	cost := (float64(c.In)*r.Input + float64(c.CacheRead)*r.CacheRead + float64(c.CacheWrite-c.CacheWrite1h)*r.CacheWrite +
		float64(c.CacheWrite1h)*r.CacheWrite1h + float64(c.Out)*r.Output) / 1e6
	c.Cost, c.CostKind = &cost, ListPrice
	return c
}
