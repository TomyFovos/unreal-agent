package tui

import (
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"strings"
)

// Picker is UI-local, using the slash menu's selection window and styles.
type Picker struct {
	Kind, Title, Hint string
	Options           []string
	Selection, Offset int
	Catalog           modelcatalog.Catalog
	Model             modelcatalog.Model
	Revision          uint64
	Provider          string
	Providers         []modelcatalog.Provider
	Responses         []ResponseChoice
	Views             []ViewMode
	ToolBridge        bool
}

func responseExportPicker(choices []ResponseChoice) *Picker {
	p := &Picker{Kind: "response-export", Title: "Export response", Responses: choices}
	for _, choice := range choices {
		label := fmt.Sprintf("Turn %d", choice.TurnNumber)
		if choice.TurnNumber == 0 {
			label = fmt.Sprintf("Response %d (turn unknown)", choice.Sequence)
		}
		p.Options = append(p.Options, label+"  "+choice.Preview)
	}
	if len(choices) == 0 {
		p.Hint = "no public agent responses to export"
	}
	return p
}

func providerPicker(providers []modelcatalog.Provider, current *sessionstore.RuntimeSelection) *Picker {
	p := &Picker{Kind: "provider", Title: "Provider", Providers: providers}
	if current != nil {
		p.Revision = current.Revision
	}
	for i, b := range providers {
		p.Options = append(p.Options, SafeText(b.Name)+" · "+SafeText(b.Availability))
		if current != nil && b.ID == current.Provider {
			p.Selection = i
		}
	}
	return p
}

func analyzePicker(view string) *Picker {
	p := &Picker{Kind: "analyze", Title: "Analyze", Options: append([]string(nil), analysis.Views...)}
	for i, v := range p.Options {
		if strings.EqualFold(v, view) {
			p.Selection = i
		}
	}
	return p
}

func modelPicker(c modelcatalog.Catalog, current *sessionstore.RuntimeSelection, tools ...bool) *Picker {
	p := &Picker{Kind: "model", Title: "Model", Catalog: c.Clone()}
	p.ToolBridge = len(tools) > 0 && tools[0]
	if !c.Available {
		p.Hint = "catalog unavailable; current selection only"
	}
	if current != nil {
		p.Provider = current.Provider
		if current.Provider == "claude-code" {
			p.Hint = claudeModelHint(c, p.ToolBridge || len(tools) == 0 && sessionstore.ToolBridgeEnabledFromConfiguration([]byte(current.Binding)))
		}
		found := false
		for _, m := range p.Catalog.Models {
			if m.Matches(current.Model) {
				found = true
			}
		}
		if !found && c.Authoritative {
			p.Hint = "Claude Code · discovered catalog; current model unavailable"
		} else if !found {
			if c.Available {
				p.Hint = "catalog unavailable for current model; current effort only"
			}
			p.Catalog.Models = append(p.Catalog.Models, modelcatalog.Model{ID: current.Model, Name: current.Name, Efforts: []llm.ReasoningEffort{current.Effort}, DefaultEffort: current.Effort})
		}
		p.Revision = current.Revision
	}
	selected := false
	for i, m := range p.Catalog.Models {
		p.Options = append(p.Options, SafeText(m.Name))
		if current != nil && (m.ID == current.Model || !selected && m.Matches(current.Model)) {
			p.Selection = i
			selected = true
		}
	}
	if len(p.Options) == 0 {
		p.Hint = "catalog unavailable; no observed current selection"
		if c.Authoritative {
			p.Hint = "Claude Code · discovered catalog; no selectable models"
		}
	}
	return p
}

func claudeModelHint(c modelcatalog.Catalog, tools bool) string {
	hint := "Claude Code · text-only; explicit configured catalog"
	if c.Problem != "" && c.Available {
		hint = "Claude Code · text-only; discovery unavailable; explicit configured catalog"
	}
	if c.Authoritative {
		hint = "Claude Code · text-only; discovered catalog"
	}
	if !c.Available {
		hint = "Claude Code · text-only; catalog unavailable; current selection only"
	}
	if tools {
		hint = strings.Replace(hint, "text-only", "Unreal tool bridge", 1)
	}
	return hint
}

func (p *Picker) capability(c modelcatalog.Capabilities) {
	if p.Provider == "claude-code" && c.ToolBridge != "" {
		if p.Hint == "" {
			p.Hint = claudeModelHint(p.Catalog, c.Tools)
		}
		p.Hint += "; Tool Bridge: " + SafeText(c.ToolBridge)
	}
}
func (p *Picker) effort(current *sessionstore.RuntimeSelection) *Picker {
	m := p.Catalog.Models[p.Selection]
	n := &Picker{Kind: "effort", Title: SafeText(m.Name) + " · Reasoning effort", Model: m, Catalog: p.Catalog, Revision: p.Revision, Hint: p.Hint, Provider: p.Provider, Providers: p.Providers, ToolBridge: p.ToolBridge}
	if m.SupportsEffort != nil && !*m.SupportsEffort {
		n.Options = []string{"No effort parameter (not supported by this model)"}
		return n
	}
	if m.UnsupportedEfforts {
		n.Hint = "catalog efforts unsupported by this harness are omitted"
	}
	for i, e := range m.Efforts {
		n.Options = append(n.Options, string(e))
		if e == m.DefaultEffort {
			n.Selection = i
		}
	}
	if current != nil && current.Provider == p.Provider && m.Matches(current.Model) {
		for i, e := range m.Efforts {
			if e == current.Effort {
				n.Selection = i
			}
		}
	}
	if len(n.Options) == 0 {
		n.Hint = "catalog has no effort levels supported by this harness"
	}
	return n
}

func (p *Picker) selectedEffort() llm.ReasoningEffort {
	if p.Model.SupportsEffort != nil && !*p.Model.SupportsEffort {
		return ""
	}
	return p.Model.Efforts[p.Selection]
}
func (p *Picker) move(delta int) {
	p.Selection = min(max(0, p.Selection+delta), max(0, len(p.Options)-1))
}
func pickerRows(p *Picker, u UIState, l layout, budget int) ([]line, int) {
	if p == nil || budget <= 0 {
		return nil, 0
	}
	title := p.Title
	if u.Theme.ASCII {
		title = strings.ReplaceAll(title, " · ", " / ")
	}
	rows := []line{l.gutter("", "", textLine(title, strong).clip(l.text))}
	if p.Hint != "" && budget >= 3 {
		rows = append(rows, l.gutter("", "", textLine(p.Hint, warningStyle).clip(l.text)))
	}
	limit := max(0, budget-len(rows))
	start, end := menuWindow(len(p.Options), p.Selection, p.Offset, limit)
	if start > 0 && limit > 1 {
		rows = append(rows, l.gutter("", "", textLine(u.Theme.symbol("↑", "^")+" more", meta)))
	}
	for i := start; i < end; i++ {
		mark, st := "", normal
		if i == p.Selection {
			mark = u.Theme.symbol("›", "*")
			st = selectionStyle
		}
		rows = append(rows, l.gutter("", mark, textLine(p.Options[i], st).clip(l.text)))
	}
	if end < len(p.Options) && len(rows) < budget {
		rows = append(rows, l.gutter("", "", textLine(u.Theme.symbol("↓", "v")+" more", meta)))
	}
	return rows, start
}
