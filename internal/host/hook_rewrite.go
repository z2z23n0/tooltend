package host

import (
	"errors"
	"path/filepath"
	"strings"
)

type shellToken struct {
	start, end int
	value      string
}

// RewriteHookCommand replaces a simple existing hook executable (or a
// supported interpreter+script pair) while preserving literal trailing args.
// Complex shell programs are rejected instead of being reinterpreted.
func RewriteHookCommand(original, executable string) (string, error) {
	if !filepath.IsAbs(executable) || strings.ContainsAny(executable, "\x00\r\n") {
		return "", errors.New("host: managed hook executable must be an absolute path")
	}
	tokens, err := literalShellTokens(original)
	if err != nil || len(tokens) == 0 {
		return "", errors.New("host: existing hook command is not a supported literal command")
	}
	commandIndex := 0
	for commandIndex < len(tokens) && strings.Contains(tokens[commandIndex].value, "=") && !strings.HasPrefix(tokens[commandIndex].value, "=") {
		commandIndex++
	}
	if commandIndex >= len(tokens) {
		return "", errors.New("host: existing hook command has no executable")
	}
	replaceStart, replaceEnd := tokens[commandIndex].start, tokens[commandIndex].end
	carrier := strings.ToLower(filepath.Base(tokens[commandIndex].value))
	if carrier == "env" {
		commandIndex++
		for commandIndex < len(tokens) && (strings.HasPrefix(tokens[commandIndex].value, "-") || strings.Contains(tokens[commandIndex].value, "=")) {
			commandIndex++
		}
		if commandIndex >= len(tokens) {
			return "", errors.New("host: env hook command has no executable")
		}
		replaceEnd = tokens[commandIndex].end
		carrier = strings.ToLower(filepath.Base(tokens[commandIndex].value))
	}
	if carrier == "bash" || carrier == "sh" || carrier == "python" || carrier == "python3" || carrier == "node" {
		scriptIndex := commandIndex + 1
		for scriptIndex < len(tokens) && strings.HasPrefix(tokens[scriptIndex].value, "-") {
			scriptIndex++
		}
		if scriptIndex >= len(tokens) {
			return "", errors.New("host: interpreted hook command has no script")
		}
		replaceEnd = tokens[scriptIndex].end
	}
	return original[:replaceStart] + shellQuote(executable) + original[replaceEnd:], nil
}

func literalShellTokens(command string) ([]shellToken, error) {
	if strings.TrimSpace(command) == "" || strings.ContainsAny(command, "\x00\r\n;|&<>$`(){}") {
		return nil, errors.New("complex shell command")
	}
	var result []shellToken
	for index := 0; index < len(command); {
		for index < len(command) && (command[index] == ' ' || command[index] == '\t') {
			index++
		}
		if index == len(command) {
			break
		}
		start := index
		var value strings.Builder
		quote := byte(0)
		for index < len(command) {
			char := command[index]
			if quote == 0 && (char == ' ' || char == '\t') {
				break
			}
			if char == '\'' || char == '"' {
				if quote == 0 {
					quote = char
					index++
					continue
				}
				if quote == char {
					quote = 0
					index++
					continue
				}
			}
			if char == '\\' && quote != '\'' {
				index++
				if index >= len(command) {
					return nil, errors.New("trailing escape")
				}
				char = command[index]
			}
			value.WriteByte(char)
			index++
		}
		if quote != 0 || value.Len() == 0 {
			return nil, errors.New("unterminated or empty shell token")
		}
		result = append(result, shellToken{start: start, end: index, value: value.String()})
	}
	return result, nil
}
