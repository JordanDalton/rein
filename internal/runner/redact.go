package runner

import (
	"regexp"
	"strings"
)

// Redaction.
//
// Command output reaches three audiences the user did not necessarily mean to
// hand a credential to: the terminal scrollback, the model backend, and the
// run log on disk. Anything that looks like a secret is masked before it
// reaches any of them. The first few characters survive so the output is
// still useful — "which token is this?", "does it start with xoxb?" — and
// the length is stated so the model has no reason to go looking for the rest.
//
// This is a heuristic. It keys on variable names and on well-known token
// shapes, so an unlabelled password in free text will get through, and a
// harmless value under a suspicious name will be masked. Both are the right
// way to fail.

// keepPrefix is how many leading characters of a masked value are shown.
const keepPrefix = 4

// secretKey matches assignment keys that conventionally carry credentials.
var secretKey = regexp.MustCompile(`(?i)(secret|token|passw(or)?d|passphrase|credential|api[_-]?key|access[_-]?key|private[_-]?key|signing[_-]?key|encryption[_-]?key|_key$|^authorization$|^auth$|auth[_-])`)

// keyValue splits a KEY=value / KEY: value / "KEY": "value" line into its
// parts. Group 1 is everything up to and including the separator, 2 the value.
var keyValue = regexp.MustCompile(`^(\s*(?:export\s+)?["']?[A-Za-z_][A-Za-z0-9_.\-]*["']?\s*[=:]\s*)(.*)$`)

// keyName pulls the bare identifier out of the prefix matched by keyValue.
var keyName = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.\-]*`)

// tokenShapes are credential formats recognisable without a label. Each
// pattern's whole match is masked, except that group 1, when present, is
// kept as-is (it is the "Bearer " or "user:" that precedes the secret).
var tokenShapes = []*regexp.Regexp{
	regexp.MustCompile(`xox[abopers]-[A-Za-z0-9-]{10,}`),                             // Slack
	regexp.MustCompile(`xapp-[A-Za-z0-9-]{10,}`),                                     // Slack app-level
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),                                      // OpenAI, Anthropic, Stripe
	regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}`),                   // GitHub
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),                               // GitHub fine-grained
	regexp.MustCompile(`glpat-[A-Za-z0-9_-]{16,}`),                                   // GitLab
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                           // AWS access key id
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),                                      // Google API key
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), // JWT
	regexp.MustCompile(`((?i:bearer|basic)\s+)[A-Za-z0-9._~+/=-]{16,}`),              // Authorization header
	regexp.MustCompile(`(://[^/\s:@]+:)[^@\s/]+@`),                                   // password in a URL
}

var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z ]*PRIVATE KEY-----`)
)

// Redact masks credentials in s and reports how many it masked.
func Redact(s string) (string, int) {
	if s == "" {
		return s, 0
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	n := 0
	inPEM := false
	for _, line := range lines {
		switch {
		case inPEM:
			if pemEnd.MatchString(line) {
				inPEM = false
				out = append(out, line)
			}
			continue
		case pemBegin.MatchString(line):
			inPEM = true
			n++
			out = append(out, line, "[redacted private key]")
			continue
		}
		masked, k := redactLine(line)
		n += k
		out = append(out, masked)
	}
	return strings.Join(out, "\n"), n
}

func redactLine(line string) (string, int) {
	if m := keyValue.FindStringSubmatch(line); m != nil {
		key := keyName.FindString(strings.TrimPrefix(strings.TrimSpace(m[1]), "export "))
		if secretKey.MatchString(key) {
			if v, ok := maskValue(m[2]); ok {
				return m[1] + v, 1
			}
		}
	}
	n := 0
	for _, re := range tokenShapes {
		line = re.ReplaceAllStringFunc(line, func(match string) string {
			sub := re.FindStringSubmatch(match)
			keep := ""
			if len(sub) > 1 {
				keep = sub[1]
			}
			secret := match[len(keep):]
			trail := ""
			if strings.HasSuffix(secret, "@") { // URL password: keep the @
				secret, trail = secret[:len(secret)-1], "@"
			}
			n++
			return keep + mask(secret) + trail
		})
	}
	return line, n
}

// maskValue masks the value side of an assignment, preserving any quotes and
// a trailing comma or semicolon so JSON and YAML still read as such. It
// declines for values that are plainly not secrets: booleans, small numbers,
// empty strings, ${REFERENCES} to other variables.
func maskValue(raw string) (string, bool) {
	v := strings.TrimSpace(raw)
	trail := ""
	if strings.HasSuffix(v, ",") || strings.HasSuffix(v, ";") {
		v, trail = v[:len(v)-1], v[len(v)-1:]
	}
	quote := ""
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		quote, v = v[:1], v[1:len(v)-1]
	}
	if !looksSecret(v) {
		return "", false
	}
	return quote + mask(v) + quote + trail, true
}

func looksSecret(v string) bool {
	if len(v) < 6 {
		return false
	}
	switch strings.ToLower(v) {
	case "true", "false", "null", "none", "changeme", "placeholder":
		return false
	}
	if strings.HasPrefix(v, "${") || strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">") {
		return false
	}
	digits := true
	for _, r := range v {
		if r < '0' || r > '9' {
			digits = false
			break
		}
	}
	return !digits
}

func mask(v string) string {
	if len(v) <= keepPrefix {
		return "[redacted]"
	}
	return v[:keepPrefix] + "…[redacted, " + itoa(len(v)) + " chars]"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
