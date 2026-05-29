package speccycle

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// taskLineRegex matches checkbox lines: "- [ ]" or "- [x]" with optional [parallel] tag.
var taskLineRegex = regexp.MustCompile(`^\s*-\s*\[([ xX])\]\s*(.*?)(\s*\[parallel\])?\s*$`)

func parseTasks(repoDir, name string) ([]Task, error) {
	path := tasksPath(repoDir, name)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open tasks %s: %w", path, err)
	}
	defer f.Close()

	var tasks []Task
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		// Try to parse as a checkbox line
		matches := taskLineRegex.FindStringSubmatch(line)
		if matches == nil {
			// Skip non-task lines (headings, blank lines, prose)
			continue
		}

		done := strings.EqualFold(matches[1], "x")
		desc := strings.TrimSpace(matches[2])
		parallel := strings.TrimSpace(matches[3]) == "[parallel]"

		tasks = append(tasks, Task{
			Index:       len(tasks) + 1,
			Description: desc,
			Done:        done,
			Parallel:    parallel,
			Raw:         line,
		})
	}

	if err := scanner.Err(); err != nil {
		return tasks, fmt.Errorf("scan tasks %s: %w", path, err)
	}

	return tasks, nil
}

func markTaskComplete(repoDir, name string, index int) error {
	if index < 1 {
		return fmt.Errorf("task index must be >= 1, got %d", index)
	}

	path := tasksPath(repoDir, name)
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open tasks %s: %w", path, err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	taskNum := 0
	for scanner.Scan() {
		line := scanner.Text()
		if taskLineRegex.MatchString(line) {
			taskNum++
			if taskNum == index {
				// Replace - [ ] with - [x]
				replaced := taskLineRegex.ReplaceAllString(line, "- [x] $2$3")
				lines = append(lines, replaced)
				continue
			}
		}
		lines = append(lines, line)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan tasks %s: %w", path, err)
	}

	if taskNum < index {
		return fmt.Errorf("task index %d out of range: only %d tasks found", index, taskNum)
	}

	// Write back
	output := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(output), 0644); err != nil {
		return fmt.Errorf("write tasks %s: %w", path, err)
	}

	return nil
}
