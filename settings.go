package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const defaultPollInterval = 5
const defaultTerminalApp = "com.mitchellh.ghostty"

var terminalApps = map[string]bool{
	"":                       true,
	"com.mitchellh.ghostty":  true,
	"com.googlecode.iterm2":  true,
	"com.github.wez.wezterm": true,
}

type appSettings struct {
	PollIntervalSeconds int    `json:"pollIntervalSeconds"`
	CompactMode         bool   `json:"compactMode"`
	TerminalApp         string `json:"terminalApp"`
}

type settingsUpdate struct {
	PollIntervalSeconds *int    `json:"pollIntervalSeconds"`
	CompactMode         *bool   `json:"compactMode"`
	TerminalApp         *string `json:"terminalApp"`
}

func defaultSettings() appSettings {
	return appSettings{PollIntervalSeconds: defaultPollInterval, TerminalApp: defaultTerminalApp}
}

func validPollInterval(seconds int) bool {
	return seconds == 5 || seconds == 10 || seconds == 30 || seconds == 60
}

func validTerminalApp(bundleID string) bool {
	return terminalApps[bundleID]
}

func loadSettings(path string) appSettings {
	settings := defaultSettings()
	data, err := os.ReadFile(path)
	if err != nil {
		return settings
	}
	if json.Unmarshal(data, &settings) != nil || !validPollInterval(settings.PollIntervalSeconds) ||
		!validTerminalApp(settings.TerminalApp) {
		return defaultSettings()
	}
	return settings
}

func saveSettings(path string, settings appSettings) error {
	if !validPollInterval(settings.PollIntervalSeconds) {
		return errors.New("poll interval must be 5, 10, 30, or 60 seconds")
	}
	if !validTerminalApp(settings.TerminalApp) {
		return errors.New("terminal app is not supported")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".settings-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func settingsPath() string {
	if configured := os.Getenv("FOREMAN_SETTINGS_PATH"); configured != "" {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "foreman", "settings.json")
}
