package runner

import (
	"strings"
	"testing"
)

func TestRedactMasksLabelledValues(t *testing.T) {
	in := strings.Join([]string{
		`SLACK_BOT_TOKEN=xoxb-NOT-A-REAL-TOKEN-0000`,
		`export DB_PASSWORD='hunter2hunter2'`,
		`  "client_secret": "abcdefghijklmnopqrstuvwxyz",`,
		`api_key: AAAAAAAAAAAAAAAAAAAA`,
		`DEBUG=true`,
		`PORT=8080`,
		`APP_NAME=tackle`,
		`AUTH_ENABLED=false`,
		`SECRET_REF=${VAULT_SECRET}`,
	}, "\n")
	out, n := Redact(in)
	for _, leaked := range []string{"NOT-A-REAL", "hunter2", "abcdefghijklmnopqrstuvwxyz", "AAAAAAAAAAAAAAAAAAA"} {
		if strings.Contains(out, leaked) {
			t.Errorf("value %q survived redaction:\n%s", leaked, out)
		}
	}
	for _, kept := range []string{
		`SLACK_BOT_TOKEN=xoxb…[redacted, 26 chars]`,
		`export DB_PASSWORD='hunt…[redacted, 14 chars]'`,
		`"client_secret": "abcd…[redacted, 26 chars]",`,
		`DEBUG=true`, `PORT=8080`, `APP_NAME=tackle`, `AUTH_ENABLED=false`, `SECRET_REF=${VAULT_SECRET}`,
	} {
		if !strings.Contains(out, kept) {
			t.Errorf("expected %q in:\n%s", kept, out)
		}
	}
	if n != 4 {
		t.Errorf("redacted %d values, want 4", n)
	}
}

func TestRedactRecognisesTokenShapes(t *testing.T) {
	cases := map[string]string{
		`token is ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345 ok`:                                `ghp_…[redacted`,
		`Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefghijklmnop`: `Authorization: Bear…[redacted`,
		`curl -H 'Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefghijklmnop'`:      `Bearer eyJh…[redacted`,
		`url=postgres://app:s3cretpassw0rd@db.internal/app`:                               `postgres://app:s3cr…[redacted, 14 chars]@db.internal/app`,
		`aws_access_key_id = AKIAIOSFODNN7EXAMPLE`:                                        `AKIA…[redacted`,
		`ANTHROPIC_API_KEY=sk-ant-api03-abcdefghijklmnopqrstuvwxyz`:                       `sk-a…[redacted`,
	}
	for in, want := range cases {
		out, n := Redact(in)
		if !strings.Contains(out, want) || n == 0 {
			t.Errorf("Redact(%q) = %q (n=%d), want it to contain %q", in, out, n, want)
		}
	}
}

func TestRedactCollapsesPrivateKeys(t *testing.T) {
	in := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\nMIIEowIBAAKCAQEB\n-----END RSA PRIVATE KEY-----\nafter"
	out, n := Redact(in)
	if strings.Contains(out, "MIIEow") || !strings.Contains(out, "[redacted private key]") || !strings.HasSuffix(out, "after") || n != 1 {
		t.Errorf("got (n=%d):\n%s", n, out)
	}
}

func TestRedactLeavesOrdinaryOutputAlone(t *testing.T) {
	in := "commit 9d19eac\nAuthor: someone\n\n    Learn tools that answer --help with a man page\nkey: value\nname=tackle-slack"
	out, n := Redact(in)
	if out != in || n != 0 {
		t.Errorf("ordinary output was altered (n=%d):\n%s", n, out)
	}
}
