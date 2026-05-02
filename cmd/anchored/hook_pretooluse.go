package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

func runHookPreToolUse(args []string) {
	fs := newFlagSet("hook pretooluse")
	_ = fs.String("config", "", "path to config file")
	fs.Parse(args)

	content, err := io.ReadAll(os.Stdin)
	if err != nil {
		slog.Error("failed to read stdin", "error", err)
		os.Exit(1)
	}

	var input struct {
		Tool      string                 `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(content, &input); err != nil {
		outputJSON(map[string]string{"decision": "allow"})
		return
	}

	// Security checks for command execution tools
	if input.Tool == "anchored_execute" || input.Tool == "anchored_execute_file" || input.Tool == "anchored_batch_execute" {
		code, _ := input.Arguments["code"].(string)
		// For batch, also check each command
		if input.Tool == "anchored_batch_execute" {
			if cmds, ok := input.Arguments["commands"].([]any); ok {
				for _, cmd := range cmds {
					if m, ok := cmd.(map[string]any); ok {
						if c, ok := m["command"].(string); ok && c != "" {
							code += "\n" + c
						}
					}
				}
			}
		}
		if blocked, pattern := checkDangerousPattern(code); blocked {
			outputJSON(map[string]string{
				"decision": "block",
				"reason":   "dangerous pattern detected: " + pattern,
			})
			return
		}
	}

	// Routing advice for memory-related queries
	if mentionsMemory(input.Arguments) {
		outputJSON(map[string]string{
			"decision": "allow",
			"reason":   "consider anchored_search for memory queries",
		})
		return
	}

	outputJSON(map[string]string{"decision": "allow"})
}

func checkDangerousPattern(code string) (blocked bool, pattern string) {
	dangerous := []string{
		"rm -rf /",
		"rm -rf /*",
		":(){:|:&};:",
		"dd if=/dev/zero",
		"mkfs",
		"format c:",
		"curl",
		"wget",
		"nc -l",
	}
	lower := strings.ToLower(code)
	for _, d := range dangerous {
		if strings.Contains(lower, strings.ToLower(d)) {
			// Fine-grained: curl/wget only block if piping to shell
			if d == "curl" || d == "wget" {
				if strings.Contains(lower, "|") && (strings.Contains(lower, "sh") || strings.Contains(lower, "bash")) {
					return true, d + " piped to shell"
				}
				continue
			}
			return true, d
		}
	}
	return false, ""
}

func mentionsMemory(args map[string]any) bool {
	memoryWords := []string{"memory", "fact", "decision", "preference"}
	for _, v := range args {
		s, ok := v.(string)
		if !ok {
			continue
		}
		lower := strings.ToLower(s)
		for _, w := range memoryWords {
			if strings.Contains(lower, w) {
				return true
			}
		}
	}
	return false
}

func outputJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("failed to marshal JSON", "error", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}
