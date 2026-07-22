// Command fleet provisions the LocalStack-backed test fleet used by
// `make fleet-up`, `make e2e`, and the CI e2e job. It is a dev tool, not part
// of the awsmux binary: stdlib only, invoked via `go run ./scripts/fleet`.
//
//	go run ./scripts/fleet up   [-teams N] [-shards N] [-dir PATH]
//	go run ./scripts/fleet down
//	go run ./scripts/fleet env  [-teams N] [-shards N] [-dir PATH]
//
// `up` boots (or reuses) the pinned LocalStack container, generates an AWS
// config and credentials file describing the fleet, isolates awsmux state
// under <dir>/home, seeds a few storyline resources once per container, and
// writes <dir>/env.sh for the caller to source. `down` removes the container.
// `env` prints the export lines without touching Docker.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Pinned to the last fully license-free community line; newer
	// localstack/localstack images exit without a LOCALSTACK_AUTH_TOKEN.
	localstackImage     = "localstack/localstack:3.8"
	localstackContainer = "awsmux-localstack"
	localstackEndpoint  = "http://localhost:4566"
)

type account struct {
	Profile string
	Account string
	Team    string
	Env     string
	Region  string
}

var teams = []string{"payments", "search", "platform", "media", "data",
	"ml", "infra", "billing", "identity", "edge"}

var regions = []string{"us-east-1", "us-west-2", "eu-west-1"}

// buildFleet is nTeams x {prod,stage} x nShards accounts plus one deliberate
// duplicate profile so `--dedupe` has something to find. Account IDs are
// stable: 200000000000 + index. Each ID doubles as the profile's access key,
// which is how LocalStack namespaces resources per account.
func buildFleet(nTeams, nShards int) []account {
	fleet := make([]account, 0, nTeams*2*nShards+1)
	for _, team := range teams[:nTeams] {
		for _, env := range []string{"prod", "stage"} {
			for shard := 1; shard <= nShards; shard++ {
				fleet = append(fleet, account{
					Profile: fmt.Sprintf("%s-%s-%d", team, env, shard),
					Account: fmt.Sprintf("%012d", 200000000000+len(fleet)),
					Team:    team,
					Env:     env,
					Region:  regions[len(fleet)%len(regions)],
				})
			}
		}
	}
	dup := fleet[0]
	if a := byProfile(fleet, "platform-prod-1"); a != nil {
		dup = *a
	}
	dup.Profile = "admin-legacy"
	return append(fleet, dup)
}

func byProfile(fleet []account, profile string) *account {
	for i := range fleet {
		if fleet[i].Profile == profile {
			return &fleet[i]
		}
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fleet <up|down|env> [-teams N] [-shards N] [-dir PATH]")
		os.Exit(2)
	}
	sub, rest := os.Args[1], os.Args[2:]

	fs := flag.NewFlagSet("fleet "+sub, flag.ExitOnError)
	nTeams := fs.Int("teams", len(teams), fmt.Sprintf("number of teams, 1..%d", len(teams)))
	nShards := fs.Int("shards", 5, "shards per team and environment")
	dir := fs.String("dir", filepath.Join(".tmp", "fleet"), "output directory for config, credentials, and state")
	_ = fs.Parse(rest)
	if *nTeams < 1 || *nTeams > len(teams) || *nShards < 1 {
		fmt.Fprintf(os.Stderr, "fleet: -teams must be 1..%d and -shards >= 1\n", len(teams))
		os.Exit(2)
	}

	ctx := context.Background()
	var err error
	switch sub {
	case "up":
		err = up(ctx, *dir, *nTeams, *nShards)
	case "down":
		err = down(ctx)
	case "env":
		err = printEnv(os.Stdout, *dir, buildFleet(*nTeams, *nShards))
	default:
		fmt.Fprintf(os.Stderr, "fleet: unknown subcommand %q (want up, down, or env)\n", sub)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet %s: %s\n", sub, err)
		os.Exit(1)
	}
}

func up(ctx context.Context, dir string, nTeams, nShards int) error {
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		return errors.New("docker is not available; the test fleet needs Docker for LocalStack")
	}
	if _, err := exec.LookPath("aws"); err != nil {
		return errors.New("the aws CLI is required to seed the fleet; install AWS CLI v2")
	}
	cid, err := ensureLocalStack(ctx)
	if err != nil {
		return err
	}

	fleet := buildFleet(nTeams, nShards)
	if err := writeFleetFiles(dir, fleet); err != nil {
		return err
	}
	if err := seed(ctx, dir, fleet, cid); err != nil {
		fmt.Fprintf(os.Stderr, "fleet up: warning: seeding: %s\n", err)
	}

	fmt.Fprintf(os.Stderr, "fleet up: %d profiles ready\n", len(fleet))
	fmt.Fprintf(os.Stderr, "next: source %s && ./bin/awsmux targets\n", filepath.Join(dir, "env.sh"))
	return nil
}

func down(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", localstackContainer).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") {
		return fmt.Errorf("remove %s: %s", localstackContainer, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureLocalStack makes sure the pinned LocalStack container is running and
// healthy, starting it (and pulling the image on first use) if needed, and
// returns the running container's id so seeding can tell a fresh container
// from the one it already planted resources in.
func ensureLocalStack(ctx context.Context) (string, error) {
	out, _ := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", localstackContainer).Output()
	if strings.TrimSpace(string(out)) != "true" {
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", localstackContainer).Run()
		fmt.Fprintf(os.Stderr, "fleet up: starting LocalStack (%s); the first run pulls the image...\n", localstackImage)
		if out, err := exec.CommandContext(ctx, "docker", "run", "-d",
			"--name", localstackContainer, "-p", "4566:4566", localstackImage).CombinedOutput(); err != nil {
			return "", fmt.Errorf("start LocalStack: %s", strings.TrimSpace(string(out)))
		}
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, localstackEndpoint+"/_localstack/health", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				id, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.Id}}", localstackContainer).Output()
				if err != nil {
					return "", fmt.Errorf("inspect LocalStack container: %w", err)
				}
				return strings.TrimSpace(string(id)), nil
			}
		}
		time.Sleep(time.Second)
	}
	return "", errors.New("LocalStack did not become healthy in 3 minutes; check `docker logs " + localstackContainer + "`")
}

// writeFleetFiles generates the AWS config and credentials files, the state
// home, and env.sh. Each profile's access key is its 12-digit account ID,
// which LocalStack uses to namespace resources per account; endpoint_url
// points the real aws CLI at the emulator.
func writeFleetFiles(dir string, fleet []account) error {
	if err := os.MkdirAll(filepath.Join(dir, "home"), 0o700); err != nil {
		return err
	}
	// Reprovisioning may change the profile-to-account mapping (different
	// -teams/-shards move the admin-legacy duplicate), so a warm identity
	// cache would hand awsmux stale identities for up to 5 minutes.
	_ = os.Remove(filepath.Join(dir, "home", "identity-cache.json"))
	var cfg, creds strings.Builder
	cfg.WriteString("# Generated by `make fleet-up`. This fleet is fictional.\n")
	for _, a := range fleet {
		fmt.Fprintf(&cfg, "[profile %s]\nregion = %s\nendpoint_url = %s\n\n",
			a.Profile, a.Region, localstackEndpoint)
		fmt.Fprintf(&creds, "[%s]\naws_access_key_id = %s\naws_secret_access_key = test\n\n",
			a.Profile, a.Account)
	}
	if err := os.WriteFile(filepath.Join(dir, "aws-config"), []byte(cfg.String()), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "aws-credentials"), []byte(creds.String()), 0o600); err != nil {
		return err
	}

	var env strings.Builder
	if err := printEnv(&env, dir, fleet); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "env.sh"), []byte(env.String()), 0o600)
}

// printEnv writes the export lines (absolute paths) that point awsmux and the
// aws CLI at the fleet. AWSMUX_FLEET_SIZE lets scripts/e2e.sh assert the
// exact target count whatever -teams/-shards were used.
func printEnv(w interface{ Write([]byte) (int, error) }, dir string, fleet []account) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "export AWS_CONFIG_FILE=%s\n", filepath.Join(abs, "aws-config"))
	fmt.Fprintf(w, "export AWS_SHARED_CREDENTIALS_FILE=%s\n", filepath.Join(abs, "aws-credentials"))
	fmt.Fprintf(w, "export AWSMUX_HOME=%s\n", filepath.Join(abs, "home"))
	fmt.Fprintf(w, "export AWSMUX_FLEET_SIZE=%d\n", len(fleet))
	return nil
}

// seed plants a few storyline resources once per container, most importantly
// the payments-prod-1 security group that is open to the world. The marker
// records which container was seeded: a recreated container comes up empty,
// so a stale marker must trigger a reseed, not skip it.
func seed(ctx context.Context, dir string, fleet []account, containerID string) error {
	marker := filepath.Join(dir, ".seeded")
	if prev, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(prev)) == containerID {
		return nil
	}
	fmt.Fprintln(os.Stderr, "fleet up: seeding storyline resources (one time per container)...")
	calls := []struct {
		profile string
		argv    []string
	}{
		{"payments-prod-1", []string{"ec2", "create-security-group",
			"--group-name", "legacy-bastion", "--description", "fleet: accidentally open to the world"}},
		{"payments-prod-1", []string{"ec2", "authorize-security-group-ingress",
			"--group-name", "legacy-bastion", "--protocol", "tcp", "--port", "22", "--cidr", "0.0.0.0/0"}},
		{"payments-prod-1", []string{"ssm", "put-parameter", "--name", "/payments/deploy-ring",
			"--value", "ring-2", "--type", "String", "--overwrite"}},
		{"billing-prod-1", s3CreateBucketArgs(fleet, "billing-prod-1", "billing-prod-1-invoices")},
		{"platform-prod-1", []string{"ssm", "put-parameter", "--name", "/platform/feature-flags",
			"--value", "canary=on", "--type", "String", "--overwrite"}},
	}
	env := []string{
		"AWS_CONFIG_FILE=" + filepath.Join(dir, "aws-config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(dir, "aws-credentials"),
	}
	var firstErr error
	for _, c := range calls {
		// A shrunken fleet (-teams/-shards below the defaults) may not
		// contain a storyline profile; skip its seed calls.
		if byProfile(fleet, c.profile) == nil {
			continue
		}
		argv := append([]string{"--profile", c.profile, "--output", "json"}, c.argv...)
		cmd := exec.CommandContext(ctx, "aws", argv...)
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil && firstErr == nil {
			msg := strings.TrimSpace(string(out))
			// A reseed against a still-running LocalStack finds the
			// storyline resources already in place; that is success.
			if strings.Contains(msg, "Duplicate") || strings.Contains(msg, "AlreadyExists") ||
				strings.Contains(msg, "already exists") || strings.Contains(msg, "AlreadyOwnedByYou") {
				continue
			}
			firstErr = fmt.Errorf("aws %s (%s): %s", strings.Join(c.argv[:2], " "), c.profile, msg)
		}
	}
	if err := os.WriteFile(marker, []byte(containerID+"\n"), 0o600); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// s3CreateBucketArgs handles the S3 quirk that any region except us-east-1
// must state a LocationConstraint.
func s3CreateBucketArgs(fleet []account, profile, bucket string) []string {
	argv := []string{"s3api", "create-bucket", "--bucket", bucket}
	if a := byProfile(fleet, profile); a != nil && a.Region != "us-east-1" {
		argv = append(argv, "--create-bucket-configuration", "LocationConstraint="+a.Region)
	}
	return argv
}
