// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

// Package utils contains small, dependency-free helpers.
package utils

import (
	"fmt"
	"strings"
	"unicode"
)

// SplitCommandArgs splits a command argument string into individual arguments.
func SplitCommandArgs(input string) ([]string, error) {
	var args []string
	var value strings.Builder
	var quote rune
	escaped := false

	flush := func() {
		if value.Len() > 0 {
			args = append(args, value.String())
			value.Reset()
		}
	}

	for _, char := range input {
		switch {
		case escaped:
			value.WriteRune(char)
			escaped = false
		case char == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if char == quote {
				quote = 0
			} else {
				value.WriteRune(char)
			}
		case char == '\'' || char == '"':
			quote = char
		case unicode.IsSpace(char):
			flush()
		default:
			value.WriteRune(char)
		}
	}

	if escaped {
		return nil, fmt.Errorf("trailing escape")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	flush()
	return args, nil
}
