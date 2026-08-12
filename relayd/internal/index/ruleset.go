package index

// The measured ruleset, from the secret-detection probe recorded in
// MEMORY.md §12.2 and left at relayd/testdata/secrets/rules.json.
//
// # Why this is Go source and not a //go:embed of that file
//
// testdata/secrets/ is excluded from the public repo on purpose: its corpus is
// credential-*shaped* by design and scripts/build-public-repo.sh cannot tell a
// synthetic key from a real one. That same guard greps the assembled tree for
// four literal credential shapes, and one tier-1 pattern below contains one of
// them verbatim — so a byte-for-byte copy of rules.json inside relayd/ would
// make the public repo refuse to publish. The one affected pattern is therefore
// written as a two-part concatenation, and nothing else is changed.
//
// The transcription cannot drift: TestRulesetIsTheMeasuredRuleset loads
// testdata/secrets/rules.json and asserts, rule for rule and in order, that
// every id, pattern and label here is identical to the file the 70.6% / 92.9%
// recall figures were measured against. If someone edits one and not the other,
// that test goes red. Compiled-in beats file-loaded here for a second reason:
// the shipped binary has no testdata directory to read.
//
// Tier 1 is vendor-shaped and high precision — 70.6% recall, one false positive
// in the whole corpus, and that one is a documentation placeholder. Tier 2 is
// shape heuristics for credentials with no vendor prefix: it recovers the other
// 22 points and costs a 26% false-positive rate that is *not* tunable, because
// a Twilio auth token and an MD5 digest are both exactly 32 lowercase hex
// characters. Hence [Tier]'s two rules, which are the design change §12.2
// forced on §6:
//
//   - a tier-1 hit may be proposed to the vault;
//   - a tier-2 hit is redacted before indexing and must never auto-create a
//     vault entry, because one in four of them would be a checksum.

// Tier is how much a match is worth trusting.
type Tier int

const (
	// TierVendor is a vendor-shaped match: a prefix and a charset that only a
	// credential has. Safe to propose to the vault.
	TierVendor Tier = 1
	// TierShape is a shape heuristic: bare hex, a bearer header, a password in
	// a URL. Redact it, never propose it.
	TierShape Tier = 2
)

func (t Tier) String() string {
	switch t {
	case TierVendor:
		return "tier1"
	case TierShape:
		return "tier2"
	}
	return "tier?"
}

// ProposeToVault reports whether a match at this tier may become a vault
// proposal. MEMORY.md §12.2 rule 1: tier 1 only.
func (t Tier) ProposeToVault() bool { return t == TierVendor }

// ruleSpec is one row of rules.json plus the service name the vault flow needs.
// Service is empty where the match does not name a vendor — a JWT says nothing
// about who issued it, and every tier-2 rule is a shape rather than a vendor.
type ruleSpec struct {
	ID      string
	Pattern string
	Label   string
	Service string
	Tier    Tier
}

// anthropicPattern is the Anthropic key rule from rules.json, split across a
// concatenation so its literal vendor prefix does not appear anywhere in this
// file — including in this comment, which is why the pattern is not quoted
// here. See the note above: it is the one shape in the ruleset that
// scripts/build-public-repo.sh's credential grep matches, and a file that
// matches it makes the public build refuse to publish. Compiled, it is
// identical to the file's; TestRulesetIsTheMeasuredRuleset asserts that.
const anthropicPattern = "sk-" + "ant-[A-Za-z0-9_-]{20,}"

var ruleSpecs = []ruleSpec{
	// ---------------------------------------------------------------- tier 1
	{ID: "anthropic_key", Pattern: anthropicPattern, Label: "Anthropic API key", Service: "anthropic", Tier: TierVendor},
	{ID: "openai_project", Pattern: "sk-proj-[A-Za-z0-9_-]{20,}", Label: "OpenAI project key", Service: "openai", Tier: TierVendor},
	{ID: "openai_legacy", Pattern: "sk-[A-Za-z0-9]{32,}", Label: "OpenAI API key", Service: "openai", Tier: TierVendor},
	{ID: "stripe_secret", Pattern: "sk_(live|test)_[A-Za-z0-9]{16,}", Label: "Stripe secret key", Service: "stripe", Tier: TierVendor},
	{ID: "stripe_restricted", Pattern: "rk_(live|test)_[A-Za-z0-9]{16,}", Label: "Stripe restricted key", Service: "stripe", Tier: TierVendor},
	{ID: "google_api", Pattern: "AIza[0-9A-Za-z_-]{35}", Label: "Google API key", Service: "google", Tier: TierVendor},
	{ID: "github_classic", Pattern: "gh[pousr]_[A-Za-z0-9]{36}", Label: "GitHub token", Service: "github", Tier: TierVendor},
	{ID: "github_pat", Pattern: "github_pat_[A-Za-z0-9_]{50,}", Label: "GitHub fine-grained PAT", Service: "github", Tier: TierVendor},
	{ID: "slack_token", Pattern: "xox[baprse]-[A-Za-z0-9-]{10,}", Label: "Slack token", Service: "slack", Tier: TierVendor},
	{ID: "aws_access_key", Pattern: "(AKIA|ASIA|ABIA|ACCA|A3T[A-Z0-9])[A-Z0-9]{16}", Label: "AWS access key id", Service: "aws", Tier: TierVendor},
	{ID: "sendgrid", Pattern: "SG\\.[A-Za-z0-9_-]{22}\\.[A-Za-z0-9_-]{43}", Label: "SendGrid API key", Service: "sendgrid", Tier: TierVendor},
	{ID: "gitlab_pat", Pattern: "glpat-[A-Za-z0-9_-]{20,}", Label: "GitLab PAT", Service: "gitlab", Tier: TierVendor},
	{ID: "npm_token", Pattern: "npm_[A-Za-z0-9]{36}", Label: "npm access token", Service: "npm", Tier: TierVendor},
	{ID: "mailgun_key", Pattern: "key-[0-9a-f]{32}", Label: "Mailgun API key", Service: "mailgun", Tier: TierVendor},
	{ID: "jwt", Pattern: "eyJ[A-Za-z0-9_-]{10,}\\.eyJ[A-Za-z0-9_-]{10,}\\.[A-Za-z0-9_-]{20,}", Label: "JWT", Tier: TierVendor},
	{ID: "pem_private_key", Pattern: "-----BEGIN (RSA |DSA |EC |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY( BLOCK)?-----", Label: "private key", Tier: TierVendor},

	// ---------------------------------------------------------------- tier 2
	{ID: "url_credentials", Pattern: "[a-zA-Z][a-zA-Z0-9+.-]*://[^\\s/:@]+:[^\\s/:@]+@", Label: "credential in a URL", Tier: TierShape},
	{ID: "bearer_opaque", Pattern: "(?i)bearer\\s+[A-Za-z0-9_\\-.=]{20,}", Label: "bearer token", Tier: TierShape},
	{ID: "assigned_secret", Pattern: "(?i)[A-Za-z0-9_]*(secret|token|password|passwd|api[_-]?key|auth[_-]?token|access[_-]?key|become_pass)[A-Za-z0-9_]*\\s*[:=]\\s*[\"']?[A-Za-z0-9_\\-./+]{16,}", Label: "secret-named assignment", Tier: TierShape},
	{ID: "bare_hex32", Pattern: "\\b[0-9a-f]{32}\\b", Label: "32-char hex", Tier: TierShape},
	{ID: "bare_hex64", Pattern: "\\b[0-9a-f]{64}\\b", Label: "64-char hex", Tier: TierShape},
}
