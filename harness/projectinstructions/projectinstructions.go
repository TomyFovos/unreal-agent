// Package projectinstructions binds a workspace's project instruction surface
// to a session. Instructions are model guidance only: they never grant
// permissions, tools, credentials, or lifecycle authority.
package projectinstructions

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Version is the persisted Snapshot schema version.
const Version = 1

// FileName is the only surface discovered: the workspace root AGENTS.md.
const FileName = "AGENTS.md"

// MaxBytes bounds AGENTS.md. Larger files fail; they are never truncated.
const MaxBytes = 64 << 10

type SourceKind string

const (
	// SourceNone records that a session was bound with no AGENTS.md present.
	SourceNone SourceKind = "none"
	// SourceWorkspaceAgents is <workspace root>/AGENTS.md.
	SourceWorkspaceAgents SourceKind = "workspace_agents_md"
)

// Snapshot is the exact instruction text a session was created with. It is
// persisted so resume, fork, and child sessions never reread the filesystem.
type Snapshot struct {
	Version    uint32
	SourceKind SourceKind
	SourcePath string `json:",omitzero"`
	Content    string `json:",omitzero"`
	Digest     string `json:",omitzero"`
	ByteLength int64  `json:",omitzero"`
}

// Metadata is the non-content part of a Snapshot, suitable for inspection.
type Metadata struct {
	Version    uint32
	SourceKind SourceKind
	SourcePath string `json:",omitzero"`
	Digest     string `json:",omitzero"`
	ByteLength int64  `json:",omitzero"`
}

// None is the snapshot of a workspace without AGENTS.md.
func None() Snapshot { return Snapshot{Version: Version, SourceKind: SourceNone} }

// FromContent validates AGENTS.md bytes and returns their snapshot.
func FromContent(content []byte) (Snapshot, error) {
	if len(content) > MaxBytes {
		return Snapshot{}, &Error{Code: CodeOversized, Path: FileName, Err: fmt.Errorf("file exceeds %d bytes", MaxBytes)}
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return Snapshot{}, &Error{Code: CodeBinary, Path: FileName, Err: errors.New("file contains NUL bytes")}
	}
	if !utf8.Valid(content) {
		return Snapshot{}, &Error{Code: CodeInvalidEncoding, Path: FileName, Err: errors.New("file is not valid UTF-8")}
	}
	return Snapshot{
		Version:    Version,
		SourceKind: SourceWorkspaceAgents,
		SourcePath: FileName,
		Content:    string(content),
		Digest:     digest(content),
		ByteLength: int64(len(content)),
	}, nil
}

func digest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Validate rejects snapshots whose content does not match their identity.
func (s Snapshot) Validate() error {
	if s.Version != Version {
		return fmt.Errorf("unsupported project instructions version %d", s.Version)
	}
	switch s.SourceKind {
	case SourceNone:
		if s.SourcePath != "" || s.Content != "" || s.Digest != "" || s.ByteLength != 0 {
			return errors.New("project instructions without a source carry content")
		}
	case SourceWorkspaceAgents:
		if s.SourcePath != FileName {
			return fmt.Errorf("unsupported project instructions path %q", s.SourcePath)
		}
		want, err := FromContent([]byte(s.Content))
		if err != nil {
			return err
		}
		if s.Digest != want.Digest || s.ByteLength != want.ByteLength {
			return errors.New("project instructions digest or length mismatch")
		}
	default:
		return fmt.Errorf("unsupported project instructions source %q", s.SourceKind)
	}
	return nil
}

// Validate checks public identity fields without requiring persisted content.
func (m Metadata) Validate() error {
	if m.Version != Version {
		return fmt.Errorf("unsupported project instruction metadata version %d", m.Version)
	}
	switch m.SourceKind {
	case SourceNone:
		if m.SourcePath != "" || m.Digest != "" || m.ByteLength != 0 {
			return errors.New("metadata without a source carries content identity")
		}
	case SourceWorkspaceAgents:
		if m.SourcePath != FileName || m.ByteLength < 0 || m.ByteLength > MaxBytes || len(m.Digest) != len("sha256:")+64 || m.Digest[:len("sha256:")] != "sha256:" {
			return errors.New("invalid project instruction metadata")
		}
		if _, err := hex.DecodeString(m.Digest[len("sha256:"):]); err != nil {
			return fmt.Errorf("invalid project instruction digest: %w", err)
		}
	default:
		return fmt.Errorf("unsupported project instruction metadata source %q", m.SourceKind)
	}
	return nil
}

func (s Snapshot) Metadata() Metadata {
	return Metadata{Version: s.Version, SourceKind: s.SourceKind, SourcePath: s.SourcePath, Digest: s.Digest, ByteLength: s.ByteLength}
}

type Code string

const (
	CodeInvalidWorkspace Code = "invalid_workspace"
	CodeUnreadable       Code = "unreadable"
	CodeSymlink          Code = "symlink"
	CodeNotRegular       Code = "not_regular"
	CodeOversized        Code = "oversized"
	CodeBinary           Code = "binary"
	CodeInvalidEncoding  Code = "invalid_encoding"
	CodeUnsupported      Code = "unsupported_platform"
)

// Error is an explicit discovery failure. A present but unusable AGENTS.md
// fails session creation instead of being skipped.
type Error struct {
	Code Code
	Path string
	Err  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("project instructions %s: %s: %v", e.Path, e.Code, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }
