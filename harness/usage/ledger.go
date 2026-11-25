package usage

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const EMAAlpha = 0.35
const BurnLookback = 3 * time.Hour

// Counter is a sum with the number of records missing that metric. A missing
// value in JSON means there was no measurement, including for charges.
type Counter[T int64 | float64] struct {
	Value   *T  `json:"value,omitempty"`
	Unknown int `json:"unknown,omitempty"`
}

func (c *Counter[T]) add(value *T, n int) {
	if value == nil {
		c.Unknown += n
		return
	}
	if c.Value == nil {
		c.Value = new(*value)
	} else {
		*c.Value += *value
	}
}

func (c Counter[T]) snapshot() Counter[T] {
	if c.Value != nil {
		c.Value = new(*c.Value)
	}
	return c
}

type Amounts struct {
	Currency      string `json:"currency"`
	BillingSource string `json:"billing_source,omitempty"`
	Calls         int    `json:"calls"`
	Charged, List Counter[float64]
}

func (a Amounts) PerCall() *float64 {
	if a.Charged.Value == nil || a.Calls <= a.Charged.Unknown {
		return nil
	}
	return new(*a.Charged.Value / float64(a.Calls-a.Charged.Unknown))
}

type Totals struct {
	Calls                                                     int
	Input, Uncached, CacheRead, CacheWrite, Output, Reasoning Counter[int64]
	Charges                                                   []Amounts `json:",omitempty"`
	ChargeUnknown                                             int       `json:",omitempty"`
}

// DollarText names partial measurements as such when some calls in the group
// have no reported charge.
func (t Totals) DollarText() string {
	var parts []string
	for _, a := range t.Charges {
		if a.BillingSource != "" || a.Charged.Value == nil {
			continue
		}
		parts = append(parts, Money(a.Currency, *a.Charged.Value, 2))
	}
	if len(parts) == 0 {
		return "unknown"
	}
	text := strings.Join(parts, " + ")
	if t.ChargeUnknown > 0 {
		text += " (partial)"
	}
	return text
}

func Money(currency string, v float64, precision int) string {
	if currency == "USD" {
		return fmt.Sprintf("$%.*f", precision, v)
	}
	return fmt.Sprintf("%.*f %s", precision, v, currency)
}

type Group struct {
	Provider, Model, Role string `json:",omitempty"`
	Totals
}

type Window struct {
	Totals                                   Totals
	ByProvider, ByModel, ByRole, ByModelRole []Group `json:",omitempty"`
}

type TotalsBuilder struct {
	totals  Totals
	charges map[chargeKey]*Amounts
}
type chargeKey struct{ currency, source string }

func (b *TotalsBuilder) Add(s Sample) {
	t := &b.totals
	t.Calls += s.Calls
	t.Input.add(s.Input, s.Calls)
	t.Uncached.add(s.Uncached, s.Calls)
	t.CacheRead.add(s.CacheRead, s.Calls)
	t.CacheWrite.add(s.CacheWrite, s.Calls)
	t.Output.add(s.Output, s.Calls)
	t.Reasoning.add(s.Reasoning, s.Calls)
	if s.Charge == nil || s.Charge.Charged == nil {
		t.ChargeUnknown += s.Calls
	}
	if c := s.Charge; c != nil && c.Currency != "" {
		if b.charges == nil {
			b.charges = make(map[chargeKey]*Amounts)
		}
		b.charge(chargeKey{currency: c.Currency}, c, s.Calls)
		if c.BillingSource != "" {
			b.charge(chargeKey{c.Currency, c.BillingSource}, c, s.Calls)
		}
	}
}

func (b *TotalsBuilder) charge(key chargeKey, c *llm.Charge, count int) {
	a := b.charges[key]
	if a == nil {
		a = &Amounts{Currency: key.currency, BillingSource: key.source}
		b.charges[key] = a
	}
	a.Calls += count
	a.Charged.add(c.Charged, count)
	a.List.add(c.List, count)
}

func (b *TotalsBuilder) Merge(other Totals) {
	t := &b.totals
	t.Calls += other.Calls
	t.Input.add(other.Input.Value, 0)
	t.Uncached.add(other.Uncached.Value, 0)
	t.CacheRead.add(other.CacheRead.Value, 0)
	t.CacheWrite.add(other.CacheWrite.Value, 0)
	t.Output.add(other.Output.Value, 0)
	t.Reasoning.add(other.Reasoning.Value, 0)
	t.Input.Unknown += other.Input.Unknown
	t.Uncached.Unknown += other.Uncached.Unknown
	t.CacheRead.Unknown += other.CacheRead.Unknown
	t.CacheWrite.Unknown += other.CacheWrite.Unknown
	t.Output.Unknown += other.Output.Unknown
	t.Reasoning.Unknown += other.Reasoning.Unknown
	t.ChargeUnknown += other.ChargeUnknown
	for _, a := range other.Charges {
		if b.charges == nil {
			b.charges = make(map[chargeKey]*Amounts)
		}
		key := chargeKey{a.Currency, a.BillingSource}
		dst := b.charges[key]
		if dst == nil {
			dst = &Amounts{Currency: a.Currency, BillingSource: a.BillingSource}
			b.charges[key] = dst
		}
		dst.Calls += a.Calls
		dst.Charged.add(a.Charged.Value, 0)
		dst.List.add(a.List.Value, 0)
		dst.Charged.Unknown += a.Charged.Unknown
		dst.List.Unknown += a.List.Unknown
	}
}

func (b *TotalsBuilder) Totals() Totals {
	t := b.totals
	t.Input, t.Uncached, t.CacheRead = t.Input.snapshot(), t.Uncached.snapshot(), t.CacheRead.snapshot()
	t.CacheWrite, t.Output, t.Reasoning = t.CacheWrite.snapshot(), t.Output.snapshot(), t.Reasoning.snapshot()
	for _, key := range slices.SortedFunc(maps.Keys(b.charges), func(a, b chargeKey) int {
		return cmp.Or(cmp.Compare(a.currency, b.currency), cmp.Compare(a.source, b.source))
	}) {
		a := *b.charges[key]
		a.Charged, a.List = a.Charged.snapshot(), a.List.snapshot()
		t.Charges = append(t.Charges, a)
	}
	return t
}

type groupKey struct{ provider, model, role string }
type groupBuilders map[groupKey]*TotalsBuilder

func (b groupBuilders) Add(key groupKey, s Sample) {
	if b[key] == nil {
		b[key] = &TotalsBuilder{}
	}
	b[key].Add(s)
}

func (b groupBuilders) Groups() []Group {
	var groups []Group
	for _, key := range slices.SortedFunc(maps.Keys(b), func(a, b groupKey) int {
		return cmp.Or(cmp.Compare(a.provider, b.provider), cmp.Compare(a.model, b.model), cmp.Compare(a.role, b.role))
	}) {
		groups = append(groups, Group{Provider: key.provider, Model: key.model, Role: key.role, Totals: b[key].Totals()})
	}
	return groups
}

type WindowBuilder struct {
	total                                TotalsBuilder
	providers, models, roles, modelRoles groupBuilders
}

func (b *WindowBuilder) Add(role string, s Sample) {
	if b.providers == nil {
		b.providers, b.models, b.roles, b.modelRoles = make(groupBuilders), make(groupBuilders), make(groupBuilders), make(groupBuilders)
	}
	b.total.Add(s)
	b.providers.Add(groupKey{provider: s.Provider}, s)
	b.models.Add(groupKey{provider: s.Provider, model: s.Model}, s)
	b.roles.Add(groupKey{role: role}, s)
	b.modelRoles.Add(groupKey{s.Provider, s.Model, role}, s)
}

func (b *WindowBuilder) Window() Window {
	return Window{Totals: b.total.Totals(), ByProvider: b.providers.Groups(), ByModel: b.models.Groups(),
		ByRole: b.roles.Groups(), ByModelRole: b.modelRoles.Groups()}
}
