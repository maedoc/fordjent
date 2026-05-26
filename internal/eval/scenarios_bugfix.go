package eval

import "time"

// GreenfieldScenario tests the full PM→implementer→reviewer pipeline
// by asking the agent to build a stringutil CLI from a seeded empty repo.
var GreenfieldScenario = Scenario{
	Name:        "greenfield",
	Description: "Build a stringutil CLI with reverse and wordcount commands from empty repo",
	RepoName:    "bench-greenfield",
	IssueTitle:   "[pm] Build a string utility CLI with reverse and wordcount commands",
	IssueBody: `## Project

Build a Go CLI tool called 'stringutil' with two subcommands.

### Commands
- reverse — reverses the input string
- wordcount — counts words in the input string

### Structure
- cmd/stringutil/main.go — CLI entry point
- pkg/stringutil/reverse.go — reverse implementation
- pkg/stringutil/wordcount.go — word count implementation
- pkg/stringutil/reverse_test.go — tests
- pkg/stringutil/wordcount_test.go — tests

### Please
1. Decompose into sub-issues with [implementer] and [tester] tags
2. Create milestone, attach sub-issues
3. Use Depends on: #N for dependencies`,
	SeedFiles: map[string]string{
		"go.mod":     "module bench-greenfield\n\ngo 1.26",
		".gitignore": "*.o\n*.exe\nstringutil\n",
		"README.md":  "# bench-greenfield\n\nA string utility CLI.\n",
	},
	Verify:  GreenfieldVerify,
	Timeout: 15 * time.Minute,
}

// BugfixScenario tests targeted maintenance by asking the agent
// to fix a known off-by-one error in existing code.
var BugfixScenario = Scenario{
	Name:        "bugfix",
	Description: "Fix off-by-one error in binary search implementation",
	RepoName:    "bench-bugfix",
	IssueTitle:   "[implementer] Binary search returns wrong index for edge cases",
	IssueBody: `## Bug

The BinarySearch function in pkg/search/search.go fails TestFindLastElement.

### Steps to reproduce
Run go test ./pkg/search/... — TestFindLastElement fails.

### Expected behavior
- BinarySearch should correctly find elements at all positions
- Should handle empty slices
- Should handle single-element slices
- Should not overflow for large slices`,
	SeedFiles: map[string]string{
		"go.mod":     "module bench-bugfix\n\ngo 1.26",
		".gitignore": "*.o\n*.exe\nbenchbug\n",
		"pkg/search/search.go": `package search

// BinarySearch returns the index of target in sorted arr, or -1 if not found.
// BUG: loop condition should be low <= high, and mid should use
// overflow-safe calculation low + (high-low)/2.
func BinarySearch(arr []int, target int) int {
	low, high := 0, len(arr)-1
	for low < high {
		mid := (low + high) / 2
		if arr[mid] < target {
			low = mid + 1
		} else {
			high = mid
		}
	}
	if len(arr) == 0 {
		return -1
	}
	if arr[low] == target {
		return low
	}
	return -1
}
`,
		"pkg/search/search_test.go": `package search

import "testing"

func TestFindFirstElement(t *testing.T) {
	arr := []int{1, 3, 5, 7, 9}
	if got := BinarySearch(arr, 1); got != 0 {
		t.Errorf("BinarySearch([1,3,5,7,9], 1) = %d, want 0", got)
	}
}

func TestFindMiddleElement(t *testing.T) {
	arr := []int{1, 3, 5, 7, 9}
	if got := BinarySearch(arr, 5); got != 2 {
		t.Errorf("BinarySearch([1,3,5,7,9], 5) = %d, want 2", got)
	}
}

func TestFindLastElement(t *testing.T) {
	arr := []int{1, 3, 5, 7, 9}
	if got := BinarySearch(arr, 9); got != 4 {
		t.Errorf("BinarySearch([1,3,5,7,9], 9) = %d, want 4", got)
	}
}

func TestNotFound(t *testing.T) {
	arr := []int{1, 3, 5, 7, 9}
	if got := BinarySearch(arr, 4); got != -1 {
		t.Errorf("BinarySearch([1,3,5,7,9], 4) = %d, want -1", got)
	}
}

func TestEmptySlice(t *testing.T) {
	arr := []int{}
	if got := BinarySearch(arr, 1); got != -1 {
		t.Errorf("BinarySearch([], 1) = %d, want -1", got)
	}
}
`,
	},
	Verify:  BugfixVerify,
	Timeout: 10 * time.Minute,
}