package cmd

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// fake-aws is the offline stand-in for the AWS CLI that powers `awsmux demo`.
// The engine invokes it exactly like the real binary (via AWSMUX_AWS_BIN), so
// every code path (STS preflight, classification, plan/approve/apply, MCP)
// runs unmodified against a fictional fleet. It never reads credentials and
// never touches the network.

type demoAccount struct {
	Profile string
	Account string
	Team    string
	Env     string
	Region  string
}

var demoTeams = []string{"payments", "search", "platform", "media", "data",
	"ml", "infra", "billing", "identity", "edge"}

var demoFleetRegions = []string{"us-east-1", "us-west-2", "eu-west-1"}

// demoFleet is 100 accounts (10 teams x prod/stage x 5 shards) plus one
// deliberate duplicate profile so --dedupe has something to find. Account
// IDs are stable: 200000000000 + index. In LocalStack mode each ID doubles
// as the profile's access key, which namespaces resources per account.
var demoFleet = buildDemoFleet()

func buildDemoFleet() []demoAccount {
	fleet := make([]demoAccount, 0, 101)
	for _, team := range demoTeams {
		for _, env := range []string{"prod", "stage"} {
			for shard := 1; shard <= 5; shard++ {
				fleet = append(fleet, demoAccount{
					Profile: fmt.Sprintf("%s-%s-%d", team, env, shard),
					Account: fmt.Sprintf("%012d", 200000000000+len(fleet)),
					Team:    team,
					Env:     env,
					Region:  demoFleetRegions[len(fleet)%len(demoFleetRegions)],
				})
			}
		}
	}
	dup := *demoByProfileIn(fleet, "platform-prod-1")
	dup.Profile = "admin-legacy"
	return append(fleet, dup)
}

func demoByProfileIn(fleet []demoAccount, profile string) *demoAccount {
	for i := range fleet {
		if fleet[i].Profile == profile {
			return &fleet[i]
		}
	}
	return nil
}

func demoByProfile(profile string) *demoAccount {
	return demoByProfileIn(demoFleet, profile)
}

func (a *demoAccount) roleARN() string {
	team := strings.ToUpper(a.Team[:1]) + a.Team[1:]
	return fmt.Sprintf("arn:aws:sts::%s:assumed-role/%sAdmin/demo", a.Account, team)
}

var fakeAWSCmd = &cobra.Command{
	Use:                "fake-aws",
	Hidden:             true,
	Short:              "Offline AWS CLI emulator backing `awsmux demo` (internal)",
	DisableFlagParsing: true,
	RunE:               runFakeAWS,
}

func init() {
	rootCmd.AddCommand(fakeAWSCmd)
}

func runFakeAWS(cmd *cobra.Command, args []string) error {
	var profile, region, query string
	var pos []string
	joined := strings.Join(args, " ")
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--profile" && i+1 < len(args):
			profile = args[i+1]
			i++
		case a == "--region" && i+1 < len(args):
			region = args[i+1]
			i++
		case a == "--query" && i+1 < len(args):
			query = args[i+1]
			i++
		case a == "--output" && i+1 < len(args):
			// Single-value flag that may precede the positional
			// service/operation; consume exactly one value.
			i++
		case strings.HasPrefix(a, "--"):
			// Any other flag appears after the positionals; swallow its
			// following value tokens greedily.
			for i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
			}
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) < 2 {
		fmt.Fprintln(os.Stderr, "usage: aws <service> <operation> [args]")
		return &ExitError{Code: 252}
	}
	service, operation := pos[0], pos[1]

	acct := demoByProfile(profile)
	if acct == nil {
		fmt.Fprintf(os.Stderr, "The config profile (%s) could not be found\n", profile)
		return &ExitError{Code: 255}
	}
	if region == "" {
		region = acct.Region
	}

	// Deterministic per-call latency so the demo feels like a real fleet.
	time.Sleep(time.Duration(120+hash32(profile+service+operation)%330) * time.Millisecond)

	// The one planted failure: data-prod-1 denies Lambda reads, so the
	// synthetic demo always has an access_denied row to show the taxonomy.
	if profile == "data-prod-1" && service == "lambda" {
		fmt.Fprintf(os.Stderr,
			"An error occurred (AccessDeniedException) when calling the ListFunctions operation: "+
				"User: %s is not authorized to perform: lambda:ListFunctions on resource: * "+
				"because no identity-based policy allows the lambda:ListFunctions action\n",
			acct.roleARN())
		return &ExitError{Code: 254}
	}

	payload := applyQuery(fakeResponse(acct, region, service, operation, joined), query)
	out, err := json.MarshalIndent(payload, "", "    ")
	if err != nil {
		return fmt.Errorf("encode fake response: %w", err)
	}
	fmt.Println(string(out))
	return nil
}

func fakeResponse(a *demoAccount, region, service, operation, joined string) any {
	prod := a.Env == "prod"
	switch service + " " + operation {
	case "sts get-caller-identity":
		return map[string]string{
			"UserId":  fmt.Sprintf("AROADEMO%08X:demo", hash32(a.Profile)),
			"Account": a.Account,
			"Arn":     a.roleARN(),
		}
	case "ec2 describe-vpcs":
		return map[string]any{"Vpcs": []any{map[string]any{
			"VpcId":     fakeID("vpc", a.Profile),
			"CidrBlock": fmt.Sprintf("10.%d.0.0/16", hash32(a.Profile)%200),
			"State":     "available",
			"IsDefault": false,
			"Tags":      nameTags(a.Team + "-" + a.Env),
		}}}
	case "ec2 describe-instances":
		n := 0
		if prod {
			n = 3
		} else if a.Env == "stage" {
			n = 1
		}
		instances := []any{}
		for i := 0; i < n; i++ {
			instances = append(instances, map[string]any{
				"InstanceId":   fakeID("i", fmt.Sprintf("%s%d", a.Profile, i)),
				"InstanceType": []string{"m5.large", "r5.xlarge", "c5.large"}[i%3],
				"State":        map[string]string{"Name": "running"},
				"Tags":         nameTags(fmt.Sprintf("%s-%s-%d", a.Team, []string{"api", "db", "worker"}[i%3], i+1)),
			})
		}
		return map[string]any{"Reservations": []any{map[string]any{"Instances": instances}}}
	case "ec2 describe-security-groups":
		// The scenario centerpiece: only payments-prod in us-east-1 has the
		// bastion group open to the world. Filtered queries find exactly it.
		filtered := strings.Contains(joined, "--filters") || strings.Contains(joined, "0.0.0.0/0")
		groups := []any{}
		if a.Profile == "payments-prod-1" {
			groups = append(groups, map[string]any{
				"GroupId":   "sg-0a1b2c3d",
				"GroupName": "legacy-bastion",
				"VpcId":     fakeID("vpc", a.Profile),
				"IpPermissions": []any{map[string]any{
					"IpProtocol": "tcp", "FromPort": 22, "ToPort": 22,
					"IpRanges": []any{map[string]string{"CidrIp": "0.0.0.0/0"}},
				}},
			})
		}
		if !filtered {
			groups = append(groups, map[string]any{
				"GroupId":   fakeID("sg", a.Profile),
				"GroupName": "default",
				"VpcId":     fakeID("vpc", a.Profile),
			})
		}
		return map[string]any{"SecurityGroups": groups}
	case "ec2 revoke-security-group-ingress":
		return map[string]any{"Return": true}
	case "lambda list-functions":
		fns := []any{}
		names := []string{a.Team + "-events"}
		if prod {
			names = append(names, a.Team+"-cron", a.Team+"-alerts")
		}
		for _, n := range names {
			fns = append(fns, map[string]any{
				"FunctionName": n,
				"Runtime":      "python3.12",
				"MemorySize":   256,
			})
		}
		return map[string]any{"Functions": fns}
	case "eks list-clusters":
		if prod {
			return map[string]any{"clusters": []string{a.Team + "-primary"}}
		}
		return map[string]any{"clusters": []string{}}
	case "ecs list-clusters":
		if !prod {
			return map[string]any{"clusterArns": []string{}}
		}
		return map[string]any{"clusterArns": []string{
			fmt.Sprintf("arn:aws:ecs:%s:%s:cluster/%s-%s", region, a.Account, a.Team, a.Env),
		}}
	case "s3api list-buckets":
		return map[string]any{"Buckets": []any{
			map[string]string{"Name": fmt.Sprintf("%s-%s-artifacts", a.Team, a.Env), "CreationDate": "2024-03-11T00:00:00Z"},
			map[string]string{"Name": fmt.Sprintf("%s-%s-logs", a.Team, a.Env), "CreationDate": "2024-03-11T00:00:00Z"},
		}}
	case "iam list-users":
		return map[string]any{"Users": []any{
			map[string]string{"UserName": "deploy-bot", "CreateDate": "2023-08-01T00:00:00Z"},
		}}
	case "ssm put-parameter":
		return map[string]any{"Version": 1, "Tier": "Standard"}
	case "ssm get-parameter":
		return map[string]any{"Parameter": map[string]any{"Name": "demo", "Value": "demo", "Version": 1}}
	case "ssm delete-parameter":
		return map[string]any{}
	}
	// Graceful fallback so any command still round-trips in demo mode.
	return map[string]string{"DemoNote": fmt.Sprintf(
		"no canned demo data for `aws %s %s`; rich responses exist for: sts, ec2, lambda, eks, ecs, s3api, iam, ssm",
		service, operation)}
}

// applyQuery evaluates the small JMESPath subset the demo docs use: dotted
// key paths, "Key[]" projections (flattened like real JMESPath), and
// "{alias:Key}" multiselect hashes. Anything it cannot parse returns the
// payload unchanged, which is safer than failing the demo.
func applyQuery(v any, expr string) any {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return v
	}
	// Normalize the typed payload into generic maps/slices first.
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return v
	}
	return evalPath(generic, strings.Split(expr, "."))
}

func evalPath(v any, tokens []string) any {
	if v == nil || len(tokens) == 0 {
		return v
	}
	tok, rest := strings.TrimSpace(tokens[0]), tokens[1:]
	switch {
	case strings.HasPrefix(tok, "{") && strings.HasSuffix(tok, "}"):
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		out := map[string]any{}
		for _, pair := range strings.Split(strings.Trim(tok, "{}"), ",") {
			kv := strings.SplitN(pair, ":", 2)
			if len(kv) != 2 {
				return v
			}
			out[strings.TrimSpace(kv[0])] = evalPath(m, strings.Split(strings.TrimSpace(kv[1]), "."))
		}
		return evalPath(out, rest)
	case strings.HasSuffix(tok, "[]"):
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		arr, ok := m[strings.TrimSuffix(tok, "[]")].([]any)
		if !ok {
			return nil
		}
		nested := false
		for _, t := range rest {
			if strings.HasSuffix(strings.TrimSpace(t), "[]") {
				nested = true
			}
		}
		out := []any{}
		for _, elem := range arr {
			r := evalPath(elem, rest)
			if r == nil {
				continue
			}
			if sub, ok := r.([]any); ok && nested {
				out = append(out, sub...)
			} else {
				out = append(out, r)
			}
		}
		return out
	default:
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		return evalPath(m[tok], rest)
	}
}

func nameTags(name string) []any {
	return []any{map[string]string{"Key": "Name", "Value": name}}
}

func hash32(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

func fakeID(prefix, seed string) string {
	h := fnv.New64a()
	h.Write([]byte(seed))
	return fmt.Sprintf("%s-0%016x", prefix, h.Sum64())
}
