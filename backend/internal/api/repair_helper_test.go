package api_test

import "strings"

// asRepair makes a request body a repair (2.4.0 Phase I4; IN-7): an array
// body is wrapped as {repair, reason, assignments}, an object body gains the
// two fields. A move is a global administrator's repair since I4.
func asRepair(body string) string {
	b := strings.TrimSpace(body)
	switch {
	case strings.HasPrefix(b, "["):
		return `{"repair":true,"reason":"a test's repair","assignments":` + b + `}`
	case b == "{}":
		return `{"repair":true,"reason":"a test's repair"}`
	case strings.HasPrefix(b, "{"):
		return `{"repair":true,"reason":"a test's repair",` + b[1:]
	}
	return b
}

// repairOf is asRepair for a body built as a Go value: a map gains the two
// fields, anything else (a list) is wrapped as the assignments.
func repairOf(body any) map[string]any {
	if m, ok := body.(map[string]any); ok {
		out := map[string]any{"repair": true, "reason": "a test's repair"}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return map[string]any{"repair": true, "reason": "a test's repair", "assignments": body}
}
