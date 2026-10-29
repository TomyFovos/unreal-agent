package contextbuilder

import (
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Only small, public receipt metadata is retained here, never raw Operation state.
type receiptInfo struct {
	operations        []operation.ID
	captures, changed []string
	failed            bool
}

func toolReceiptInfo(status sessionstore.ToolCallStatus) receiptInfo {
	info := receiptInfo{failed: status.Status.Error != "" || status.Status.Denial != nil}
	info.operations = slices.Clone(status.Status.WaitingFor)
	for _, op := range status.Operations {
		info.operations = append(info.operations, op.ID)
		info.failed = info.failed || op.Status == operation.StatusFailed || op.Status == operation.StatusCanceled || op.Denial != nil
		if op.Type == operation.TypeShell {
			if state, err := operation.DecodeShellState(op); err == nil {
				for _, path := range []string{state.OutPath, state.ErrPath} {
					if path != "" && !contextengine.Sensitive(path) {
						info.captures = append(info.captures, path)
					}
				}
				info.failed = info.failed || state.Result != nil && state.Result.ExitCode != 0
			}
		}
		if op.Type != operation.TypeRemoteJob {
			continue
		}
		state, err := operation.DecodeRemoteJobState(op)
		if err != nil {
			continue
		}
		// Native edits and structural edits already record the files actually
		// changed, including successful targets of a partially failed mutation.
		var result struct {
			Code    string `json:"code"`
			Path    string `json:"path"`
			Targets []struct {
				Path string `json:"path"`
				Code string `json:"code"`
			} `json:"targets"`
			Mutation *struct {
				Targets []struct {
					Path string `json:"path"`
					Code string `json:"code"`
				} `json:"targets"`
			} `json:"mutation"`
		}
		if state.Plan.Type != "native_file" && state.Plan.Type != "structural_code" && state.Plan.Type != "file_mutation" {
			continue
		}
		if json.Unmarshal(state.Handle, &result) != nil {
			continue
		}
		if result.Code == "applied" && result.Path != "" {
			info.changed = append(info.changed, result.Path)
		}
		if result.Mutation != nil {
			result.Targets = append(result.Targets, result.Mutation.Targets...)
		}
		for _, target := range result.Targets {
			if target.Code == "applied" {
				info.changed = append(info.changed, target.Path)
			}
		}
	}
	return info
}

func (b *builder) toolCalls() map[string]llm.ToolCall {
	calls := map[string]llm.ToolCall{}
	for _, u := range b.engine.Units() {
		if call, ok := u.Item.Data.(llm.ToolCall); ok {
			calls[call.CallID] = call
		}
	}
	return calls
}

// Match the existing conservative estimator to the most recent measured
// request when available. The original request size is captured at its Turn,
// before its response and tool results are added, also during canonical replay.
func (b *builder) toolTokens(bytes int64) int64 {
	if b.pruneInput > 0 && b.pruneEstimate > 0 {
		return int64(float64(bytes) * float64(b.pruneInput) / float64(b.pruneEstimate))
	}
	return bytes
}

func (b *builder) activeToolOperations() map[operation.ID]bool {
	active := map[operation.ID]bool{}
	for _, op := range b.engine.State().Operations {
		active[operation.ID(op.ID)] = true
	}
	return active
}

func (b *builder) canPruneTool(u contextengine.Unit, required map[string]bool, active map[operation.ID]bool) bool {
	if u.ID <= b.viewThrough || u.Class == contextengine.Omit || u.Kind != contextengine.ToolResult || required[u.Group] || b.pruned[u.ID] {
		return false
	}
	for _, id := range b.receiptInfo[u.ID].operations {
		if active[id] {
			return false
		}
	}
	return true
}

func (b *builder) currentToolResult(u contextengine.Unit) (llm.ToolResult, bool) {
	r, ok := u.Item.Data.(llm.ToolResult)
	if items, overridden := b.overrides[u.ID]; overridden {
		for _, item := range items {
			if replacement, found := item.Data.(llm.ToolResult); found {
				return replacement, true
			}
		}
	}
	return r, ok
}

func (b *builder) pruneToolEpoch(persist func(sessionstore.ContextRecord) error) error {
	required, err := b.requiredContinuations()
	if err != nil {
		return err
	}
	active, calls := b.activeToolOperations(), b.toolCalls()
	units := b.engine.Units()
	seenFiles := map[string]bool{}
	var recentBytes, removableBytes int64
	record := sessionstore.ContextRecord{Kind: "prune"}
	for i := len(units) - 1; i >= 0; i-- {
		u := units[i]
		if u.ID <= b.viewThrough || u.Class == contextengine.Omit || u.Kind != contextengine.ToolResult {
			continue
		}
		r, ok := b.currentToolResult(u)
		if !ok {
			continue
		}
		call, info := calls[r.CallID], b.receiptInfo[u.ID]
		read, writes := toolFileAccess(call)
		obsolete := read != "" && seenFiles[read]
		finished := true
		if group := b.continuations[u.Group]; group != nil {
			if dependency := group.calls[r.CallID]; dependency != nil {
				finished = dependency.terminal
			}
		}
		// Only a finished read/edit can supersede an older snapshot. Actual
		// mutation targets remain useful even when another target failed.
		// Its own unacknowledged receipt stays protected; an older, already
		// acknowledged read may be omitted before sending this newer snapshot.
		if finished && !info.failed && r.State != llm.ToolResultFailed && r.State != llm.ToolResultPending {
			if read != "" {
				seenFiles[read] = true
			}
			for _, path := range writes {
				seenFiles[path] = true
			}
		}
		for _, path := range info.changed {
			seenFiles[filepath.Clean(path)] = true
		}
		old := b.toolTokens(recentBytes) >= b.contextConfig.ToolResultKeepTokens
		recentBytes += contextengine.Estimate(r)
		if b.pruned[u.ID] {
			continue
		}
		if (!old && !obsolete) || !b.canPruneTool(u, required, active) {
			continue
		}
		before := contextengine.Estimate(r)
		r.Output = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: b.omittedOutput(u, r, call)}}
		saved := before - contextengine.Estimate(r)
		if saved <= 0 {
			continue
		}
		removableBytes += saved
		record.Projections = append(record.Projections, sessionstore.ContextProjection{UnitID: u.ID, Items: []llm.Item{{Type: llm.ItemToolResult, Data: r}}})
	}
	record.PrunedTokens = b.toolTokens(removableBytes)
	if len(record.Projections) == 0 || record.PrunedTokens < b.contextConfig.ToolResultMinPruneTokens {
		return nil
	}
	// Persist in recording order; the single record is the entire cache boundary.
	slices.Reverse(record.Projections)
	return persist(record)
}

func (b *builder) omittedOutput(u contextengine.Unit, r llm.ToolResult, call llm.ToolCall) string {
	label := strings.Join(strings.Fields(call.Name), " ")
	var args struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
	_ = json.Unmarshal([]byte(call.Arguments), &args)
	if args.Command != "" {
		label += " " + fmt.Sprintf("%q", args.Command)
	} else if args.Path != "" {
		label += " " + fmt.Sprintf("%q", args.Path)
	}
	if label == "" {
		label = "tool result"
	}
	lines := 0
	for _, part := range r.Output {
		if part.Kind == llm.ToolResultText && part.Value != "" {
			lines += strings.Count(part.Value, "\n")
			if !strings.HasSuffix(part.Value, "\n") {
				lines++
			}
		}
	}
	files := ""
	if paths := b.receiptInfo[u.ID].captures; len(paths) > 0 {
		quoted := make([]string, len(paths))
		for i, path := range paths {
			quoted[i] = fmt.Sprintf("%q", path)
		}
		files = " Full output: " + strings.Join(quoted, ", ") + "."
	}
	return fmt.Sprintf("[Output omitted to keep the context small: %s (%d lines). Run it again if you need it.%s Canonical Unit ID %s.]", label, lines, files, u.ID)
}

func toolFileAccess(call llm.ToolCall) (read string, writes []string) {
	var args struct {
		Path    string `json:"path"`
		Command string `json:"command"`
		Apply   bool   `json:"apply"`
		Targets []struct {
			Path string `json:"path"`
		} `json:"targets"`
	}
	if json.Unmarshal([]byte(call.Arguments), &args) != nil {
		return
	}
	switch strings.ToLower(call.Name) {
	case "read":
		if args.Path != "" {
			read = filepath.Clean(args.Path)
		}
	case "bash":
		read = simpleReadPath(args.Command)
	case "edit", "write":
		if args.Path != "" {
			writes = append(writes, filepath.Clean(args.Path))
		}
	case "ast_edit":
		if args.Apply {
			for _, target := range args.Targets {
				if target.Path != "" {
					writes = append(writes, filepath.Clean(target.Path))
				}
			}
		}
	}
	return
}

var sedReadRange = regexp.MustCompile(`^[0-9]+(,[0-9]+)?p$`)
var lineCount = regexp.MustCompile(`^[0-9]+$`)

func simpleReadPath(command string) string {
	words, ok := simpleShellWords(command)
	if !ok {
		return ""
	}
	if pipe := slices.Index(words, "|"); pipe >= 0 {
		filter := words[pipe+1:]
		if len(filter) != 3 || filter[0] != "sed" || filter[1] != "-n" || !sedReadRange.MatchString(filter[2]) {
			return ""
		}
		words = words[:pipe]
	}
	if len(words) < 2 {
		return ""
	}
	path := words[len(words)-1]
	if path == "" || strings.HasPrefix(path, "-") || path == "|" {
		return ""
	}
	valid := false
	switch words[0] {
	case "cat":
		valid = len(words) == 2
	case "sed":
		valid = len(words) == 4 && words[1] == "-n" && sedReadRange.MatchString(words[2])
	case "head", "tail":
		valid = len(words) == 2 || len(words) == 4 && words[1] == "-n" && lineCount.MatchString(words[2])
	case "nl":
		valid = len(words) == 3 && words[1] == "-ba"
	}
	if !valid {
		return ""
	}
	return filepath.Clean(path)
}

// A deliberately small literal-word lexer, not a shell interpreter. Expansion,
// redirection, lists, substitutions and extra pipelines are left unclassified.
func simpleShellWords(command string) ([]string, bool) {
	var words []string
	var word strings.Builder
	var quote rune
	started, escape := false, false
	flush := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}
	for _, c := range command {
		if escape {
			word.WriteRune(c)
			started, escape = true, false
			continue
		}
		if c == '\n' || c == '\r' {
			return nil, false
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				if quote == '"' && strings.ContainsRune("$`\\", c) {
					return nil, false
				}
				word.WriteRune(c)
			}
			continue
		}
		switch c {
		case '\\':
			escape, started = true, true
		case '\'', '"':
			quote, started = c, true
		case ' ', '\t':
			flush()
		case '|':
			flush()
			words = append(words, "|")
		default:
			if strings.ContainsRune("$`;&<>()*?[]{}~#", c) {
				return nil, false
			}
			word.WriteRune(c)
			started = true
		}
	}
	if quote != 0 || escape {
		return nil, false
	}
	flush()
	return words, true
}
