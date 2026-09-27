package tool

import (
	"encoding/json/v2"
	"fmt"
)

// Extension installs an immutable definition and result codec. A disabled
// extension remains resolvable for history, never for new model calls.
type Extension struct {
	Definition Definition
	Translator Translator
	Enabled    bool
}
type extendedRegistry struct {
	Registry
	extensions []Extension
}

func WithExtensions(base Registry, extensions []Extension) (Registry, error) {
	if base == nil {
		return nil, fmt.Errorf("base registry is required")
	}
	result := &extendedRegistry{Registry: base}
	names := map[string]bool{}
	for _, e := range extensions {
		name := e.Definition.Tool.Name
		_, exists := ResolveHistory(base, name)
		for _, builtin := range staticDefinitions() {
			if builtin.Tool.Name == name {
				exists = true
			}
		}
		if name == "" || names[name] || exists || e.Translator == nil {
			return nil, fmt.Errorf("invalid or duplicate tool extension %q", name)
		}
		names[name] = true
		// Detach nested schema/metadata bytes from caller-owned configuration.
		data, err := json.Marshal(e.Definition)
		if err != nil {
			return nil, err
		}
		e.Definition = Definition{}
		if err = json.Unmarshal(data, &e.Definition); err != nil {
			return nil, err
		}
		result.extensions = append(result.extensions, e)
	}
	return result, nil
}
func (r *extendedRegistry) StaticDefinitions() []Definition {
	out := append([]Definition(nil), r.Registry.StaticDefinitions()...)
	for _, e := range r.extensions {
		if e.Enabled {
			out = append(out, e.Definition)
		}
	}
	data, _ := json.Marshal(out)
	var result []Definition
	_ = json.Unmarshal(data, &result)
	return result
}
func (r *extendedRegistry) Resolve(name string) (Translator, bool) {
	for _, e := range r.extensions {
		if e.Definition.Tool.Name == name {
			if !e.Enabled {
				return nil, false
			}
			return e.Translator, true
		}
	}
	return r.Registry.Resolve(name)
}
func (r *extendedRegistry) ResolveHistory(name string) (Translator, bool) {
	for _, e := range r.extensions {
		if e.Definition.Tool.Name == name {
			return e.Translator, true
		}
	}
	return ResolveHistory(r.Registry, name)
}
