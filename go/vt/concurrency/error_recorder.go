// Copyright 2013, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package concurrency

import (
	"fmt"
	"strings"
	"sync"

	"code.google.com/p/vitess/go/relog"
)

// ErrorRecorder offers a way to record errors during complex
// asynchronous operations.  Various implementation will offer
// different services.
type ErrorRecorder interface {
	RecordError(error)
	HasErrors() bool
	Error() error
}

// FirstErrorRecorder records the first error, logs the others.
// Error() will return the first recorded error or nil.
type FirstErrorRecorder struct {
	mu         sync.Mutex
	errorCount int
	firstError error
}

// RecordError records a possible error:
// - does nothing if err is nil
// - only records the first error reported
// - the rest is just logged
func (fer *FirstErrorRecorder) RecordError(err error) {
	if err == nil {
		return
	}
	fer.mu.Lock()
	fer.errorCount++
	if fer.errorCount == 1 {
		fer.firstError = err
	} else {
		relog.Error("FirstErrorRecorder: error[%v]: %v", fer.errorCount, err)
	}
	fer.mu.Unlock()
}

// HasErrors returns true if we ever recorded an error
func (fer *FirstErrorRecorder) HasErrors() bool {
	fer.mu.Lock()
	defer fer.mu.Unlock()
	return fer.errorCount > 0
}

// Error returns the first error we saw, or nil
func (fer *FirstErrorRecorder) Error() error {
	fer.mu.Lock()
	defer fer.mu.Unlock()
	return fer.firstError
}

// AllErrorRecorder records all the errors.
type AllErrorRecorder struct {
	mu     sync.Mutex
	Errors []string
}

// RecordError records a possible error:
// - does nothing if err is nil
func (aer *AllErrorRecorder) RecordError(err error) {
	if err == nil {
		return
	}
	aer.mu.Lock()
	aer.Errors = append(aer.Errors, err.Error())
	aer.mu.Unlock()
}

// HasErrors returns true if we ever recorded an error
func (aer *AllErrorRecorder) HasErrors() bool {
	aer.mu.Lock()
	defer aer.mu.Unlock()
	return len(aer.Errors) > 0
}

// Error() returns an aggregate of all errors.
func (aer *AllErrorRecorder) Error() error {
	aer.mu.Lock()
	defer aer.mu.Unlock()
	if len(aer.Errors) == 0 {
		return nil
	}
	return fmt.Errorf("%v", strings.Join(aer.Errors, "\n"))
}
