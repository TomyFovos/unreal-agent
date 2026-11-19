package host

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/subagent/identity"
	"github.com/unreallabsai/unreal-agent/internal/jsonspan"
)

// RecordShow reads canonical evidence in this Session's lineage. pageBytes
// is supplied by the caller; zero reads the whole selected range.
func (s *Session) RecordShow(ctx context.Context, locator record.Locator, pageBytes int64) (record.Page, error) {
	if pageBytes < 0 {
		return record.Page{}, fmt.Errorf("record page size must be nonnegative")
	}
	address, err := locator.Address()
	if err != nil {
		return record.Page{}, err
	}
	ctx = s.WithExecutionPolicy(ctx)
	var page record.Page
	found, err := s.walkRecordLogs(ctx, session.ID(address.Session), func(log recordLog) (bool, error) {
		content, err := s.recordContent(ctx, log, address)
		if err != nil {
			return false, err
		}
		defer content.close()
		page, err = content.page(ctx, address, pageBytes)
		return true, err
	})
	if err == nil && !found {
		err = fs.ErrNotExist
	}
	return page, err
}

// RecordArtifact explicitly captures one workspace file version. Its bytes
// are authorized as an ordinary read, even when the Session is in Read Only.
func (s *Session) RecordArtifact(ctx context.Context, path string) (record.Locator, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return "", ErrStopped
	}
	if !filepath.IsAbs(path) && s.workspace != "" {
		// Preserve path components for ordinary permission checks.
		path = s.workspace + string(filepath.Separator) + path
	}
	item, err := s.store.CaptureArtifact(s.WithExecutionPolicy(ctx), s.ID, path)
	if err != nil {
		return "", err
	}
	return item.Data.(sessionstore.HostRecord).Artifact.Locator(s.ID, item.Sequence), nil
}

type recordLog struct {
	*localfile.RecordLog
	handle    operation.ID
	workspace string
}

// Traverse canonical start operations, including starts whose executors
// ended and whose logs moved. An unrelated Session ID is not a capability.
// A target limits the callback to that log and prefers its direct lineage.
func (s *Session) walkRecordLogs(ctx context.Context, target session.ID, visit func(recordLog) (bool, error)) (bool, error) {
	seen := make(map[session.ID]bool)
	var walk func(session.ID, operation.ID) (bool, error)
	walk = func(id session.ID, handle operation.ID) (bool, error) {
		if seen[id] {
			return false, nil
		}
		seen[id] = true
		raw, err := s.store.ReadRecordLog(ctx, id)
		if err != nil {
			if id != s.ID && errors.Is(err, fs.ErrNotExist) {
				return false, nil // a start may never have created an executor
			}
			return false, err
		}
		log := recordLog{RecordLog: raw, handle: handle}
		if target == "" || target == id {
			for _, item := range raw.Items {
				if value, ok := item.Data.(sessionstore.HostRecord); ok && value.Kind == "configuration" {
					var config struct {
						Workspace   string
						OperationID operation.ID
						ChildID     session.ID
					}
					if json.Unmarshal(value.Configuration, &config) == nil {
						log.workspace = config.Workspace
						if log.handle == "" && config.ChildID == id {
							log.handle = config.OperationID
						}
					}
				}
			}
			if id == s.ID && log.workspace == "" {
				log.workspace = s.workspace
			}
			stop, err := visit(log)
			if stop || err != nil {
				return stop, err
			}
		}
		type child struct {
			id     session.ID
			handle operation.ID
		}
		var children []child
		for _, op := range raw.Operations {
			p, ok := childArchivePlan(op)
			if !ok || p.Action != "start" || p.ParentID != id {
				continue
			}
			childID := identity.ChildID(id, op.ID)
			if p.ChildID != "" && childID != p.ChildID {
				continue
			}
			if target == childID {
				return walk(childID, op.ID)
			}
			children = append(children, child{childID, op.ID})
		}
		for _, child := range children {
			if stop, err := walk(child.id, child.handle); stop || err != nil {
				return stop, err
			}
		}
		return false, nil
	}
	return walk(s.ID, "")
}

func (s *Session) recordContent(ctx context.Context, log recordLog, a record.Address) (recordContent, error) {
	switch a.Kind {
	case record.ContextIndex:
		raw, err := log.ContextIndex(a.Section)
		return memoryRecord(raw), err
	case record.OperationCheckpoint:
		raw, err := log.RawOperation(operation.ID(a.Operation), a.Offset)
		return memoryRecord(raw), err
	case record.OperationOutput:
		op, ok := log.Operation(operation.ID(a.Operation))
		if !ok || !terminal(op.Status) {
			return recordContent{}, fs.ErrNotExist
		}
		return s.operationRecordContent(ctx, log.ID, op, a.Stream)
	}

	raw := log.RawItem(sessionstore.Sequence(a.Sequence))
	if raw == nil {
		return recordContent{}, fs.ErrNotExist
	}
	item := log.Items[a.Sequence-1]
	switch a.Kind {
	case record.SessionItem:
		if err := allowRecordItem(ctx, log.workspace, item); err != nil {
			return recordContent{}, err
		}
		return memoryRecord(raw), nil
	case record.UserMessage, record.PeerMessage:
		in, ok := item.Data.(inbox.Input)
		if ok && a.Kind == record.UserMessage && in.Kind == inbox.InputExternal {
			var text string
			if err := json.Unmarshal(in.Payload, &text); err != nil {
				return memoryRecord(in.Payload), nil
			}
			return memoryRecord([]byte(text)), nil
		}
		if ok && a.Kind == record.PeerMessage {
			if peer, err := in.DecodePeerMessage(); err == nil {
				return memoryRecord([]byte(peer.Text)), nil
			}
		}
	case record.ToolCall, record.AssistantMessage:
		if r, ok := item.Data.(sessionstore.ModelResponse); ok && a.Part < len(r.Response.Output) {
			switch v := r.Response.Output[a.Part].Data.(type) {
			case llm.Message:
				if a.Kind == record.AssistantMessage && v.Role == llm.RoleAssistant {
					return memoryRecord([]byte(v.Text)), nil
				}
			case llm.ToolCall:
				if a.Kind == record.ToolCall {
					return modelPart(raw, a.Part)
				}
			}
		}
	case record.ToolError:
		if r, ok := item.Data.(sessionstore.ToolCallStatus); ok && r.Status.Error != "" {
			return originalOutput(r.Status.Error, r.Status.OriginalError, r.Status.ErrorTruncated)
		}
	case record.ChildReport:
		if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Kind == "finish" && a.Handle == string(log.handle) {
			return reportSection(raw, r.Finish.Result, a.Section)
		}
	case record.Artifact:
		if r, ok := item.Data.(sessionstore.HostRecord); ok && r.Kind == sessionstore.HostArtifact && a.Path == r.Artifact.Path && a.Hash == r.Artifact.SHA256 {
			file, err := s.store.OpenArtifact(ctx, *r.Artifact)
			if err != nil {
				return recordContent{}, originalUnavailable(err)
			}
			return fileRecord(file, r.Artifact.Size)
		}
	}
	return recordContent{}, fs.ErrNotExist
}

// Public history already withholds the bound project instructions. The raw
// record may expose them when the caller can read the originating path.
func allowRecordItem(ctx context.Context, workspace string, item sessionstore.Item) error {
	r, ok := item.Data.(sessionstore.HostRecord)
	if !ok {
		return nil
	}
	protected := r.ProjectInstructions != nil && r.ProjectInstructions.SourcePath != ""
	if r.Kind == "configuration" {
		projected := projectConfiguration(r.Configuration)
		protected = !bytes.Equal(r.Configuration, projected)
	}
	if !protected {
		return nil
	}
	if workspace == "" {
		return &permission.Error{Code: permission.Denied, Capability: "filesystem", Reason: "instruction source has no recorded workspace"}
	}
	return permission.FromContext(ctx).CheckPath(filepath.Join(workspace, "AGENTS.md"), false)
}

func modelPart(raw []byte, part int) (recordContent, error) {
	var envelope struct {
		Data struct {
			Item struct {
				Data struct {
					Response struct {
						Output []struct{ Data jsonspan.Span }
					}
				}
			}
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return recordContent{}, err
	}
	return memoryRecord(envelope.Data.Item.Data.Response.Output[part].Data.Value(raw)), nil
}

func reportSection(raw []byte, result sessionstore.FinishResult, name string) (recordContent, error) {
	switch name {
	case "":
		return memoryRecord(raw), nil
	case "summary":
		return memoryRecord([]byte(result.Summary)), nil
	case "next":
		if result.Next == "" {
			return recordContent{}, fs.ErrNotExist
		}
		return memoryRecord([]byte(result.Next)), nil
	case "tests", "changed-files", "blockers":
		var envelope struct {
			Data struct {
				Item struct {
					Data struct {
						Finish struct {
							Result struct{ Tests, ChangedFiles, Blockers jsonspan.Span }
						}
					}
				}
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return recordContent{}, err
		}
		fields := envelope.Data.Item.Data.Finish.Result
		span := map[string]jsonspan.Span{"tests": fields.Tests, "changed-files": fields.ChangedFiles, "blockers": fields.Blockers}[name]
		if data := span.Value(raw); len(data) != 0 {
			return memoryRecord(data), nil
		}
	}
	return recordContent{}, fs.ErrNotExist
}

func originalUnavailable(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %v", record.ErrUnavailable, err)
	}
	return err
}

// RecordFind visits canonical evidence in Session and start-operation order.
// Excerpts are the literal match, textual first line, or a record identity
// token when the source is structured. No product excerpt size is applied.
func (s *Session) RecordFind(ctx context.Context, query record.Query) ([]record.Match, error) {
	ctx = s.WithExecutionPolicy(ctx)
	var matches []record.Match
	_, err := s.walkRecordLogs(ctx, "", func(log recordLog) (bool, error) {
		appendMatch := func(a record.Address, handle string, at time.Time, excerpt []byte, captured []byte) error {
			if err := context.Cause(ctx); err != nil {
				return err
			}
			if !recordMatches(query, a, handle, at) {
				return nil
			}
			content := memoryRecord(captured)
			if captured == nil {
				var err error
				content, err = s.recordContent(ctx, log, a)
				if err != nil {
					if permission.Failure(err) != nil || errors.Is(err, record.ErrUnavailable) || errors.Is(err, fs.ErrNotExist) {
						return nil
					}
					return err
				}
				defer content.close()
			}
			if query.Text != "" {
				text := []byte(query.Text)
				found, err := content.contains(ctx, text)
				if err != nil || !found {
					return err
				}
				excerpt = text
			} else if excerpt == nil && content.size != 0 {
				var err error
				excerpt, err = defaultRecordExcerpt(ctx, a, content)
				if err != nil {
					return err
				}
			}
			matches = append(matches, record.Match{Locator: a.Locator(), Kind: a.Kind, Handle: handle, At: at, Excerpt: slices.Clone(excerpt)})
			return nil
		}
		for _, item := range log.Items {
			if err := findRecordItem(log, query, item, appendMatch); err != nil {
				return false, err
			}
		}
		if query.Kind == "" || query.Kind == record.OperationOutput {
			for i, op := range log.Operations {
				streams := operation.RecordStreams(op)
				if len(streams) == 0 {
					continue
				}
				at := log.OperationTime(i)
				handle := operationRecordHandle(op, log.handle)
				for _, stream := range streams {
					a := record.Address{Session: string(log.ID), Kind: record.OperationOutput, Operation: string(op.ID), Stream: stream}
					if err := appendMatch(a, string(handle), at, nil, nil); err != nil {
						return false, err
					}
				}
			}
		}
		if query.Kind == "" || query.Kind == record.OperationCheckpoint {
			err := log.WalkOperationEvidence(func(e localfile.OperationEvidence) error {
				op, _ := log.Operation(operation.ID(e.Address.Operation))
				return appendMatch(e.Address, string(operationRecordHandle(op, log.handle)), e.At, []byte("operation"), e.Bytes)
			})
			return false, err
		}
		return false, nil
	})
	return matches, err
}

func defaultRecordExcerpt(ctx context.Context, a record.Address, content recordContent) ([]byte, error) {
	if a.Kind == record.OperationOutput && (a.Stream == record.State || a.Stream == record.Result) {
		// Structured results use one JSON token as their default excerpt.
		decoder := jsontext.NewDecoder(bytes.NewReader(content.data))
		if _, err := decoder.ReadToken(); err == nil {
			return content.data[:decoder.InputOffset()], nil
		}
	}
	first, err := content.page(ctx, record.Address{Line: 1}, 0)
	excerpt, _, _ := bytes.Cut(first.Bytes, []byte{'\n'})
	return excerpt, err
}

func recordMatches(q record.Query, a record.Address, handle string, at time.Time) bool {
	return (q.Kind == "" || q.Kind == a.Kind) &&
		(q.Handle == "" || q.Handle == handle || q.Handle == a.Operation) &&
		(q.From.IsZero() && q.Before.IsZero() || !at.IsZero() &&
			(q.From.IsZero() || !at.Before(q.From)) && (q.Before.IsZero() || at.Before(q.Before)))
}

type recordMatchFunc func(record.Address, string, time.Time, []byte, []byte) error

func findRecordItem(log recordLog, query record.Query, item sessionstore.Item, match recordMatchFunc) error {
	a := record.Address{Session: string(log.ID), Sequence: uint64(item.Sequence), Kind: record.SessionItem}
	at, handle := item.RecordedAt, string(log.handle)
	metadata := string(item.Kind)
	var parts []record.Address
	switch v := item.Data.(type) {
	case inbox.Input:
		if v.Kind == inbox.InputExternal {
			a.Kind = record.UserMessage
			parts = append(parts, a)
		} else if peer, err := v.DecodePeerMessage(); err == nil {
			handle = peer.Handle
			a.Kind = record.PeerMessage
			parts = append(parts, a)
		} else if op, err := sessionstore.DecodeOperationIntent(v); err == nil {
			handle = string(operationRecordHandle(op, log.handle))
		}
	case sessionstore.ModelResponse:
		for i, output := range v.Response.Output {
			switch data := output.Data.(type) {
			case llm.Message:
				if data.Role != llm.RoleAssistant {
					continue
				}
				a.Kind = record.AssistantMessage
			case llm.ToolCall:
				a.Kind = record.ToolCall
			default:
				continue
			}
			a.Part = i
			parts = append(parts, a)
		}
	case sessionstore.ToolCallStatus:
		for _, op := range v.Operations {
			handle = string(operationRecordHandle(op, operation.ID(handle)))
			if query.Handle == handle {
				break
			}
		}
		if v.Status.Error != "" && (!v.Status.ErrorTruncated || len(v.Status.OriginalError) > 0) {
			a.Kind = record.ToolError
			parts = append(parts, a)
		}
	case sessionstore.HostRecord:
		metadata = v.Kind
		if v.ChildReview != nil {
			handle = v.ChildReview.Handle
		}
		if v.Artifact != nil {
			a.Kind, a.Path, a.Hash = record.Artifact, v.Artifact.Path, v.Artifact.SHA256
			parts = append(parts, a)
		}
		if v.Finish != nil && handle != "" {
			a.Kind, a.Handle = record.ChildReport, handle
			summary, _, _ := bytes.Cut([]byte(v.Finish.Result.Summary), []byte{'\n'})
			if query.Kind != record.SessionItem {
				if query.Text != "" {
					if bytes.Contains([]byte(v.Finish.Result.Summary), []byte(query.Text)) {
						a.Section = "summary"
					} else if bytes.Contains([]byte(v.Finish.Result.Next), []byte(query.Text)) {
						a.Section = "next"
					}
				}
				return match(a, handle, at, summary, nil)
			}
		}
	}
	if len(parts) == 0 || query.Kind == record.SessionItem {
		a.Kind = record.SessionItem
		return match(a, handle, at, []byte(metadata), nil)
	}
	for _, part := range parts {
		var excerpt []byte
		if part.Kind == record.ToolCall {
			call := item.Data.(sessionstore.ModelResponse).Response.Output[part.Part].Data.(llm.ToolCall)
			if query.Text != "" && bytes.Contains([]byte(call.Arguments), []byte(query.Text)) {
				// Arguments are an exact string inside the raw JSON call.
				if err := match(part, handle, at, nil, []byte(call.Arguments)); err != nil {
					return err
				}
				continue
			}
			excerpt = []byte(call.Name)
		}
		if err := match(part, handle, at, excerpt, nil); err != nil {
			return err
		}
	}
	return nil
}
