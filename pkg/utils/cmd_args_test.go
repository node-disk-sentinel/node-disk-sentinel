// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"reflect"
	"testing"
)

func TestSplitCommandArgs(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"whitespace", "  -d   sat  -T permissive ", []string{"-d", "sat", "-T", "permissive"}, false},
		{"quoted argument", `-d "sat,12" -T permissive`, []string{"-d", "sat,12", "-T", "permissive"}, false},
		{"single quotes", `-d 'megaraid,0'`, []string{"-d", "megaraid,0"}, false},
		{"escaped whitespace", `-d sat\ auto`, []string{"-d", "sat auto"}, false},
		{"shell characters remain literal", `-d sat; $(bad) | test`, []string{"-d", "sat;", "$(bad)", "|", "test"}, false},
		{"unterminated quote", `-d "sat`, nil, true},
		{"trailing escape", `-d sat\`, nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SplitCommandArgs(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v; wantErr = %t", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitCommandArgs(%q) = %#v; want %#v", tt.input, got, tt.want)
			}
		})
	}
}
