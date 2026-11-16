package buildfeatures

import (
	"errors"
	"fmt"
	"runtime/debug"
)

const OpenCodeGoProvider = "opencode-go"
const OpenCodeGoDisabledCode = "opencode_go_disabled"
const OpenCodeGoChildDisabledMessage = "この配布物では OpenCode Go は使えません。/model-sub でほかの接続を選んでください。"
const OpenCodeGoDisabledMessage = "この配布物では OpenCode Go は使えません。/model でほかの接続を選んでください。"

var ErrOpenCodeGoDisabled = errors.New(OpenCodeGoDisabledCode + ": " + OpenCodeGoDisabledMessage)

func Disabled(provider string) bool {
	return provider == OpenCodeGoProvider && !OpenCodeGoEnabled
}

func CheckProvider(provider string) error {
	if Disabled(provider) {
		return ErrOpenCodeGoDisabled
	}
	return nil
}

func Version(revision string) string {
	if revision == "" {
		revision = "dev"
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					revision = setting.Value
				}
			}
		}
	}
	return fmt.Sprintf("%s\nopencode_go_enabled=%t", revision, OpenCodeGoEnabled)
}
