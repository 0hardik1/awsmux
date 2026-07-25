# Org-aware discovery

Date: 2026-07-25
Status: approved, ready for implementation planning

## Problem

Target discovery reads only the AWS shared config and credentials files, so
awsmux can address exactly the accounts someone hand-wrote into
`~/.aws/config`. Selection over those profiles is by name glob
(`--profiles 'prod-*'`), which is the one signal the safety model explicitly
distrusts everywhere else: invariant 5 says identity is never inferred from
profile names, yet a profile name is currently the only way to choose what a
fan-out hits.

Meanwhile the authoritative picture of which accounts exist, and which of them
are production, lives in AWS Organizations: the OU tree and account tags.
awsmux cannot see any of it.

## What this feature is, and what it is not

It is **org-aware selection**. AWS Organizations is enumerated to learn the
account and OU tree, and that structure becomes a selector over the profiles
you can already reach. Selection joins on the account ID that STS verified
during preflight, never on a profile name.

It is **not** org-aware credential provisioning. awsmux does not gain the
ability to reach an account that has no local profile. Fan-out execution is
untouched: still one `aws --profile <p>` child process per target, still the
AWS CLI resolving credentials.

The role passed to `--org-role` is assumed for **enumeration only**, so that
the Organizations API can be called from a principal that has
`organizations:ListAccounts` even when your default credentials do not.

Accounts that the org lists but that you cannot reach are reported rather than
hidden. That makes `awsmux targets` a coverage report, which is the more
valuable half of this feature: knowing that an OU holds 140 accounts and you
have profiles for 37 is the fact you want before a fan-out, not after.

## Data model

New file `internal/core/org.go`, shaped deliberately like `identity.go`: a
cache file with a TTL, shell-out through the `awscmd` seam, bounded
concurrency, and only successful lookups cached.

```go
// OrgCacheTTL bounds how long an enumerated org tree is trusted.
const OrgCacheTTL = 1 * time.Hour

// OrgAccount is one account as AWS Organizations reports it.
type OrgAccount struct {
    ID     string            // 12-digit account ID; the join key
    Name   string            // Organizations account name, not the profile name
    Status string            // ACTIVE | SUSPENDED | PENDING_CLOSURE
    OUPath string            // "" at root, else a slash path like "eng/prod"
    Tags   map[string]string
}

// Org is an enumerated organization, indexed by account ID.
type Org struct {
    MasterAccountID string
    Accounts        map[string]OrgAccount
    FetchedAt       time.Time
}

// OrgOptions controls enumeration.
type OrgOptions struct {
    Profile    string // base profile the organizations calls run under
    AssumeRole string // role ARN assumed for enumeration; "" uses Profile directly
    Refresh    bool   // bypass the cache
    WantTags   bool   // fetch per-account tags (one extra API call per account)
}

func LoadOrg(ctx context.Context, opts OrgOptions) (*Org, error)
```

`Selector` (in `types.go`) gains:

```go
OU          []string          // OU path globs, e.g. "eng/prod", "eng/*"
AccountTags map[string]string // every pair must match
OrgRole     string            // role ARN assumed to enumerate
OrgProfile  string            // base profile for the organizations calls
OrgRefresh  bool
```

`OrgOptions` is derived from `Selector`: `Profile` from `OrgProfile`,
`AssumeRole` from `OrgRole`, `Refresh` from `OrgRefresh`, and `WantTags` from
`len(AccountTags) > 0`, so the per-account tag calls happen only when a tag
filter was actually asked for.

`Target` gains two fields, both `omitempty`:

```go
OrgAccountName string `json:"org_account_name,omitempty"`
OUPath         string `json:"ou_path,omitempty"`
```

Cache file: `$AWSMUX_HOME/org-cache.json`, mode 0600, written atomically with
the existing `writeJSONAtomic`. It stores the enumerated tree only. It never
stores credentials.

## Enumeration

1. When `AssumeRole` is set, shell out to
   `aws sts assume-role --role-arn <arn> --role-session-name awsmux-org-discovery`
   through the base profile. The returned credentials are passed as
   environment to the `organizations` calls that follow, and to nothing else.
2. `describe-organization` for `MasterAccountID`.
3. Walk the tree: `list-roots`, then recurse
   `list-organizational-units-for-parent` and `list-accounts-for-parent`,
   accumulating the slash-joined OU path for each account. The flat
   `list-accounts` call is one request instead of a walk but returns no OU
   membership, so it cannot serve this feature.
4. When `WantTags` is set, `list-tags-for-resource` per account, at the same
   bounded concurrency preflight uses.
5. Cache the result.

Tags cost one API call per account, so `WantTags` is set only when
`--account-tag` is actually used. A 500-account org therefore pays roughly one
call per OU for structure, and 500 more only if tags were requested. That cost
is why `OrgCacheTTL` is an hour rather than the five minutes identity uses:
org structure changes weekly at most, while agents poll `list_aws_targets`
constantly.

## OU path matching

An OU pattern matches an account's OU path when the pattern matches a **prefix
of the path, segment by segment**, with `path.Match` glob semantics applied
within each segment. So:

| Pattern | Matches | Does not match |
| --- | --- | --- |
| `eng/prod` | `eng/prod`, `eng/prod/db` | `eng/dev`, `platform/prod` |
| `eng/*` | `eng/prod`, `eng/prod/db`, `eng/dev` | `platform/prod` |
| `*` | every account in the org | nothing |

This makes `--ou eng/prod` recursive: it selects the prod OU and everything
nested beneath it. That is what "in the prod OU" means organizationally, since
a child OU inherits its parent's SCPs and is genuinely inside it. The
alternative reading, exact-path-only, would silently miss accounts in a
sub-OU, and silently missing accounts is the worse failure for a fleet tool.

Recursion is not a blast-radius hazard here: every matched account is still
STS-verified, still enumerated in full in the plan the human reads, and still
gated by `RequiresApproval`. The tradeoff accepted is that "this OU but not
its children" is not expressible. If that turns out to be wanted, an
`--ou-exact` variant can be added later without changing these semantics.

Note that plain `path.Match` on the full path would not do this: `*` does not
cross `/` in Go's implementation, so `eng/*` would match `eng/prod` but not
`eng/prod/db`. Segment-prefix matching must be implemented explicitly rather
than by handing the whole path to `MatchGlob`.

## Resolution flow

`ResolveTargets` gains one stage, positioned after preflight because the join
key is the verified account ID:

```
LoadProfiles
  -> profile glob filter (--profiles / --exclude)
  -> region expansion
  -> Preflight              (STS fills AccountID, Principal)
  -> MarkDuplicates
  -> org filter             (new; only when OU or AccountTags is set)
        org := LoadOrg(...)
        keep t iff org.Accounts[t.AccountID] matches every OU glob and tag
        annotate kept targets with OUPath and OrgAccountName
  -> dedupe
```

`--ou` and `--account-tag` imply preflight, exactly as `--dedupe` already does
at `discovery.go:154`; the condition there simply extends. Passing
`--preflight=false` together with `--ou` is a `cmd/`-level error rather than a
silent override, detected with `cmd.Flags().Changed("preflight")`.

To report unreachable accounts without changing the AGENT CONTRACT signature
of `ResolveTargets`, add:

```go
// OrgSelection reports what org enumeration contributed to a resolution.
type OrgSelection struct {
    Org         *Org
    Matched     []string     // account IDs that matched the OU and tag filters
    Unreachable []OrgAccount // matched the filters, but no local profile resolves to them
}

func ResolveTargetsWithOrg(ctx context.Context, sel Selector) ([]Target, *OrgSelection, error)
```

`ResolveTargets` becomes a wrapper that discards the second value, so its
pinned signature and behavior are unchanged. `OrgSelection` is nil when no org
selector was used.

## Safety model impact

Three changes touch the invariants in AGENTS.md. All three are deliberate and
must be called out in the implementing PR.

**1. awsmux briefly handles credential material (amends the `discovery.go:33`
rule).** That rule says awsmux never resolves credentials itself because it
always shells out to `aws --profile <name>`. Enumeration with `--org-role`
breaks that: the assume-role output is held in memory and passed as
environment to the `organizations` calls. The scope is bounded and must stay
bounded: enumeration only, never fan-out execution, never written to the org
cache or any other file, never logged. Fan-out remains profile-based, so the
rule still holds for everything that actually executes an approved plan.

Noted for the record: `sts assume-role` is `ClassMutating` in awsmux's own
tables (`classify.go:101`), so awsmux performs internally an operation it
would gate if a user planned it. That is acceptable because it is machinery
rather than a user-submitted plan, but the doc comment on `LoadOrg` should say
so rather than leave it as a surprise.

**2. The plan hash covers the new Target fields.** `OUPath` and
`OrgAccountName` are added to `targetKey` in `ComputeHash` (`plan.go:64`).
They do not influence `BuildCommand`, so this is not about blast radius. It is
that both are rendered in the plan echo a human reads when approving, and
anything shown at approval time must be covered by the integrity check.
Without this, editing a stored plan's `OUPath` would change what an approver
sees without tripping the hash mismatch in `CheckApproval`.

The consequence is the desirable one. `CheckApproval` recomputes the hash from
the stored plan struct (`policy.go:76`), not from live org data, so an account
that moves OU between plan and apply does not invalidate an approval, and
apply gains no new network dependency. Identity drift remains covered by
`VerifyIdentities`, which is where it belongs.

**3. `PolicyVersion` goes v2 to v3.** Adding fields to `targetKey` changes the
hash payload for every plan, so plans stored before the upgrade will fail
`CheckApproval` with "hash mismatch: plan was modified after approval". That
fails closed, which is correct, and `DefaultPlanTTL` is one hour so the window
is narrow. Bumping the version makes the cause explicit instead of letting a
routine upgrade look like tampering.

Unchanged and worth stating: selection still cannot widen blast radius on its
own, because every selected target is still STS-verified, still hashed into
the plan, and still gated by `RequiresApproval`.

## Error handling

Every failure path closes rather than widens.

- **Enumeration failure with an org selector set is a hard
  `ExitConfigError`.** There is no fallback to an unfiltered target list. A
  selector that fails open would silently turn `--ou eng/prod` into "every
  profile on this machine", which is the exact blast-radius bug awsmux exists
  to prevent. This mirrors classification failing safe to `unknown`.
- **Assume-role failure** surfaces the STS error verbatim, as
  `ExitConfigError`.
- **Zero matches** must distinguish the two cases that currently look
  identical:

  ```
  no targets matched --ou eng/prod
    12 accounts in eng/prod, 0 with a local profile
    hint: awsmux targets --ou eng/prod --show-unreachable
  ```

  An empty OU and an OU you have no access to are different problems, and the
  difference matters most immediately before a fan-out.
- **Stale cache** is never an error. `--org-refresh` forces re-enumeration,
  and the hint appears in the zero-match message.

One principled non-behavior: awsmux does not filter on the Organizations
`SUSPENDED` status. If a profile exists and preflight succeeded, the account
is reachable, and STS is the authority on reachability rather than org
metadata. Status is surfaced in output and never enforced.

## CLI surface

`addSelectorFlags` (`cmd/root.go:73`) gains the flags, which covers `targets`,
`run`, and `plan` from one edit point:

| Flag | Meaning |
| --- | --- |
| `--ou` | OU path globs, repeatable, e.g. `eng/prod`, `eng/*` |
| `--account-tag` | `key=value`, repeatable, all must match |
| `--org-role` | role ARN assumed for enumeration |
| `--org-profile` | base profile for the organizations calls |
| `--org-refresh` | bypass the org cache |
| `--show-unreachable` | list every unreachable account instead of a summary |

With `--org-profile` empty, the organizations calls run with no `--profile`
flag at all, letting normal AWS resolution (environment, then default profile)
apply.

Table output gains an `OU` column when an org selector was used, and a
trailing summary:

```
$ awsmux targets --ou eng/prod
PROFILE     REGION      ACCOUNT       OU
prod-web    us-east-1   1234...9012   eng/prod
prod-api    us-east-1   2109...4321   eng/prod

37 targets. 12 accounts in eng/prod have no local profile:
  4455...6677 (prod-batch), 5566...7788 (prod-ml), +10 more
  hint: --show-unreachable for the full list
```

## MCP surface

`list_aws_targets` and `plan_aws_operation` gain `ou`, `account_tags`,
`org_role`, `org_profile`, and `org_refresh`, in both the JSON schemas and the
strict-decode structs at `tools.go:197` and `tools.go:224`. Strict decoding
rejects unknown fields by design, so these are required additions rather than
optional ones.

`results.go` renders the unreachable set under the existing token-economy
rule: one compact count line with a truncated ID list, never a full roster.
There is no MCP equivalent of `--show-unreachable`; an agent that needs the
full list pages it with the existing offset mechanism, which is how every
other oversized result in that layer already works.

## Testing

All tests drive the existing `AWSMUX_AWS_BIN` stub-binary seam, so they need
neither network nor Docker, consistent with the rest of the suite.

- `org_test.go`: OU path construction from a fake tree, segment-prefix
  matching (including the recursive case `eng/prod` matching `eng/prod/db`,
  which plain `path.Match` would miss), tag AND-matching, cache hit and TTL
  expiry, `--org-refresh` bypass, and fail-closed behavior when enumeration
  errors.
- `plan_test.go`: changing `OUPath` changes the plan hash.
- `discovery_test.go`: the org filter runs after preflight, and joins on the
  verified account ID rather than the profile name. A profile whose name
  suggests one OU but whose verified account sits in another must follow the
  account.
- `tools_test.go`: the new MCP fields decode, and a misspelled field still
  errors.

To verify during implementation: whether LocalStack community supports the
Organizations API. It may be Pro-gated, in which case `make e2e` cannot cover
this path and the stub-binary tests are the entire story. If it is gated, say
so plainly in `docs/ARCHITECTURE.md` rather than leave an unexplained gap in
e2e coverage.

## Docs to update in the same change

- `README.md`: the new flags, and the roadmap line that currently promises
  this feature.
- `docs/ARCHITECTURE.md`: the engine diagram, the enumeration sequence, and
  the e2e coverage note.
- `AGENTS.md`: the three safety-model changes above.
