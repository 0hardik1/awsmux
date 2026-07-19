package core

import (
	"reflect"
	"testing"
)

func TestBuildCommand(t *testing.T) {
	tests := []struct {
		name      string
		target    Target
		service   string
		operation string
		args      []string
		want      []string
	}{
		{
			name:      "region omitted when empty",
			target:    Target{Profile: "dev"},
			service:   "ec2",
			operation: "describe-instances",
			want:      []string{"--profile", "dev", "--output", "json", "ec2", "describe-instances"},
		},
		{
			name:      "region included when set",
			target:    Target{Profile: "prod", Region: "eu-west-1"},
			service:   "s3api",
			operation: "list-buckets",
			want:      []string{"--profile", "prod", "--region", "eu-west-1", "--output", "json", "s3api", "list-buckets"},
		},
		{
			name:      "extra args appended after operation",
			target:    Target{Profile: "dev"},
			service:   "ec2",
			operation: "describe-instances",
			args:      []string{"--max-items", "5"},
			want:      []string{"--profile", "dev", "--output", "json", "ec2", "describe-instances", "--max-items", "5"},
		},
		{
			name:      "existing --output not duplicated",
			target:    Target{Profile: "dev"},
			service:   "ec2",
			operation: "describe-instances",
			args:      []string{"--output", "text"},
			want:      []string{"--profile", "dev", "ec2", "describe-instances", "--output", "text"},
		},
		{
			name:      "existing --output=text not duplicated",
			target:    Target{Profile: "dev"},
			service:   "ec2",
			operation: "describe-instances",
			args:      []string{"--output=text"},
			want:      []string{"--profile", "dev", "ec2", "describe-instances", "--output=text"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildCommand(tt.target, tt.service, tt.operation, tt.args)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("BuildCommand = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateArgs(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--max-items", "5"},
		{"--output", "text"},
		{"--profile-not-really", "x"}, // only exact flag or = form is reserved
	} {
		if err := ValidateArgs(args); err != nil {
			t.Errorf("ValidateArgs(%v) = %v, want nil", args, err)
		}
	}
	for _, args := range [][]string{
		{"--profile", "other"},
		{"--profile=other"},
		{"--max-items", "5", "--region", "us-west-2"},
		{"--region=us-west-2"},
	} {
		if err := ValidateArgs(args); err == nil {
			t.Errorf("ValidateArgs(%v) = nil, want error", args)
		}
	}
}

func TestSummarize(t *testing.T) {
	results := []TargetResult{
		{Status: StatusSuccess},
		{Status: StatusSuccess},
		{Status: StatusError},
		{Status: StatusTimeout},
		{Status: StatusAccessDenied},
		{Status: StatusExpiredCreds},
		{Status: StatusSkipped},
	}
	got := Summarize(results)
	want := Summary{
		Total:             7,
		Succeeded:         2,
		Failed:            4, // error + timeout + access denied + credential expired
		AccessDenied:      1,
		CredentialExpired: 1,
		TimedOut:          1,
		Skipped:           1,
	}
	if got != want {
		t.Errorf("Summarize = %+v, want %+v", got, want)
	}

	if got := Summarize(nil); got != (Summary{}) {
		t.Errorf("Summarize(nil) = %+v, want zero", got)
	}
}

func TestExecutionExitCode(t *testing.T) {
	tests := []struct {
		name string
		e    Execution
		want int
	}{
		{"all succeeded", Execution{Summary: Summary{Total: 2, Succeeded: 2}}, ExitOK},
		{"some failed", Execution{Summary: Summary{Total: 2, Succeeded: 1, Failed: 1}}, ExitCommandFailed},
		{"skipped counts as not ok", Execution{Summary: Summary{Total: 2, Succeeded: 1, Skipped: 1}}, ExitCommandFailed},
		{"stopped wins", Execution{Stopped: true, Summary: Summary{Total: 2, Failed: 1, Skipped: 1}}, ExitStoppedByThreshold},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.e.ExitCode(); got != tt.want {
				t.Errorf("ExitCode = %d, want %d", got, tt.want)
			}
		})
	}
}
