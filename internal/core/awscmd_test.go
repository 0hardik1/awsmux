package core

import (
	"os"
	"path/filepath"
	"testing"
)

// swapFallbacks substitutes the well-known-location list for one test.
func swapFallbacks(t *testing.T, candidates []string) {
	t.Helper()
	old := awsBinFallbacks
	awsBinFallbacks = candidates
	t.Cleanup(func() { awsBinFallbacks = old })
}

func writeFakeAWS(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveAWSBinOverrideWins(t *testing.T) {
	t.Setenv(AWSBinEnv, "/x/stub-aws --endpoint http://localhost")
	bin, lead := resolveAWSBin()
	if bin != "/x/stub-aws" {
		t.Fatalf("bin = %q, want /x/stub-aws", bin)
	}
	if len(lead) != 2 || lead[0] != "--endpoint" || lead[1] != "http://localhost" {
		t.Fatalf("leading args = %v, want [--endpoint http://localhost]", lead)
	}
}

func TestResolveAWSBinPrefersPATH(t *testing.T) {
	t.Setenv(AWSBinEnv, "")
	dir := t.TempDir()
	writeFakeAWS(t, dir, "aws")
	t.Setenv("PATH", dir)
	swapFallbacks(t, []string{writeFakeAWS(t, t.TempDir(), "aws")})

	if bin, _ := resolveAWSBin(); bin != "aws" {
		t.Fatalf("bin = %q, want bare \"aws\" when PATH resolves it", bin)
	}
}

func TestResolveAWSBinFallsBackToKnownLocation(t *testing.T) {
	t.Setenv(AWSBinEnv, "")
	t.Setenv("PATH", t.TempDir()) // empty dir: no aws on PATH

	fake := writeFakeAWS(t, t.TempDir(), "aws")
	swapFallbacks(t, []string{filepath.Join(t.TempDir(), "missing"), fake})

	bin, lead := resolveAWSBin()
	if bin != fake {
		t.Fatalf("bin = %q, want fallback %q", bin, fake)
	}
	if len(lead) != 0 {
		t.Fatalf("leading args = %v, want none", lead)
	}
}

func TestResolveAWSBinNothingFound(t *testing.T) {
	t.Setenv(AWSBinEnv, "")
	t.Setenv("PATH", t.TempDir())
	swapFallbacks(t, []string{filepath.Join(t.TempDir(), "missing")})

	if bin, _ := resolveAWSBin(); bin != "aws" {
		t.Fatalf("bin = %q, want \"aws\" so exec reports the standard error", bin)
	}
}
