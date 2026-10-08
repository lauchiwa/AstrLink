package forkcheckin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

const (
	// SettingsSchemaVersion is the only private settings layout this build
	// reads. The file holds the switch and nothing else: accounts, sessions
	// and schedules live in the extension's own tables.
	SettingsSchemaVersion = 1
	maxSettingsBytes      = 4 << 10
)

// Stable reasons a stored switch could not be read. The Core keeps running
// with the extension disabled; none of them carries file content or a path.
const (
	SettingsReasonInvalid     = "settings_invalid"
	SettingsReasonUnsupported = "settings_unsupported_version"
	SettingsReasonUnreadable  = "settings_unreadable"
)

// Settings is the persisted extension switch. It defaults to off.
type Settings struct {
	SchemaVersion int  `json:"schema_version"`
	Enabled       bool `json:"enabled"`
}

// LoadSettings never fails the caller. A missing file is the default
// (disabled, no reason); anything unusable is disabled with a stable reason,
// so a damaged or newer file cannot block the Core or silently enable work.
func LoadSettings(path string) (Settings, string) {
	disabled := Settings{SchemaVersion: SettingsSchemaVersion}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return disabled, ""
	}
	if err != nil {
		return disabled, SettingsReasonUnreadable
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxSettingsBytes+1))
	if err != nil {
		return disabled, SettingsReasonUnreadable
	}
	if len(content) > maxSettingsBytes {
		return disabled, SettingsReasonInvalid
	}
	// Read the version alone first, so a newer layout is reported as
	// unsupported rather than as corruption of this build's layout.
	var probe struct {
		SchemaVersion *int `json:"schema_version"`
	}
	if json.Unmarshal(content, &probe) != nil || probe.SchemaVersion == nil {
		return disabled, SettingsReasonInvalid
	}
	if *probe.SchemaVersion != SettingsSchemaVersion {
		return disabled, SettingsReasonUnsupported
	}
	var settings struct {
		SchemaVersion int   `json:"schema_version"`
		Enabled       *bool `json:"enabled"`
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&settings) != nil || settings.Enabled == nil || decoder.More() {
		return disabled, SettingsReasonInvalid
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return disabled, SettingsReasonInvalid
	}
	return Settings{SchemaVersion: SettingsSchemaVersion, Enabled: *settings.Enabled}, ""
}

// SaveSettings atomically replaces the switch with a private file, following
// the local key's temp-file, fsync and directory-sync pattern. A crash leaves
// either the old or the new file, never a partial one.
func SaveSettings(path string, settings Settings) (err error) {
	settings.SchemaVersion = SettingsSchemaVersion
	content, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create check-in settings directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create check-in settings: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = temporary.Close()
		return fmt.Errorf("restrict check-in settings: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write check-in settings: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync check-in settings: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close check-in settings: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install check-in settings: %w", err)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open check-in settings directory: %w", err)
	}
	defer handle.Close()
	return handle.Sync()
}
