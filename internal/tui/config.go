package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type TUIConfig struct {
	Theme        string            `json:"theme"`
	PollInterval string            `json:"poll_interval"`
	Keybinds     map[string]string `json:"keybinds"`
}

func DefaultTUIConfig() TUIConfig {
	return TUIConfig{
		Theme:        "default",
		PollInterval: "3s",
		Keybinds:     make(map[string]string),
	}
}

func LoadTUIConfig() TUIConfig {
	cfg := DefaultTUIConfig()

	home, err := os.UserHomeDir()
	if err != nil {
		return cfg
	}

	paths := []string{
		filepath.Join(home, ".config", "fj", "tui.json"),
		filepath.Join(home, ".config", "fj", "tui.jsonc"),
	}

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := stripJSONCComments(string(data))
		var loaded TUIConfig
		if err := json.Unmarshal([]byte(content), &loaded); err != nil {
			continue
		}
		if loaded.Theme != "" {
			cfg.Theme = loaded.Theme
		}
		if loaded.PollInterval != "" {
			cfg.PollInterval = loaded.PollInterval
		}
		if loaded.Keybinds != nil {
			for k, v := range loaded.Keybinds {
				cfg.Keybinds[k] = v
			}
		}
		break
	}

	return cfg
}

func (c TUIConfig) PollDuration() time.Duration {
	d, err := time.ParseDuration(c.PollInterval)
	if err != nil {
		return 3 * time.Second
	}
	return d
}

func (c TUIConfig) ApplyKeybinds(defaults KeyBinds) KeyBinds {
	if v, ok := c.Keybinds["navigate_up"]; ok {
		defaults.NavigateUp = v
	}
	if v, ok := c.Keybinds["navigate_down"]; ok {
		defaults.NavigateDown = v
	}
	if v, ok := c.Keybinds["open"]; ok {
		defaults.Open = v
	}
	if v, ok := c.Keybinds["back"]; ok {
		defaults.Back = v
	}
	if v, ok := c.Keybinds["switch_focus"]; ok {
		defaults.SwitchFocus = v
	}
	if v, ok := c.Keybinds["filter_next"]; ok {
		defaults.FilterNext = v
	}
	if v, ok := c.Keybinds["filter_prev"]; ok {
		defaults.FilterPrev = v
	}
	if v, ok := c.Keybinds["new_issue"]; ok {
		defaults.NewIssue = v
	}
	if v, ok := c.Keybinds["help"]; ok {
		defaults.Help = v
	}
	if v, ok := c.Keybinds["quit"]; ok {
		defaults.Quit = v
	}
	if v, ok := c.Keybinds["metrics"]; ok {
		defaults.Metrics = v
	}
	if v, ok := c.Keybinds["activity"]; ok {
		defaults.Activity = v
	}
	if v, ok := c.Keybinds["enter_command"]; ok {
		defaults.EnterCommand = v
	}
	return defaults
}

func stripJSONCComments(input string) string {
	var b strings.Builder
	for _, line := range strings.Split(input, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}