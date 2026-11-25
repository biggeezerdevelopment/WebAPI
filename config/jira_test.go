package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const jiraTemplate = `
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
    name: "jira-audit"
    subject: "JiraAudit"
    endpoint: "https://example.atlassian.net/rest/api/3/auditing/record"
    method: "get"
    %AUTH%
    sleeptime: 300
    timeout: 30
    %PARAMS%
    collector:
      type: "jira_audit"
      initial_lookback_hours: 24
    outputs: ["siem-webhook"]
`

func loadJira(t *testing.T, auth, params string) error {
	t.Helper()
	text := strings.NewReplacer("%STATE%", `"`+filepath.ToSlash(t.TempDir())+`"`, "%AUTH%", auth, "%PARAMS%", params).Replace(jiraTemplate)
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	return err
}

func TestJiraCollectorValidation(t *testing.T) {
	t.Setenv("JIRA_API_TOKEN", "secret-token-value")
	os.Unsetenv("TEST_UNSET_TOKEN")
	const basic = "auth_type: \"basic\"\n    api_username: \"svc-audit@example.com\"\n    api_password_env: \"JIRA_API_TOKEN\""
	param := func(kv string) string { return "params:\n      " + kv }
	valid := map[string]struct{ auth, params string }{
		"issue example":   {basic, param(`limit: "1000"`)},
		"no params":       {basic, ""},
		"limit 1":         {basic, param(`limit: "1"`)},
		"filter":          {basic, param(`filter: "user management"`)},
		"inline password": {"auth_type: \"basic\"\n    api_username: \"u\"\n    api_password: \"secret-inline\"", ""},
	}
	for name, tc := range valid {
		if err := loadJira(t, tc.auth, tc.params); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	invalid := map[string]struct{ auth, params, want string }{
		"apikey auth": {"auth_type: \"apikey\"\n    api_key: \"secret-key\"", "", "auth_type basic"},
		"no auth":     {"", "", "auth_type basic"},
		"no username": {"auth_type: \"basic\"\n    api_password_env: \"JIRA_API_TOKEN\"", "", "auth_type basic"},
		"no password": {"auth_type: \"basic\"\n    api_username: \"u\"", "", "auth_type basic"},
		"unset env":   {"auth_type: \"basic\"\n    api_username: \"u\"\n    api_password_env: \"TEST_UNSET_TOKEN\"", "", "unset or empty"},
		"from":        {basic, param(`from: "2024-01-01"`), `"from"`},
		"to":          {basic, param(`to: "2024-01-01"`), `"to"`},
		"offset":      {basic, param(`offset: "0"`), `"offset"`},
		"limit 0":     {basic, param(`limit: "0"`), "params.limit"},
		"limit 1001":  {basic, param(`limit: "1001"`), "params.limit"},
		"limit word":  {basic, param(`limit: "many"`), "params.limit"},
		"limit float": {basic, param(`limit: "2.5"`), "params.limit"},
		"limit empty": {basic, param(`limit: ""`), "params.limit"},
		"limit -1":    {basic, param(`limit: "-1"`), "params.limit"},
	}
	for name, tc := range invalid {
		err := loadJira(t, tc.auth, tc.params)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "jira-audit") {
			t.Errorf("%s: error does not name the entry: %s", name, msg)
		}
		for _, leak := range []string{"secret", "https://", "example.atlassian.net"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%s: error leaks %q: %s", name, leak, msg)
			}
		}
	}
}
