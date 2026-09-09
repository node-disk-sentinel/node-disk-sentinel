// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

func TestMustFormatValue_ValidValueRemainsUnchanged(t *testing.T) {
	tests := []string{
		"node-1",
		"worker-01.example.com",
		"node-worker-0",
		"a",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			got := MustFormatValue(name)
			if got != name {
				t.Errorf("MustFormatValue(%q) = %q; want unchanged %q", name, got, name)
			}
			if errs := validation.IsValidLabelValue(got); len(errs) > 0 {
				t.Errorf("MustFormatValue(%q) = %q is not a valid label value: %v", name, got, errs)
			}
		})
	}
}

func TestMustFormatValue_LongNodeNameHashedToValidLabel(t *testing.T) {
	// Node names are valid DNS subdomains and may be up to 253 characters,
	// far exceeding validation.LabelValueMaxLength (63).
	longName := ""
	for i := 0; i < 100; i++ {
		longName += "a"
	}
	longName += ".very.long.internal.example.com"

	got := MustFormatValue(longName)
	if errs := validation.IsValidLabelValue(got); len(errs) > 0 {
		t.Fatalf("MustFormatValue(long name) = %q is not a valid label value: %v", got, errs)
	}

	// Deterministic: same input always yields the same output.
	got2 := MustFormatValue(longName)
	if got != got2 {
		t.Errorf("MustFormatValue is not deterministic: %q != %q", got, got2)
	}

	// Distinct long inputs must not collide.
	otherLongName := ""
	for i := 0; i < 100; i++ {
		otherLongName += "a"
	}
	otherLongName += ".very.long.internal.example.org"

	gotOther := MustFormatValue(otherLongName)
	if got == gotOther {
		t.Errorf("distinct long node names collided to the same label value %q", got)
	}
}
