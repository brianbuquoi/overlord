package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestWriteRollbackErrors_Empty(t *testing.T) {
	var buf bytes.Buffer
	writeRollbackErrors(&buf, nil)
	if buf.Len() != 0 {
		t.Errorf("expected no output for empty errs; got %q", buf.String())
	}
}

func TestWriteRollbackErrors_Multiple(t *testing.T) {
	var buf bytes.Buffer
	errs := []error{
		errors.New("remove copied file /tmp/foo: permission denied"),
		errors.New("restore backup /tmp/bar.bak -> /tmp/bar: cross-device link"),
	}
	writeRollbackErrors(&buf, errs)
	got := buf.String()
	if !strings.Contains(got, "2 partial failure(s)") {
		t.Errorf("expected count line; got %q", got)
	}
	if !strings.Contains(got, "permission denied") {
		t.Errorf("first error missing; got %q", got)
	}
	if !strings.Contains(got, "cross-device link") {
		t.Errorf("second error missing; got %q", got)
	}
}

func TestWriteCleanupWarnings_Empty(t *testing.T) {
	var buf bytes.Buffer
	writeCleanupWarnings(&buf, nil)
	if buf.Len() != 0 {
		t.Errorf("expected no output for empty warnings; got %q", buf.String())
	}
}

func TestWriteCleanupWarnings_Multiple(t *testing.T) {
	var buf bytes.Buffer
	warnings := []string{
		"remove tempdir /tmp/.overlord-init-abc: directory not empty",
	}
	writeCleanupWarnings(&buf, warnings)
	got := buf.String()
	if !strings.Contains(got, "post-commit cleanup warnings (1)") {
		t.Errorf("expected count header; got %q", got)
	}
	if !strings.Contains(got, "directory not empty") {
		t.Errorf("warning body missing; got %q", got)
	}
}
