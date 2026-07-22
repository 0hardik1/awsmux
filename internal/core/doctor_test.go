package core

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// stubAWSCLI writes an executable stand-in for the aws CLI that prints a
// version banner, and points AWSMUX_AWS_BIN at it.
func stubAWSCLI(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub")
	}
	stub := filepath.Join(t.TempDir(), "aws")
	script := "#!/bin/sh\necho 'aws-cli/2.17.0 Python/3.11.8 Darwin/23.0.0'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv(AWSBinEnv, stub)
}

func TestDoctorHealthy(t *testing.T) {
	isolateSharedFiles(t)
	stubAWSCLI(t)
	t.Setenv("AWSMUX_HOME", t.TempDir())
	cfg := writeSharedFile(t, "config", `[default]
region = us-east-1

[profile staging]
region = eu-west-1
`)
	creds := writeSharedFile(t, "credentials", `[staging]
aws_access_key_id = AKIAEXAMPLE

[credsonly]
aws_access_key_id = AKIAEXAMPLE
`)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)

	r := Doctor(context.Background())
	if r.AWSCLIErr != "" {
		t.Fatalf("AWSCLIErr = %q, want empty", r.AWSCLIErr)
	}
	if r.AWSCLIVersion != "aws-cli/2.17.0 Python/3.11.8 Darwin/23.0.0" {
		t.Errorf("AWSCLIVersion = %q", r.AWSCLIVersion)
	}
	if r.AWSCLIPath == "" {
		t.Error("AWSCLIPath is empty")
	}
	if r.ConfigFile.Path != cfg || !r.ConfigFile.Exists || r.ConfigFile.EnvVar != "AWS_CONFIG_FILE" {
		t.Errorf("ConfigFile = %+v", r.ConfigFile)
	}
	if r.ConfigFile.Profiles != 2 {
		t.Errorf("ConfigFile.Profiles = %d, want 2", r.ConfigFile.Profiles)
	}
	if r.CredentialsFile.Profiles != 2 || r.CredentialsFile.EnvVar != "AWS_SHARED_CREDENTIALS_FILE" {
		t.Errorf("CredentialsFile = %+v", r.CredentialsFile)
	}
	if r.ProfilesTotal != 3 {
		t.Errorf("ProfilesTotal = %d, want 3", r.ProfilesTotal)
	}
	if r.ProfilesBoth != 1 {
		t.Errorf("ProfilesBoth = %d, want 1", r.ProfilesBoth)
	}
	if r.Home.EnvVar != "AWSMUX_HOME" || !r.Home.Writable || r.Home.Path == "" {
		t.Errorf("Home = %+v", r.Home)
	}
	if !r.OK {
		t.Errorf("OK = false, want true; report %+v", r)
	}
}

func TestDoctorMissingCLI(t *testing.T) {
	isolateSharedFiles(t)
	t.Setenv("AWSMUX_HOME", t.TempDir())
	t.Setenv(AWSBinEnv, filepath.Join(t.TempDir(), "no-such-aws"))

	r := Doctor(context.Background())
	if r.AWSCLIErr == "" {
		t.Error("AWSCLIErr is empty, want a lookup failure")
	}
	if r.OK {
		t.Error("OK = true, want false")
	}
}

func TestDoctorParseFailure(t *testing.T) {
	isolateSharedFiles(t)
	stubAWSCLI(t)
	t.Setenv("AWSMUX_HOME", t.TempDir())
	// A directory opens fine but fails on read: the one way the lenient
	// parser can error out.
	t.Setenv("AWS_CONFIG_FILE", t.TempDir())

	r := Doctor(context.Background())
	if r.ConfigFile.ParseErr == "" {
		t.Error("ConfigFile.ParseErr is empty, want a read failure")
	}
	if r.OK {
		t.Error("OK = true, want false")
	}
}

func TestDoctorZeroProfiles(t *testing.T) {
	isolateSharedFiles(t)
	stubAWSCLI(t)
	t.Setenv("AWSMUX_HOME", t.TempDir())

	r := Doctor(context.Background())
	if r.ProfilesTotal != 0 {
		t.Errorf("ProfilesTotal = %d, want 0", r.ProfilesTotal)
	}
	if r.ConfigFile.Exists || r.CredentialsFile.Exists {
		t.Errorf("files should not exist: %+v %+v", r.ConfigFile, r.CredentialsFile)
	}
	if r.OK {
		t.Error("OK = true, want false")
	}
}
