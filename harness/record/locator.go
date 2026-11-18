// Package record defines references to canonical Session evidence. A locator
// identifies data and carries no authority to read it.
package record

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type Locator string
type Kind string

const (
	SessionItem         Kind = "item"
	UserMessage         Kind = "user"
	PeerMessage         Kind = "peer"
	AssistantMessage    Kind = "message"
	ToolCall            Kind = "call"
	ChildReport         Kind = "finish"
	OperationOutput     Kind = "output"
	OperationCheckpoint Kind = "operation"
	Artifact            Kind = "artifact"
)

const (
	Stdout = "stdout"
	Stderr = "stderr"
	Result = "result"
	Error  = "error"
	State  = "state"
)

// ByteRange uses zero-based, half-open byte offsets. A line selector uses a
// one-based line number and includes the original line ending.
type ByteRange struct{ Start, End int64 }

type Address struct {
	Session   string
	Sequence  uint64
	Kind      Kind
	Part      int
	Handle    string
	Section   string
	Operation string
	Offset    int64
	Stream    string
	Path      string
	Hash      string
	Range     *ByteRange
	Line      int64
}

func Item(session string, sequence uint64) Locator {
	return Address{Session: session, Sequence: sequence, Kind: SessionItem}.Locator()
}

func Input(session string, sequence uint64, kind Kind) Locator {
	return Address{Session: session, Sequence: sequence, Kind: kind}.Locator()
}

func Output(session, operation, stream string) Locator {
	return Address{Session: session, Kind: OperationOutput, Operation: operation, Stream: stream}.Locator()
}

func (a Address) Locator() Locator {
	parts := []string{a.Session}
	switch a.Kind {
	case OperationOutput:
		parts = append(parts, string(a.Kind), a.Operation, a.Stream)
	case OperationCheckpoint:
		parts = append(parts, string(a.Kind), a.Operation, strconv.FormatInt(a.Offset, 10))
	default:
		parts = append(parts, strconv.FormatUint(a.Sequence, 10))
		switch a.Kind {
		case UserMessage, PeerMessage:
			parts = append(parts, string(a.Kind))
		case AssistantMessage, ToolCall:
			parts = append(parts, string(a.Kind), strconv.Itoa(a.Part))
		case ChildReport:
			parts = append(parts, string(a.Kind), a.Handle)
			if a.Section != "" {
				parts = append(parts, a.Section)
			}
		case Artifact:
			parts = append(parts, string(a.Kind), a.Path, a.Hash)
		}
	}
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	locator := "rec:" + strings.Join(parts, "/")
	if a.Range != nil {
		locator += fmt.Sprintf("?bytes=%d-%d", a.Range.Start, a.Range.End)
	} else if a.Line != 0 {
		locator += fmt.Sprintf("?line=%d", a.Line)
	}
	return Locator(locator)
}

func (l Locator) Address() (Address, error) {
	invalid := fmt.Errorf("invalid record locator %q", l)
	path, ok := strings.CutPrefix(string(l), "rec:")
	if !ok {
		return Address{}, invalid
	}
	path, selector, selected := strings.Cut(path, "?")
	parts := strings.Split(path, "/")
	for i, part := range parts {
		value, err := url.PathUnescape(part)
		if err != nil || value == "" {
			return Address{}, invalid
		}
		parts[i] = value
	}
	if len(parts) < 2 {
		return Address{}, invalid
	}
	a := Address{Session: parts[0], Kind: SessionItem}
	switch Kind(parts[1]) {
	case OperationOutput:
		if len(parts) != 4 {
			return Address{}, invalid
		}
		a.Kind, a.Operation, a.Stream = OperationOutput, parts[2], parts[3]
	case OperationCheckpoint:
		if len(parts) != 4 {
			return Address{}, invalid
		}
		offset, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil || offset <= 0 {
			return Address{}, invalid
		}
		a.Kind, a.Operation, a.Offset = OperationCheckpoint, parts[2], offset
	default:
		var err error
		a.Sequence, err = strconv.ParseUint(parts[1], 10, 64)
		if err != nil || a.Sequence == 0 {
			return Address{}, invalid
		}
		tail := parts[2:]
		if len(tail) != 0 {
			a.Kind = Kind(tail[0])
			switch a.Kind {
			case UserMessage, PeerMessage:
				if len(tail) != 1 {
					return Address{}, invalid
				}
			case AssistantMessage, ToolCall:
				if len(tail) != 2 {
					return Address{}, invalid
				}
				a.Part, err = strconv.Atoi(tail[1])
				if err != nil || a.Part < 0 {
					return Address{}, invalid
				}
			case ChildReport:
				if len(tail) != 2 && len(tail) != 3 {
					return Address{}, invalid
				}
				a.Handle = tail[1]
				if len(tail) == 3 {
					a.Section = tail[2]
				}
			case Artifact:
				if len(tail) != 3 {
					return Address{}, invalid
				}
				a.Path, a.Hash = tail[1], tail[2]
			default:
				return Address{}, invalid
			}
		}
	}
	if selected {
		key, value, ok := strings.Cut(selector, "=")
		if !ok {
			return Address{}, invalid
		}
		switch key {
		case "bytes":
			start, end, ok := strings.Cut(value, "-")
			lo, e1 := strconv.ParseInt(start, 10, 64)
			hi, e2 := strconv.ParseInt(end, 10, 64)
			if !ok || e1 != nil || e2 != nil || lo < 0 || hi < lo {
				return Address{}, invalid
			}
			a.Range = &ByteRange{Start: lo, End: hi}
		case "line":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n <= 0 {
				return Address{}, invalid
			}
			a.Line = n
		default:
			return Address{}, invalid
		}
	}
	if a.Locator() != l {
		return Address{}, invalid
	}
	return a, nil
}
