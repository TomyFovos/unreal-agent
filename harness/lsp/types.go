package lsp

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

const MaxResultBytes = 256 << 10
const MaxDocuments = 2048
const MaxWorkspaceBytes = 16 << 20

type ServerConfig struct {
	Language    string
	Path        string
	Arguments   []string
	Environment []string
	Extensions  []string
}
type Config struct {
	Mutation       *mutation.Service
	Servers        []ServerConfig
	RequestTimeout time.Duration
}
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}
type Request struct {
	Action     string   `json:"action"`
	Language   string   `json:"language"`
	Path       string   `json:"path,omitempty"`
	Position   Position `json:"position,omitempty"`
	Range      *Range   `json:"range,omitempty"`
	Encoding   string   `json:"encoding,omitempty"`
	Query      string   `json:"query,omitempty"`
	NewName    string   `json:"new_name,omitempty"`
	Generation string   `json:"generation,omitempty"`
	ActionID   string   `json:"action_id,omitempty"`
}

func (r Request) Validate() error {
	if r.Language == "" || len(r.Language) > 128 {
		return errors.New("LSP language is required")
	}
	switch r.Action {
	case "definition", "references", "diagnostics", "document_symbols", "hover", "rename", "code_actions":
		if r.Path == "" {
			return errors.New("LSP path is required")
		}
	case "workspace_symbols", "restart", "shutdown":
	case "apply_code_action":
		if r.ActionID == "" {
			return errors.New("action_id is required")
		}
	default:
		return errors.New("unsupported LSP action")
	}
	if r.Position.Line < 0 || r.Position.Character < 0 {
		return errors.New("position must not be negative")
	}
	if r.Encoding != "" && r.Encoding != "utf-8" && r.Encoding != "utf-16" && r.Encoding != "utf-32" {
		return errors.New("unsupported position encoding")
	}
	if r.Action == "rename" && (r.NewName == "" || strings.ContainsAny(r.NewName, "\r\n\x00")) {
		return errors.New("new_name is required and must be single-line")
	}
	if len(r.Path) > 4096 || len(r.Query) > 4096 || len(r.NewName) > 4096 || len(r.ActionID) > 128 || len(r.Generation) > 128 {
		return errors.New("LSP argument exceeds limit")
	}
	return nil
}

type Result struct {
	Version    int               `json:"version"`
	Code       string            `json:"code"`
	Action     string            `json:"action,omitempty"`
	Message    string            `json:"message,omitempty"`
	Generation string            `json:"generation,omitempty"`
	Encoding   string            `json:"encoding,omitempty"`
	Data       json.RawMessage   `json:"data,omitempty"`
	Mutation   *mutation.Result  `json:"mutation,omitempty"`
	Denial     *permission.Error `json:"denial,omitempty"`
}
type textEdit struct {
	AnnotationID string `json:"annotationId,omitempty"`
	Range        Range  `json:"range"`
	NewText      string `json:"newText"`
}
type documentChange struct {
	Kind         string          `json:"kind,omitempty"`
	URI          string          `json:"uri,omitempty"`
	OldURI       string          `json:"oldUri,omitempty"`
	NewURI       string          `json:"newUri,omitempty"`
	Options      json.RawMessage `json:"options,omitempty"`
	AnnotationID string          `json:"annotationId,omitempty"`
	TextDocument *struct {
		URI     string `json:"uri"`
		Version *int   `json:"version"`
	} `json:"textDocument,omitempty"`
	Edits []textEdit `json:"edits,omitempty"`
}
type workspaceEdit struct {
	Changes           map[string][]textEdit      `json:"changes,omitempty"`
	DocumentChanges   []documentChange           `json:"documentChanges,omitempty"`
	ChangeAnnotations map[string]json.RawMessage `json:"changeAnnotations,omitempty"`
}
type action struct {
	Title    string          `json:"title"`
	Kind     string          `json:"kind,omitempty"`
	Edit     *workspaceEdit  `json:"edit,omitempty"`
	Command  json.RawMessage `json:"command,omitempty"`
	Disabled json.RawMessage `json:"disabled,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}
type ActionSummary struct {
	ID          string `json:"id,omitempty"`
	Title       string `json:"title"`
	Kind        string `json:"kind,omitempty"`
	Unsupported bool   `json:"unsupported,omitempty"`
	Reason      string `json:"reason,omitempty"`
}
