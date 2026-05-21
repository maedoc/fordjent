package tui

import (
	"strings"
)

var sparklineChars = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

func Sparkline(values []int64, width int) string {
	if len(values) == 0 {
		return strings.Repeat("·", width)
	}
	var max int64
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	if max == 0 {
		return strings.Repeat(string(sparklineChars[0]), min(width, len(values)))
	}
	if len(values) > width {
		start := len(values) - width
		values = values[start:]
	}
	var b strings.Builder
	for _, v := range values {
		idx := int(float64(v) / float64(max) * float64(len(sparklineChars)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sparklineChars) {
			idx = len(sparklineChars) - 1
		}
		b.WriteRune(sparklineChars[idx])
	}
	return b.String()
}