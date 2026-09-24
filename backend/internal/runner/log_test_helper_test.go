package runner

import (
	"bufio"
	"os"
)

// linesTo reads all lines of a file into a slice. Test-only helper used by the
// runner log tests to assert on written log content.
func linesTo(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}
