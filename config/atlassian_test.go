package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const atlassianTemplate = `
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
    name: "atlassian-org-audit"
    subject: "AtlassianOrgAudit"
    endpoint: "https://api.atlassian.com/admin/v1/orgs/org-123/events-stream"
    method: "get"
    %AUTH%
    sleeptime: 60
    timeout: 30
    %PARAMS%
    collector:
      type: "atlassian_org_audit"
      initial_lookback_hours: 24
    outputs: ["siem-webhook"]
`

func loadAtlassian(t *testing.T, auth, params string) error {
	t.Helper()
	text := strings.NewReplacer("%STATE%", `"`+filepath.ToSlash(t.TempDir())+`"`, "%AUTH%", auth, "%PARAMS%", params).Replace(atlassianTemplate)
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	return err
}

func TestAtlassianCollectorValidation(t *testing.T) {
	t.Setenv("ATLASSIAN_ORG_API_KEY", "secret-token-value")
	os.Unsetenv("TEST_UNSET_TOKEN")
	const key = "auth_type: \"apikey\"\n    api_key_env: \"ATLASSIAN_ORG_API_KEY\""
	param := func(kv string) string { return "params:\n      " + kv }
	valid := map[string]struct{ auth, params string }{
		"issue example": {key, param(`limit: "500"`)},
		"no params":     {key, ""},
		"limit 1":       {key, param(`limit: "1"`)},
		"inline key":    {"auth_type: \"apikey\"\n    api_key: \"secret-inline\"", ""},
	}
	for name, tc := range valid {
		if err := loadAtlassian(t, tc.auth, tc.params); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	invalid := map[string]struct{ auth, params, want string }{
		"basic auth":  {"auth_type: \"basic\"\n    api_username: \"u\"\n    api_password: \"secret-pw\"", "", "auth_type apikey"},
		"no auth":     {"", "", "auth_type apikey"},
		"no key":      {"auth_type: \"apikey\"", "", "auth_type apikey"},
		"unset env":   {"auth_type: \"apikey\"\n    api_key_env: \"TEST_UNSET_TOKEN\"", "", "unset or empty"},
		"from":        {key, param(`from: "1"`), `"from"`},
		"to":          {key, param(`to: "1"`), `"to"`},
		"cursor":      {key, param(`cursor: "x"`), `"cursor"`},
		"sortOrder":   {key, param(`sortOrder: "asc"`), `"sortOrder"`},
		"limit 0":     {key, param(`limit: "0"`), "params.limit"},
		"limit 501":   {key, param(`limit: "501"`), "params.limit"},
		"limit word":  {key, param(`limit: "many"`), "params.limit"},
		"limit float": {key, param(`limit: "2.5"`), "params.limit"},
	}
	for name, tc := range invalid {
		err := loadAtlassian(t, tc.auth, tc.params)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "atlassian-org-audit") {
			t.Errorf("%s: error does not name the entry: %s", name, msg)
		}
		for _, leak := range []string{"secret", "https://", "api.atlassian.com", "org-123"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%s: error leaks %q: %s", name, leak, msg)
			}
		}
	}
}
