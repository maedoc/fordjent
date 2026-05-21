package tui

type KeyBinds struct {
	NavigateUp   string
	NavigateDown  string
	Open         string
	Back         string
	SwitchFocus  string
	FilterNext   string
	FilterPrev   string
	NewIssue     string
	Help         string
	Quit         string
	Metrics      string
	Activity     string
	EnterCommand string
}

func DefaultKeyBinds() KeyBinds {
	return KeyBinds{
		NavigateUp:   "up",
		NavigateDown: "down",
		Open:         "enter",
		Back:         "esc",
		SwitchFocus:  "tab",
		FilterNext:   "]",
		FilterPrev:   "[",
		NewIssue:     "n",
		Help:         "?",
		Quit:         "q",
		Metrics:      "m",
		Activity:     "a",
		EnterCommand:  "/",
	}
}