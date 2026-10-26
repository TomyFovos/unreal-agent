package tui

import "encoding/base64"

// clipboardSequence asks the terminal to put text on the system clipboard
// (OSC 52). Windows Terminal and most modern terminals honor it, including
// through a remote shell such as sbx exec, so no local clipboard tool is needed.
func clipboardSequence(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
}
