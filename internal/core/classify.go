package core

import "strings"

// verbClass maps an operation's leading verb (text before the first hyphen,
// lowercase) to its risk class.
var verbClass = map[string]Classification{
	// Read-only verbs.
	"describe": ClassReadOnly,
	"list":     ClassReadOnly,
	"get":      ClassReadOnly,
	"lookup":   ClassReadOnly,
	"search":   ClassReadOnly,
	"query":    ClassReadOnly,
	"scan":     ClassReadOnly,
	"head":     ClassReadOnly,
	"preview":  ClassReadOnly,
	"estimate": ClassReadOnly,
	"view":     ClassReadOnly,

	// Destructive verbs.
	"delete":       ClassDestructive,
	"terminate":    ClassDestructive,
	"revoke":       ClassDestructive,
	"remove":       ClassDestructive,
	"deregister":   ClassDestructive,
	"release":      ClassDestructive,
	"disable":      ClassDestructive,
	"reboot":       ClassDestructive,
	"stop":         ClassDestructive,
	"cancel":       ClassDestructive,
	"destroy":      ClassDestructive,
	"purge":        ClassDestructive,
	"detach":       ClassDestructive,
	"disassociate": ClassDestructive,
	"untag":        ClassDestructive,
	"reset":        ClassDestructive,
	"unassign":     ClassDestructive,
	"unsubscribe":  ClassDestructive,

	// Mutating verbs.
	"create":    ClassMutating,
	"put":       ClassMutating,
	"update":    ClassMutating,
	"modify":    ClassMutating,
	"run":       ClassMutating,
	"start":     ClassMutating,
	"attach":    ClassMutating,
	"associate": ClassMutating,
	"tag":       ClassMutating,
	"register":  ClassMutating,
	"enable":    ClassMutating,
	"add":       ClassMutating,
	"set":       ClassMutating,
	"import":    ClassMutating,
	"copy":      ClassMutating,
	"restore":   ClassMutating,
	"invoke":    ClassMutating,
	"send":      ClassMutating,
	"publish":   ClassMutating,
	"request":   ClassMutating,
	"allocate":  ClassMutating,
	"authorize": ClassMutating,
	"assign":    ClassMutating,
	"subscribe": ClassMutating,
	"promote":   ClassMutating,
	"resize":    ClassMutating,
	"reencrypt": ClassMutating,
	"rotate":    ClassMutating,
}

// twoWordReadOnlyVerbs are hyphenated read verbs that would misclassify if
// split at the first hyphen ("batch-get-item" would otherwise be unknown).
var twoWordReadOnlyVerbs = []string{"batch-get", "batch-describe"}

// s3Class covers aws s3 subcommands, which do not follow verb-noun naming.
var s3Class = map[string]Classification{
	"ls":      ClassReadOnly,
	"presign": ClassReadOnly,
	"rm":      ClassDestructive,
	"rb":      ClassDestructive,
	"cp":      ClassMutating,
	"sync":    ClassMutating,
	"mb":      ClassMutating,
	"mv":      ClassMutating,
}

// stsClass overrides the verb rules for sts: the "get" prefix would make
// credential-minting calls look read-only, but a minted credential is a live
// side effect (and assume-role can pivot into another account entirely), so
// those require approval. Only genuinely inert lookups run freely.
var stsClass = map[string]Classification{
	"get-caller-identity":          ClassReadOnly,
	"get-access-key-info":          ClassReadOnly,
	"decode-authorization-message": ClassReadOnly,

	"assume-role":                   ClassMutating,
	"assume-role-with-saml":         ClassMutating,
	"assume-role-with-web-identity": ClassMutating,
	"assume-root":                   ClassMutating,
	"get-session-token":             ClassMutating,
	"get-federation-token":          ClassMutating,
}

// s3apiLocalWrite overrides s3api operations that are read-only on the AWS
// side but take an outfile argument and write to an arbitrary local path;
// treat them as mutating so they need approval.
var s3apiLocalWrite = map[string]Classification{
	"get-object":            ClassMutating,
	"get-object-torrent":    ClassMutating,
	"select-object-content": ClassMutating,
}

// Classify returns the risk class for an AWS CLI service + operation pair.
//
// Strategy: classify by the operation's leading verb (text before the first
// hyphen, compared lowercase), with small service override tables where the
// verb convention lies: sts credential-minting calls are mutating despite
// their read-ish names, s3api operations that write a local outfile are
// mutating despite their get prefix, and aws s3 subcommands are mapped
// explicitly. Anything unrecognized is ClassUnknown, which policy treats
// like mutating.
func Classify(service, operation string) Classification {
	svc := strings.ToLower(strings.TrimSpace(service))
	op := strings.ToLower(strings.TrimSpace(operation))

	if svc == "sts" {
		if c, ok := stsClass[op]; ok {
			return c
		}
	}
	if svc == "s3api" {
		if c, ok := s3apiLocalWrite[op]; ok {
			return c
		}
	}
	if svc == "s3" {
		if c, ok := s3Class[op]; ok {
			return c
		}
	}
	for _, v := range twoWordReadOnlyVerbs {
		if op == v || strings.HasPrefix(op, v+"-") {
			return ClassReadOnly
		}
	}
	verb := op
	if i := strings.IndexByte(op, '-'); i >= 0 {
		verb = op[:i]
	}
	if c, ok := verbClass[verb]; ok {
		return c
	}
	return ClassUnknown
}
