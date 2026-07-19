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

		// sts: inert lookups run freely, credential-minting calls do not.
		{"sts", "get-caller-identity", ClassReadOnly},
		{"sts", "get-access-key-info", ClassReadOnly},
		{"sts", "decode-authorization-message", ClassReadOnly},
		{"sts", "assume-role", ClassMutating},
		{"sts", "assume-role-with-web-identity", ClassMutating},
		{"sts", "get-session-token", ClassMutating},
		{"sts", "get-federation-token", ClassMutating},

		// s3api operations that write a local outfile need approval even
		// though they are read-only on the AWS side.
		{"s3api", "get-object", ClassMutating},
		{"s3api", "get-object-torrent", ClassMutating},
		{"s3api", "select-object-content", ClassMutating},
		{"s3api", "get-bucket-policy", ClassReadOnly},
		{"s3api", "head-object", ClassReadOnly},

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
		{" sts ", " Assume-Role ", ClassMutating},
	}
	for _, tt := range tests {
		if got := Classify(tt.service, tt.operation); got != tt.want {
			t.Errorf("Classify(%q, %q) = %q, want %q", tt.service, tt.operation, got, tt.want)
		}
	}
}
