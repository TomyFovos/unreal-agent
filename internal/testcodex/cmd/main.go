// Command testcodex is an offline, metadata-only Codex CLI fixture.
package main

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() { os.Exit(run()) }

func run() int {
	home := os.Getenv("HOME")
	cwd, err := os.Getwd()
	if err != nil {
		return 1
	}
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
	start, err := json.Marshal(struct {
		Argv []string          `json:"argv"`
		Env  map[string]string `json:"env"`
		Cwd  string            `json:"cwd"`
	}{os.Args[1:], env, cwd})
	if err != nil || appendLine(filepath.Join(home, "codex-starts.jsonl"), append(start, '\n')) != nil {
		return 1
	}
	data, err := os.ReadFile(filepath.Join(home, "codex-frames.json"))
	if err != nil {
		return 1
	}
	var frames []jsontext.Value
	if json.Unmarshal(data, &frames) != nil {
		return 1
	}
	input := bufio.NewReader(os.Stdin)
	readMethod := func() (string, error) {
		line, err := input.ReadBytes('\n')
		if err != nil {
			return "", err
		}
		if err := appendLine(filepath.Join(home, "codex-requests.jsonl"), line); err != nil {
			return "", err
		}
		var request struct {
			Method string `json:"method"`
		}
		err = json.Unmarshal(line, &request)
		return request.Method, err
	}
	for _, frame := range frames {
		method, err := readMethod()
		if err != nil {
			return 4
		}
		if method != "initialize" && method != "initialized" && method != "model/list" {
			return 5
		}
		var mode string
		_ = json.Unmarshal(frame, &mode)
		switch mode {
		case "hang":
			time.Sleep(60 * time.Second)
		case "crash":
			return 7
		case "malformed":
			if _, err := io.WriteString(os.Stdout, "{malformed\n"); err != nil {
				return 1
			}
		default:
			if string(frame) == "null" {
				continue
			}
			if _, err := os.Stdout.Write(append(frame, '\n')); err != nil {
				return 1
			}
		}
		// initialized has no response; consume it before model/list.
		if method == "initialize" {
			if next, err := readMethod(); err != nil || next != "initialized" {
				return 6
			}
		}
	}
	if _, err := io.Copy(io.Discard, input); err != nil {
		return 1
	}
	return 0
}

func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(line)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
