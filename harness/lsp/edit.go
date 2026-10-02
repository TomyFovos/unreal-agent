package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
)

func positionOffset(data []byte, position Position, encoding string) (int, error) {
	if position.Line < 0 || position.Character < 0 || !utf8.Valid(data) {
		return 0, failure("invalid", "invalid text position")
	}
	start := 0
	for range position.Line {
		next := bytes.IndexByte(data[start:], '\n')
		if next < 0 {
			return 0, failure("invalid", "position line is out of bounds")
		}
		start += next + 1
	}
	end := bytes.IndexByte(data[start:], '\n')
	if end < 0 {
		end = len(data) - start
	}
	line := data[start : start+end]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	units := 0
	for offset := 0; offset < len(line); {
		if units == position.Character {
			return start + offset, nil
		}
		value, size := utf8.DecodeRune(line[offset:])
		increment := 1
		switch encoding {
		case "utf-8":
			increment = size
		case "utf-16":
			if value > 0xffff {
				increment = 2
			}
		case "utf-32":
		default:
			return 0, failure("unsupported", "position encoding is unsupported")
		}
		units += increment
		offset += size
		if units > position.Character {
			return 0, failure("invalid", "position splits a Unicode code point")
		}
	}
	if units == position.Character {
		return start + len(line), nil
	}
	return 0, failure("invalid", "position character is out of bounds")
}
func offsetPosition(data []byte, offset int, encoding string) Position {
	result := Position{}
	for at := 0; at < offset; {
		value, size := utf8.DecodeRune(data[at:])
		at += size
		if value == '\n' {
			result.Line++
			result.Character = 0
			continue
		}
		units := 1
		if encoding == "utf-8" {
			units = size
		} else if encoding == "utf-16" && value > 0xffff {
			units = 2
		}
		result.Character += units
	}
	return result
}

type indexedEdit struct {
	start, end, index int
	text              string
}

func replaceText(data []byte, edits []textEdit, encoding string) ([]byte, error) {
	if !utf8.Valid(data) {
		return nil, failure("unsupported", "workspace edit target is not UTF-8")
	}
	indexed := make([]indexedEdit, 0, len(edits))
	for i, edit := range edits {
		if edit.AnnotationID != "" {
			return nil, failure("unsupported", "annotated edits require an external decision")
		}
		if !utf8.ValidString(edit.NewText) {
			return nil, failure("invalid", "replacement is not UTF-8")
		}
		start, err := positionOffset(data, edit.Range.Start, encoding)
		if err != nil {
			return nil, err
		}
		end, err := positionOffset(data, edit.Range.End, encoding)
		if err != nil {
			return nil, err
		}
		if end < start {
			return nil, failure("invalid", "edit range is reversed")
		}
		indexed = append(indexed, indexedEdit{start, end, i, edit.NewText})
	}
	sort.SliceStable(indexed, func(i, j int) bool {
		if indexed[i].start != indexed[j].start {
			return indexed[i].start < indexed[j].start
		}
		return indexed[i].end < indexed[j].end
	})
	for i := 1; i < len(indexed); i++ {
		previous, current := indexed[i-1], indexed[i]
		if current.start < previous.end || current.start == previous.start && (current.end > current.start || previous.end > previous.start) {
			return nil, failure("invalid", "workspace edits overlap")
		}
	}
	result := append([]byte(nil), data...)
	for i := len(indexed) - 1; i >= 0; i-- {
		edit := indexed[i]
		next := make([]byte, 0, len(result)-(edit.end-edit.start)+len(edit.text))
		next = append(next, result[:edit.start]...)
		next = append(next, edit.text...)
		next = append(next, result[edit.end:]...)
		result = next
		if len(result) > mutation.MaxFileBytes {
			return nil, failure("limit", "replacement exceeds mutation byte limit")
		}
	}
	return result, nil
}

func (m *Manager) prepare(edit workspaceEdit, snapshots map[string]mutation.Snapshot, versions map[string]int, encoding string) (mutation.Request, error) {
	request := mutation.Request{Version: mutation.Version}
	if len(edit.ChangeAnnotations) > 0 {
		return request, failure("unsupported", "annotated workspace edits are unsupported")
	}
	if len(edit.Changes) > 0 && len(edit.DocumentChanges) > 0 {
		return request, failure("invalid", "workspace edit mixes changes and documentChanges")
	}
	edits := make(map[string][]textEdit)
	created := make(map[string]bool)
	add := func(uri string, changes []textEdit, version *int) error {
		path, err := m.path(uri)
		if err != nil {
			return err
		}
		if _, exists := edits[path]; exists {
			return failure("unsupported", "multiple document changes for the same target are unsupported")
		}
		snapshot, exists := snapshots[path]
		if !exists && !created[path] {
			return failure("stale", "workspace edit target was not observed before request")
		}
		if version != nil && (*version != versions[path] || !snapshot.Revision.Exists) {
			return failure("stale", "workspace edit document version is stale")
		}
		edits[path] = changes
		return nil
	}
	for uri, changes := range edit.Changes {
		if err := add(uri, changes, nil); err != nil {
			return request, err
		}
	}
	for _, change := range edit.DocumentChanges {
		if change.AnnotationID != "" {
			return request, failure("unsupported", "annotated resource operations are unsupported")
		}
		if change.Kind != "" {
			if change.Kind != "create" {
				return request, failure("unsupported", "resource rename/delete is not supported by mutation version 1")
			}
			path, err := m.path(change.URI)
			if err != nil {
				return request, err
			}
			if snapshot, exists := snapshots[path]; exists && snapshot.Revision.Exists {
				return request, failure("stale", "CreateFile target already existed in observed workspace")
			}
			if created[path] {
				return request, failure("invalid", "duplicate CreateFile target")
			}
			var options struct {
				Overwrite      bool `json:"overwrite"`
				IgnoreIfExists bool `json:"ignoreIfExists"`
			}
			if len(change.Options) > 0 && json.Unmarshal(change.Options, &options) != nil {
				return request, failure("invalid", "invalid CreateFile options")
			}
			if options.Overwrite || options.IgnoreIfExists {
				return request, failure("unsupported", "CreateFile requires an absent target")
			}
			created[path] = true
			continue
		}
		if change.TextDocument == nil {
			return request, failure("invalid", "document edit has no textDocument")
		}
		if err := add(change.TextDocument.URI, change.Edits, change.TextDocument.Version); err != nil {
			return request, err
		}
	}
	for path := range created {
		if _, ok := edits[path]; !ok {
			edits[path] = nil
		}
	}
	paths := make([]string, 0, len(edits))
	for path := range edits {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		snapshot := snapshots[path]
		data, err := replaceText(snapshot.Data, edits[path], encoding)
		if err != nil {
			return request, err
		}
		request.Changes = append(request.Changes, mutation.Change{Path: path, Expected: snapshot.Revision, Content: data})
	}
	if len(request.Changes) > 0 {
		if err := request.Validate(); err != nil {
			return request, failure("limit", "workspace edit exceeds mutation limits")
		}
	}
	return request, nil
}

func (m *Manager) apply(ctx context.Context, id string, server *instance, edit workspaceEdit, snapshots map[string]mutation.Snapshot, versions map[string]int, result Result) Result {
	if id == "" {
		return resultError(result, failure("invalid", "mutation requires a durable operation identity"))
	}
	request, err := m.prepare(edit, snapshots, versions, server.encoding)
	if err != nil {
		return resultError(result, err)
	}
	if len(request.Changes) == 0 {
		result.Code = "ok"
		return result
	}
	applied := m.mutation.Execute(ctx, id, request)
	result.Mutation = &applied
	result.Code = string(applied.Code)
	if applied.Code == mutation.Applied {
		for _, target := range request.Changes {
			snapshot, err := m.mutation.Snapshot(ctx, target.Path)
			if err != nil || m.synchronize(ctx, server, snapshot) != nil {
				// The edit already committed. Do not report it as uncommitted or retry it.
				result.Message = "edit applied; server synchronization must be refreshed"
				break
			}
		}
	}
	return result
}
