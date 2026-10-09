// Package testcodex provides a metadata-only fake CLI. It has no inference path.
package testcodex

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
)

// Install creates only public, synthetic protocol values in a private test HOME.
func Install(t *testing.T, home string, frames []any) string {
	t.Helper()
	data, err := json.Marshal(frames)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "codex-frames.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	if err = os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

const script = `#!/usr/bin/env python3
import json, os, pathlib, sys, time
home = pathlib.Path(os.environ['HOME'])
with (home/'codex-starts.jsonl').open('a') as f:
    f.write(json.dumps({'argv':sys.argv[1:],'env':dict(os.environ),'cwd':os.getcwd()})+'\n')
frames = json.loads((home/'codex-frames.json').read_text())
for frame in frames:
    request = sys.stdin.readline()
    if not request: sys.exit(4)
    with (home/'codex-requests.jsonl').open('a') as f: f.write(request)
    value = json.loads(request)
    if value.get('method') not in ('initialize','initialized','model/list'): sys.exit(5)
    if frame == 'hang': time.sleep(60)
    elif frame == 'crash': sys.exit(7)
    elif frame == 'malformed': print('{malformed', flush=True)
    elif frame is not None: print(json.dumps(frame), flush=True)
    else: continue
    # initialized has no response. Read it before the next model/list request.
    if value['method'] == 'initialize':
        request = sys.stdin.readline()
        with (home/'codex-requests.jsonl').open('a') as f: f.write(request)
        if json.loads(request).get('method') != 'initialized': sys.exit(6)
sys.stdin.read()
`
