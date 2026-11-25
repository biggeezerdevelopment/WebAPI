package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const passwordSecret = "pw-secret-value-1234"

const basicTemplate = `
webapi:
  - enabled: true
    name: "example"
    endpoint: "https://example.atlassian.net/rest/api/3/myself"
    method: "get"
    auth_type: "%AUTHTYPE%"
    %AUTH%
    sleeptime: 60
    timeout: 30
`

func loadBasic(t *testing.T, authType, auth string) (*Config, error) {
	t.Helper()
	text := strings.NewReplacer("%AUTHTYPE%", authType, "%AUTH%", auth).Replace(basicTemplate)
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func TestAPIPasswordEnvLoads(t *testing.T) {
	t.Setenv("TEST_JIRA_TOKEN", passwordSecret)
	cfg, err := loadBasic(t, "basic", "api_username: \"svc-audit@example.com\"\n    api_password_env: \"TEST_JIRA_TOKEN\"")
	if err != nil {
		t.Fatal(err)
	}
	if pw, err := cfg.WebAPI[0].ResolveAPIPassword(); err != nil || pw != passwordSecret {
		t.Errorf("password resolution failed: %v", err)
	}
}

func TestInlineAPIPasswordStillWorks(t *testing.T) {
	cfg, err := loadBasic(t, "basic", "api_username: \"u\"\n    api_password: \"inline\"")
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := cfg.WebAPI[0].ResolveAPIPassword(); pw != "inline" {
		t.Errorf("password = %q", pw)
	}
}

func TestAPIPasswordRules(t *testing.T) {
	t.Setenv("TEST_JIRA_TOKEN", passwordSecret)
	t.Setenv("TEST_BLANK_TOKEN", "   ")
	t.Setenv("TEST_BAD_TOKEN", passwordSecret+"\n")
	os.Unsetenv("TEST_UNSET_TOKEN")
	cases := map[string]struct {
		authType, auth, want string
	}{
		"both set": {"basic", "api_username: \"u\"\n    api_password: \"" + passwordSecret + "\"\n    api_password_env: \"TEST_JIRA_TOKEN\"",
			"api_password and api_password_env cannot both be set"},
		"both keys present, inline empty": {"basic", "api_username: \"u\"\n    api_password: \"\"\n    api_password_env: \"TEST_JIRA_TOKEN\"",
			"cannot both be set"},
		"env set but empty": {"basic", "api_username: \"u\"\n    api_password_env: \"\"",
			"api_password_env must name an environment variable"},
		"unset variable": {"basic", "api_username: \"u\"\n    api_password_env: \"TEST_UNSET_TOKEN\"",
			`"TEST_UNSET_TOKEN" named by api_password_env is unset or empty`},
		"blank variable": {"basic", "api_username: \"u\"\n    api_password_env: \"TEST_BLANK_TOKEN\"",
			`"TEST_BLANK_TOKEN" named by api_password_env is unset or empty`},
		"invalid characters": {"basic", "api_username: \"u\"\n    api_password_env: \"TEST_BAD_TOKEN\"",
			`"TEST_BAD_TOKEN" named by api_password_env contains invalid characters`},
		"basic without username": {"basic", "api_password_env: \"TEST_JIRA_TOKEN\"",
			"auth_type basic requires api_username"},
		"basic without password": {"basic", "api_username: \"u\"",
			"auth_type basic requires api_password or api_password_env"},
		"non-basic still checks password": {"apikey", "api_key: \"k\"\n    api_password_env: \"TEST_UNSET_TOKEN\"",
			"unset or empty"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadBasic(t, tc.authType, tc.auth)
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not contain %q", msg, tc.want)
			}
			if !strings.Contains(msg, `"example"`) {
				t.Errorf("error %q does not name the entry", msg)
			}
			if strings.Contains(msg, passwordSecret) {
				t.Errorf("error leaks the secret: %q", msg)
			}
		})
	}
}

func TestCheckAuthBasicForCollectors(t *testing.T) {
	t.Setenv("TEST_JIRA_TOKEN", passwordSecret)
	ok := WebAPI{AuthType: AuthTypeBasic, APIUsername: "u", APIPasswordEnv: "TEST_JIRA_TOKEN"}
	if err := ok.CheckAuth(AuthTypeBasic); err != nil {
		t.Errorf("basic: %v", err)
	}
	want := "collector requires auth_type basic with api_username and api_password or api_password_env"
	for name, w := range map[string]WebAPI{
		"wrong type":  {AuthType: AuthTypeAPIKey, APIUsername: "u", APIPasswordEnv: "TEST_JIRA_TOKEN"},
		"no username": {AuthType: AuthTypeBasic, APIPasswordEnv: "TEST_JIRA_TOKEN"},
		"no password": {AuthType: AuthTypeBasic, APIUsername: "u"},
	} {
		if err := w.CheckAuth(AuthTypeBasic); err == nil || err.Error() != want {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	apikeyOnly := WebAPI{AuthType: AuthTypeBasic, APIUsername: "u", APIPasswordEnv: "TEST_JIRA_TOKEN"}
	if err := apikeyOnly.CheckAuth(AuthTypeAPIKey); err == nil ||
		err.Error() != "collector requires auth_type apikey with api_key or api_key_env" {
		t.Errorf("apikey rule: err = %v", err)
	}
}

func TestGitHubAuditRejectsBasic(t *testing.T) {
	t.Setenv("TEST_JIRA_TOKEN", passwordSecret)
	cfg := Config{StateDir: t.TempDir(), WebAPI: []WebAPI{{
		Enabled: true, Name: "gh", Endpoint: "https://api.github.com/orgs/x/audit-log", Method: "get",
		AuthType: AuthTypeBasic, APIUsername: "u", APIPasswordEnv: "TEST_JIRA_TOKEN", SleepTime: 1, Timeout: 1,
		Collector: &Collector{Type: CollectorTypeGitHubAudit},
	}}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "collector requires auth_type apikey with api_key or api_key_env") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), passwordSecret) {
		t.Error("error leaks the secret")
	}
}
