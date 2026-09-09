// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"encoding/base64"
	"fmt"
	"hash/fnv"

	"k8s.io/apimachinery/pkg/util/validation"
)

// MustFormatValue returns the passed inputLabelValue if it meets the standards for a Kubernetes label value.
// If the name is not a valid label value this function returns a hash which meets the requirements.
//
// Matches sigs.k8s.io/cluster-api/util/labels/format.MustFormatValue.
func MustFormatValue(str string) string {
	if len(validation.IsValidLabelValue(str)) == 0 {
		return str
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(str))
	return fmt.Sprintf("hash_%s_z", base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(hasher.Sum(nil)))
}
