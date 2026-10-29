package claudeagent

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestStreamJSONByteBoundariesPreserveJapaneseAndInvisiblePrefixes(t *testing.T) {
	for _, text := range []string{"こんにちは。", "\ufeffこんにちは。", "\u200bこんにちは。", "猫👩🏽‍💻é"} {
		delta, _ := json.Marshal(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": text}}})
		assistant, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"id": "japanese", "content": []any{map[string]any{"type": "text", "text": text}}}})
		input := bufio.NewReaderSize(iotest.OneByteReader(strings.NewReader(string(delta)+"\n"+string(assistant)+"\n")), 16)
		streamed := ""
		turn := &turn{ctx: context.Background(), opt: llm.RequestOptions{Progress: func(p llm.Progress) { streamed += p.Delta }}}
		for range 2 {
			raw, err := readLine(input)
			if err != nil {
				t.Fatal(err)
			}
			var f frame
			if err = json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			turn.handle(f)
		}
		if streamed != text || len(turn.text) != 1 || turn.text[0].Data.(llm.Message).Text != text || strings.Contains(streamed, "�") {
			t.Fatalf("UTF-8 changed: stream=%q canonical=%+v", streamed, turn.text)
		}
	}
}
