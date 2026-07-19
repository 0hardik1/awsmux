package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeSTS points AWSMUX_AWS_BIN at a script that answers every call with a
// fixed sts get-caller-identity payload, and isolates the AWS config so
// ssoExpiry never reads the real ~/.aws.
func fakeSTS(t *testing.T, account, arn string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	script := filepath.Join(dir, "fake-aws")
	body := fmt.Sprintf("#!/bin/sh\nprintf '{\"UserId\":\"AIDATEST\",\"Account\":\"%s\",\"Arn\":\"%s\"}'\n", account, arn)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake aws: %v", err)
	}
	t.Setenv(AWSBinEnv, script)
}

func plannedDev() []Target {
	return []Target{{
		ID: "dev@us-east-1", Profile: "dev", Region: "us-east-1",
		AccountID: "111111111111", Principal: "arn:aws:iam::111111111111:user/dev",
	}}
}

func TestVerifyIdentitiesMatch(t *testing.T) {
	t.Setenv("AWSMUX_HOME", t.TempDir())
	fakeSTS(t, "111111111111", "arn:aws:iam::111111111111:user/dev")
	if err := VerifyIdentities(context.Background(), plannedDev()); err != nil {
		t.Errorf("matching live identity should verify, got: %v", err)
	}
}

func TestVerifyIdentitiesDetectsChange(t *testing.T) {
	t.Setenv("AWSMUX_HOME", t.TempDir())
	fakeSTS(t, "222222222222", "arn:aws:iam::222222222222:user/dev")
	if err := VerifyIdentities(context.Background(), plannedDev()); err == nil {
		t.Error("changed live identity must fail verification")
	}
}

func TestVerifyIdentitiesIgnoresCache(t *testing.T) {
	t.Setenv("AWSMUX_HOME", t.TempDir())
	// A fresh cache entry still vouches for the planned identity...
	saveIdentityCache(map[string]Identity{"dev": {
		Profile: "dev", AccountID: "111111111111",
		ARN: "arn:aws:iam::111111111111:user/dev", CheckedAt: time.Now().UTC(),
	}})
	// ...but live STS reports another account. Verification must trust only
	// the live answer, or a credential change inside IdentityCacheTTL could
	// redirect an approved plan.
	fakeSTS(t, "222222222222", "arn:aws:iam::222222222222:user/dev")
	if err := VerifyIdentities(context.Background(), plannedDev()); err == nil {
		t.Error("verification must bypass the identity cache")
	}
}
