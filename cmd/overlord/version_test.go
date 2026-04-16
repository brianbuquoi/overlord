package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCmd(t *testing.T) {
	cmd := versionCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("versionCmd execute: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, overlordVersion) {
		t.Errorf("version output missing version %q; got %q", overlordVersion, got)
	}
	if !strings.HasPrefix(got, "overlord ") {
		t.Errorf("expected output to start with 'overlord '; got %q", got)
	}
}

func TestRootCommandVersionFlag(t *testing.T) {
	root := rootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("--version execute: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, overlordVersion) {
		t.Errorf("--version output missing version %q; got %q", overlordVersion, got)
	}
}
