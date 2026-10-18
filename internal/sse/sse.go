// Package sse reads data frames shared by the native model adapters.
package sse

import (
	"bufio"
	"context"
	"io"
	"strings"
)

// Read joins data lines and dispatches complete frames. stopped reports whether
// handle returned false; otherwise the reader reached EOF. Protocol completion
// and payload decoding remain the caller's responsibility.
func Read(ctx context.Context, r io.Reader, handle func(string) bool) (stopped bool, err error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	var data []string
	first := true
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		line := scanner.Text()
		if first {
			line = strings.TrimPrefix(line, "\uFEFF")
			first = false
		}
		if line == "" {
			if len(data) > 0 {
				if !handle(strings.Join(data, "\n")) {
					return true, nil
				}
				data = nil
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	if len(data) > 0 && !handle(strings.Join(data, "\n")) {
		return true, nil
	}
	return false, nil
}
