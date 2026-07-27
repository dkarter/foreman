package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const defaultPollInterval = 5
const defaultTerminalApp = "com.mitchellh.ghostty"
const defaultBackground = "grid"
const defaultTheme = "default"

var backgrounds = map[string]bool{
	"grid":      true,
	"aurora":    true,
	"embers":    true,
	"topo":      true,
	"eclipse":   true,
	"rain":      true,
	"tesseract": true,
	"drive":     true,
}

var themes = map[string]bool{
	"default":     true,
	"catppuccin":  true,
	"tokyo-night": true,
	"dracula":     true,
	"nord":        true,
	"gruvbox":     true,
}

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
	Background          string `json:"background"`
	Theme               string `json:"theme"`
	AccentColor         string `json:"accentColor"`
}

type settingsUpdate struct {
	PollIntervalSeconds *int    `json:"pollIntervalSeconds"`
	CompactMode         *bool   `json:"compactMode"`
	TerminalApp         *string `json:"terminalApp"`
	Background          *string `json:"background"`
	Theme               *string `json:"theme"`
	AccentColor         *string `json:"accentColor"`
}

func defaultSettings() appSettings {
	return appSettings{
		PollIntervalSeconds: defaultPollInterval,
		TerminalApp:         defaultTerminalApp,
		Background:          defaultBackground,
		Theme:               defaultTheme,
	}
}

func validPollInterval(seconds int) bool {
	return seconds == 5 || seconds == 10 || seconds == 30 || seconds == 60
}

func validTerminalApp(bundleID string) bool {
	return terminalApps[bundleID]
}

func validBackground(background string) bool {
	return backgrounds[background]
}

func validTheme(theme string) bool {
	return themes[theme]
}

func validAccentColor(color string) bool {
	if color == "" {
		return true
	}
	if len(color) != 7 || color[0] != '#' {
		return false
	}
	for _, character := range color[1:] {
		if !((character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'f') ||
			(character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func loadSettings(path string) appSettings {
	settings := defaultSettings()
	data, err := os.ReadFile(path)
	if err != nil {
		return settings
	}
	if json.Unmarshal(data, &settings) != nil {
		return defaultSettings()
	}
	if settings.Background == "radar" {
		settings.Background = defaultBackground
	}
	if !validPollInterval(settings.PollIntervalSeconds) ||
		!validTerminalApp(settings.TerminalApp) || !validBackground(settings.Background) ||
		!validTheme(settings.Theme) || !validAccentColor(settings.AccentColor) {
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
	if !validBackground(settings.Background) {
		return errors.New("background is not supported")
	}
	if !validTheme(settings.Theme) {
		return errors.New("theme is not supported")
	}
	if !validAccentColor(settings.AccentColor) {
		return errors.New("accent color must be a six-digit hex color")
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
