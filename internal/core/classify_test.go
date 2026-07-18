package core

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		service   string
		operation string
		want      Classification
	}{
		// Read-only verbs.
		{"ec2", "describe-instances", ClassReadOnly},
		{"s3api", "list-buckets", ClassReadOnly},
		{"iam", "get-user", ClassReadOnly},
		{"logs", "head-object", ClassReadOnly},

		// Mutating verbs.
		{"iam", "create-user", ClassMutating},
		{"ec2", "run-instances", ClassMutating},
		{"s3api", "put-bucket-policy", ClassMutating},
		{"lambda", "update-function-code", ClassMutating},
		{"lambda", "invoke", ClassMutating},

		// Destructive verbs.
		{"ec2", "terminate-instances", ClassDestructive},
		{"ec2", "delete-vpc", ClassDestructive},
		{"iam", "remove-user-from-group", ClassDestructive},
		{"ec2", "stop-instances", ClassDestructive},

		// Unknown verbs fail safe.
		{"foo", "frobnicate-thing", ClassUnknown},
		{"ec2", "", ClassUnknown},

		// sts is always read-only, including assume-role.
		{"sts", "get-caller-identity", ClassReadOnly},
		{"sts", "assume-role", ClassReadOnly},

		// aws s3 subcommands are mapped explicitly.
		{"s3", "ls", ClassReadOnly},
		{"s3", "presign", ClassReadOnly},
		{"s3", "rm", ClassDestructive},
		{"s3", "rb", ClassDestructive},
		{"s3", "cp", ClassMutating},
		{"s3", "sync", ClassMutating},

		// Two-word read verbs must not split at the first hyphen.
		{"dynamodb", "batch-get-item", ClassReadOnly},
		{"ec2", "batch-describe-something", ClassReadOnly},

		// Case and whitespace insensitivity.
		{"EC2", "Describe-Instances", ClassReadOnly},
		{" sts ", " Assume-Role ", ClassReadOnly},
	}
	for _, tt := range tests {
		if got := Classify(tt.service, tt.operation); got != tt.want {
			t.Errorf("Classify(%q, %q) = %q, want %q", tt.service, tt.operation, got, tt.want)
		}
	}
}
