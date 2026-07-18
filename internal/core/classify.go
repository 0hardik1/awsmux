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

// Classify returns the risk class for an AWS CLI service + operation pair.
//
// Strategy: classify by the operation's leading verb (text before the first
// hyphen, compared lowercase), with a small service override table where the
// verb convention lies: sts is always read-only (including assume-role, which
// mints nothing durable), and aws s3 subcommands are mapped explicitly.
// Anything unrecognized is ClassUnknown, which policy treats like mutating.
func Classify(service, operation string) Classification {
	svc := strings.ToLower(strings.TrimSpace(service))
	op := strings.ToLower(strings.TrimSpace(operation))

	if svc == "sts" {
		return ClassReadOnly
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
