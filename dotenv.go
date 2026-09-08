package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
)

func loadDotEnv(path string, required bool) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open environment file %s: %w", path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if lineNumber == 1 {
			line = strings.TrimPrefix(line, "\uFEFF")
		}
		key, value, err := parseDotEnvLine(line)
		if err != nil {
			return fmt.Errorf("parse environment file %s line %d: %w", path, lineNumber, err)
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set %s from environment file: %w", key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read environment file %s: %w", path, err)
	}
	return nil
}

func parseDotEnvLine(line string) (string, string, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", nil
	}
	if strings.HasPrefix(line, "export ") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	}
	separator := strings.IndexByte(line, '=')
	if separator < 1 {
		return "", "", errors.New("expected KEY=VALUE")
	}
	key := strings.TrimSpace(line[:separator])
	if !validEnvKey(key) {
		return "", "", fmt.Errorf("invalid variable name %q", key)
	}
	value, err := parseDotEnvValue(strings.TrimSpace(line[separator+1:]))
	if err != nil {
		return "", "", err
	}
	return key, value, nil
}

func parseDotEnvValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value[0] != '\'' && value[0] != '"' {
		for i := 1; i < len(value); i++ {
			if value[i] == '#' && unicode.IsSpace(rune(value[i-1])) {
				value = value[:i]
				break
			}
		}
		return strings.TrimSpace(value), nil
	}

	quote := value[0]
	closing := -1
	escaped := false
	for i := 1; i < len(value); i++ {
		if quote == '"' && value[i] == '\\' && !escaped {
			escaped = true
			continue
		}
		if value[i] == quote && !escaped {
			closing = i
			break
		}
		escaped = false
	}
	if closing < 0 {
		return "", errors.New("unterminated quoted value")
	}
	remainder := strings.TrimSpace(value[closing+1:])
	if remainder != "" && !strings.HasPrefix(remainder, "#") {
		return "", errors.New("unexpected text after quoted value")
	}
	quoted := value[:closing+1]
	if quote == '\'' {
		return quoted[1 : len(quoted)-1], nil
	}
	decoded, err := strconv.Unquote(quoted)
	if err != nil {
		return "", errors.New("invalid double-quoted value")
	}
	return decoded, nil
}

func validEnvKey(key string) bool {
	for i, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return key != ""
}
