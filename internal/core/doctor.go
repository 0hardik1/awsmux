package core

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// AGENT CONTRACT (doctor): keep the exported types and the Doctor signature
// exactly as written; add unexported helpers in this file freely.

// doctorVersionTimeout bounds the `aws --version` probe so a hung CLI can
// never wedge the doctor command.
const doctorVersionTimeout = 10 * time.Second

// DoctorFileReport diagnoses one shared AWS file consulted by discovery.
type DoctorFileReport struct {
	Path string `json:"path"`
	// EnvVar names the override env var when it decided Path.
	EnvVar   string `json:"env_var,omitempty"`
	Exists   bool   `json:"exists"`
	ParseErr string `json:"parse_error,omitempty"`
	// Profiles is how many profiles this file contributed (before merging).
	Profiles int `json:"profiles"`
}

// DoctorHomeReport diagnoses the awsmux state directory.
type DoctorHomeReport struct {
	Path string `json:"path,omitempty"`
	// EnvVar is "AWSMUX_HOME" when the override is in effect.
	EnvVar   string `json:"env_var,omitempty"`
	Writable bool   `json:"writable"`
	Err      string `json:"error,omitempty"`
}

// DoctorReport is the one-shot environment diagnostic behind `awsmux doctor`.
type DoctorReport struct {
	AWSCLIPath      string           `json:"aws_cli_path,omitempty"`
	AWSCLIVersion   string           `json:"aws_cli_version,omitempty"`
	AWSCLIErr       string           `json:"aws_cli_error,omitempty"`
	ConfigFile      DoctorFileReport `json:"config_file"`
	CredentialsFile DoctorFileReport `json:"credentials_file"`
	// ProfilesTotal is the merged profile count across both files;
	// ProfilesBoth counts profiles defined in both.
	ProfilesTotal int              `json:"profiles_total"`
	ProfilesBoth  int              `json:"profiles_both"`
	Home          DoctorHomeReport `json:"awsmux_home"`
	// OK is true when the aws CLI works, both files parse, the state
	// directory is writable, and at least one profile was discovered.
	OK bool `json:"ok"`
}

// Doctor runs every environment check. It never fails as a function:
// individual problems land in the report and clear OK. A missing config or
// credentials file is not a problem by itself; zero total profiles is,
// because awsmux is unusable until one exists.
func Doctor(ctx context.Context) DoctorReport {
	var r DoctorReport
	r.AWSCLIPath, r.AWSCLIVersion, r.AWSCLIErr = doctorAWSCLI(ctx)
	r.ConfigFile = doctorFile(awsConfigPath(), "AWS_CONFIG_FILE", "config", SourceConfig, profileSectionName)
	r.CredentialsFile = doctorFile(awsCredentialsPath(), "AWS_SHARED_CREDENTIALS_FILE", "credentials", SourceCredentials, credentialsSectionName)
	if profiles, err := LoadProfiles(); err == nil {
		r.ProfilesTotal = len(profiles)
		for _, p := range profiles {
			if p.Source == SourceBoth {
				r.ProfilesBoth++
			}
		}
	}
	r.Home = doctorHome()
	r.OK = r.AWSCLIErr == "" &&
		r.ConfigFile.ParseErr == "" && r.CredentialsFile.ParseErr == "" &&
		r.Home.Writable && r.ProfilesTotal > 0
	return r
}

// doctorAWSCLI locates the AWS CLI (honoring the AWSMUX_AWS_BIN test seam)
// and probes `aws --version` under a short timeout. AWS CLI v1 prints the
// version to stderr, so the probe reads combined output.
func doctorAWSCLI(ctx context.Context) (path, version, errMsg string) {
	bin := "aws"
	if override := strings.TrimSpace(os.Getenv(AWSBinEnv)); override != "" {
		bin = strings.Fields(override)[0]
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", "", err.Error()
	}

	vctx, cancel := context.WithTimeout(ctx, doctorVersionTimeout)
	defer cancel()
	out, err := awsExec(vctx, "--version").CombinedOutput()
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if err != nil {
		msg := line
		if msg == "" {
			msg = err.Error()
		}
		return resolved, "", "aws --version failed: " + msg
	}
	return resolved, line, ""
}

// doctorFile diagnoses one shared AWS file using the same parse that
// discovery uses, so the reported counts always match what LoadProfiles
// would see.
func doctorFile(path, envVar, label string, src ProfileSource, mapName func(string) (string, bool)) DoctorFileReport {
	rep := DoctorFileReport{Path: path}
	if os.Getenv(envVar) != "" {
		rep.EnvVar = envVar
	}
	if _, err := os.Stat(path); err == nil {
		rep.Exists = true
	}
	profiles, err := loadProfilesFromFile(path, label, src, mapName)
	if err != nil {
		rep.ParseErr = err.Error()
		return rep
	}
	rep.Profiles = len(profiles)
	return rep
}

// doctorHome resolves the state directory via Dir() (which creates it if
// needed) and proves writability with a temp-file probe that is removed
// immediately.
func doctorHome() DoctorHomeReport {
	var rep DoctorHomeReport
	if os.Getenv("AWSMUX_HOME") != "" {
		rep.EnvVar = "AWSMUX_HOME"
	}
	dir, err := Dir()
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	rep.Path = dir
	probe, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	probe.Close()
	os.Remove(probe.Name())
	rep.Writable = true
	return rep
}
