package main

import (
	"strings"
	"testing"
)

// Assertions on a run's log, shared by the e2e tests and the integration tests
// (build tag integration, same package).

// wantInLog fails the test if logs lacks a string of wants.
func wantInLog(t *testing.T, logs string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
}

// wantNotInLog fails the test if logs holds a string of unwanted.
func wantNotInLog(t *testing.T, logs string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(logs, u) {
			t.Errorf("want no %q in the log:\n%s", u, logs)
		}
	}
}
