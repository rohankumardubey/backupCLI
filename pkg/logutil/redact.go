// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package logutil

import (
	"encoding/hex"
	"strings"

	"github.com/pingcap/errors"
)

// InitRedact inits the enableRedactLog
func InitRedact(redactLog bool) {
	errors.RedactLogEnabled.Store(redactLog)
}

// NeedRedact returns whether to redact log
func NeedRedact() bool {
	return errors.RedactLogEnabled.Load()
}

// RedactString receives string argument and return omitted information if redact log enabled
func RedactString(arg string) string {
	if NeedRedact() {
		return "?"
	}
	return arg
}

type stringer struct{}

// String implement fmt.Stringer
func (s stringer) String() string {
	return "?"
}

// RedactKey receives a key return omitted information if redact log enabled
func RedactKey(key []byte) string {
	if NeedRedact() {
		return "?"
	}
	return strings.ToUpper(hex.EncodeToString(key))
}
