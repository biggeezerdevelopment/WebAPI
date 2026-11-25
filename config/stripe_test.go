package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const stripeTemplate = `
state_dir: %STATE%
logoutputs:
  - name: "siem-webhook"
    enabled: true
    type: "webhook"
    url: "https://hooks.example.com/audit"
    method: "POST"
    timeout: 5
webapi:
  - enabled: true
    name: "stripe-activity"
    subject: "StripeActivityLogs"
    endpoint: "https://api.stripe.com/v2/iam/activity_logs"
    method: "get"
    %AUTH%
    sleeptime: 300
    timeout: 30
    %HEADERS%
    %PARAMS%
    collector:
      type: "%TYPE%"
      initial_lookback_hours: 24
      %SETTLE%
    outputs: ["siem-webhook"]
`

const (
	stripeKey     = "auth_type: \"apikey\"\n    api_key_env: \"STRIPE_ACTIVITY_LOGS_KEY\""
	stripeVersion = "headers:\n      - key: \"Stripe-Version\"\n        value: \"2026-11-18.preview\""
)

type stripeCase struct{ auth, headers, params, typ, settle string }

func loadStripe(t *testing.T, tc stripeCase) (*Config, error) {
	t.Helper()
	if tc.typ == "" {
		tc.typ = CollectorTypeStripeActivityLogs
	}
	text := strings.NewReplacer(
		"%STATE%", `"`+filepath.ToSlash(t.TempDir())+`"`, "%AUTH%", tc.auth, "%HEADERS%", tc.headers,
		"%PARAMS%", tc.params, "%TYPE%", tc.typ, "%SETTLE%", tc.settle,
	).Replace(stripeTemplate)
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func stripeParams(lines ...string) string {
	return "params:\n      " + strings.Join(lines, "\n      ")
}

func TestStripeCollectorValidation(t *testing.T) {
	t.Setenv("STRIPE_ACTIVITY_LOGS_KEY", "secret-token-value")
	valid := map[string]stripeCase{
		"spec example": {stripeKey, stripeVersion, stripeParams(`limit: "100"`, `"action_groups[0]": "api_key"`, `"action_groups[1]": "user_roles"`), "", "settle_minutes: 30"},
		"no params":    {stripeKey, stripeVersion, "", "", ""},
		"bare filter":  {stripeKey, stripeVersion, stripeParams(`actions: "api_key_created"`), "", ""},
		"logs_from":    {stripeKey, stripeVersion, stripeParams(`"logs_from[0]": "@self"`), "", ""},
		"limit 1":      {stripeKey, stripeVersion, stripeParams(`limit: "1"`), "", ""},
		"settle 10":    {stripeKey, stripeVersion, "", "", "settle_minutes: 10"},
		"settle 1440":  {stripeKey, stripeVersion, "", "", "settle_minutes: 1440"},
		"header case":  {stripeKey, "headers:\n      - key: \"stripe-version\"\n        value: \"2026-11-18.preview\"", "", "", ""},
	}
	for name, tc := range valid {
		if _, err := loadStripe(t, tc); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	invalid := map[string]struct {
		tc   stripeCase
		want string
	}{
		"basic auth":          {stripeCase{"auth_type: \"basic\"\n    api_username: \"u\"\n    api_password: \"secret-pw\"", stripeVersion, "", "", ""}, "auth_type apikey"},
		"no version":          {stripeCase{stripeKey, "", "", "", ""}, "Stripe-Version"},
		"empty version":       {stripeCase{stripeKey, "headers:\n      - key: \"Stripe-Version\"\n        value: \"\"", "", "", ""}, "Stripe-Version"},
		"page":                {stripeCase{stripeKey, stripeVersion, stripeParams(`page: "page_x"`), "", ""}, `"page"`},
		"limit 0":             {stripeCase{stripeKey, stripeVersion, stripeParams(`limit: "0"`), "", ""}, "params.limit"},
		"limit 101":           {stripeCase{stripeKey, stripeVersion, stripeParams(`limit: "101"`), "", ""}, "params.limit"},
		"limit word":          {stripeCase{stripeKey, stripeVersion, stripeParams(`limit: "many"`), "", ""}, "params.limit"},
		"bracket array":       {stripeCase{stripeKey, stripeVersion, stripeParams(`"actions[]": "api_key_created"`), "", ""}, "[] array syntax"},
		"time filter":         {stripeCase{stripeKey, stripeVersion, stripeParams(`"created[gte]": "2026-01-01T00:00:00Z"`), "", ""}, "unknown parameter"},
		"starting_after":      {stripeCase{stripeKey, stripeVersion, stripeParams(`starting_after: "accact_x"`), "", ""}, "unknown parameter"},
		"unknown index":       {stripeCase{stripeKey, stripeVersion, stripeParams(`"types[0]": "x"`), "", ""}, "unknown parameter"},
		"leading zero":        {stripeCase{stripeKey, stripeVersion, stripeParams(`"actions[01]": "api_key_created"`), "", ""}, "unknown parameter"},
		"signed index":        {stripeCase{stripeKey, stripeVersion, stripeParams(`"actions[+1]": "api_key_created"`), "", ""}, "unknown parameter"},
		"bare and index":      {stripeCase{stripeKey, stripeVersion, stripeParams(`actions: "api_key_created"`, `"actions[0]": "api_key_deleted"`), "", ""}, "both bare and indexed"},
		"index gap":           {stripeCase{stripeKey, stripeVersion, stripeParams(`"actions[0]": "api_key_created"`, `"actions[2]": "api_key_deleted"`), "", ""}, "without gaps"},
		"no index 0":          {stripeCase{stripeKey, stripeVersion, stripeParams(`"actions[1]": "api_key_created"`), "", ""}, "without gaps"},
		"settle 0":            {stripeCase{stripeKey, stripeVersion, "", "", "settle_minutes: 0"}, "settle_minutes"},
		"settle 9":            {stripeCase{stripeKey, stripeVersion, "", "", "settle_minutes: 9"}, "settle_minutes"},
		"settle 1441":         {stripeCase{stripeKey, stripeVersion, "", "", "settle_minutes: 1441"}, "settle_minutes"},
		"settle on atlassian": {stripeCase{stripeKey, "", "", CollectorTypeAtlassianOrgAudit, "settle_minutes: 30"}, "only supported by stripe_activity_logs"},
	}
	for name, tc := range invalid {
		_, err := loadStripe(t, tc.tc)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "stripe-activity") {
			t.Errorf("%s: error does not name the entry: %s", name, msg)
		}
		for _, leak := range []string{"secret", "https://", "api.stripe.com", "2026-11-18", "api_key_created", "accact_x"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%s: error leaks %q: %s", name, leak, msg)
			}
		}
	}
}

func TestStripeSettleDelay(t *testing.T) {
	t.Setenv("STRIPE_ACTIVITY_LOGS_KEY", "secret-token-value")
	for settle, want := range map[string]time.Duration{"": 30 * time.Minute, "settle_minutes: 45": 45 * time.Minute} {
		cfg, err := loadStripe(t, stripeCase{stripeKey, stripeVersion, "", "", settle})
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.WebAPI[0].Collector.SettleDelay(); got != want {
			t.Errorf("settle %q: SettleDelay() = %s, want %s", settle, got, want)
		}
	}
}
