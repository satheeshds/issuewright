package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupAppFixtures makes a throwaway RSA key and a fake curl that answers the two
// API calls the minter makes.
func setupAppFixtures(t *testing.T) (key, bindir, calls string) {
	t.Helper()
	dir := t.TempDir()
	key = filepath.Join(dir, "app.pem")
	if out, err := exec.Command("openssl", "genrsa", "-out", key, "2048").CombinedOutput(); err != nil {
		t.Skipf("openssl unavailable: %v\n%s", err, out)
	}
	bindir = filepath.Join(dir, "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	calls = filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$@" >> ` + calls + `
case "$*" in
  *repos/acme/demo/installation*) printf '{"id":12345}' ;;
  *app/installations/12345/access_tokens*) printf '{"token":"ghs_minted","expires_at":"2026-11-17T20:00:00Z"}' ;;
  *) printf '{"ok":true}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bindir, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return key, bindir, calls
}

func runMinter(t *testing.T, env map[string]string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"gh-app-token.sh"}, args...)...)
	cmd.Dir = "."
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run minter: %v", err)
	}
	return string(out), code
}

func TestMinterWritesATokenFile(t *testing.T) {
	key, bindir, calls := setupAppFixtures(t)
	outFile := filepath.Join(t.TempDir(), "token.env")
	out, code := runMinter(t, map[string]string{
		"PATH":          bindir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GH_APP_ID":     "42",
		"GH_APP_KEY":    key,
		"GH_REPO":       "acme/demo",
		"GH_TOKEN_FILE": outFile,
	})
	if code != 0 {
		t.Fatalf("minter failed (%d):\n%s", code, out)
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("no token file: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(raw)) != "GH_TOKEN=ghs_minted" {
		t.Errorf("token file = %q", raw)
	}
	st, err := os.Stat(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %v, want 0600", st.Mode().Perm())
	}

	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("fake curl not used: %v", err)
	}
	if !strings.Contains(string(log), "Bearer eyJ") {
		t.Errorf("no JWT bearer header:\n%s", log)
	}
	if !strings.Contains(string(log), "-X POST") || !strings.Contains(string(log), "access_tokens") {
		t.Errorf("no token exchange:\n%s", log)
	}
}

// The JWT must be a real RS256 signature GitHub will accept the shape of:
// three base64url parts, a payload carrying iss/iat/exp.
func TestMinterSignsAWellFormedJWT(t *testing.T) {
	key, bindir, calls := setupAppFixtures(t)
	if out, code := runMinter(t, map[string]string{
		"PATH":          bindir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GH_APP_ID":     "987654",
		"GH_APP_KEY":    key,
		"GH_REPO":       "acme/demo",
		"GH_TOKEN_FILE": filepath.Join(t.TempDir(), "t.env"),
	}); code != 0 {
		t.Fatalf("minter failed (%d):\n%s", code, out)
	}
	log, _ := os.ReadFile(calls)
	jwt := ""
	for _, f := range strings.Fields(string(log)) {
		if strings.HasPrefix(f, "eyJ") {
			jwt = strings.TrimPrefix(f, "Bearer ")
			break
		}
	}
	if jwt == "" {
		// The header and token are separate fields; re-parse loosely.
		idx := strings.Index(string(log), "Bearer ")
		if idx < 0 {
			t.Fatalf("no bearer token in:\n%s", log)
		}
		jwt = strings.Fields(string(log)[idx+len("Bearer "):])[0]
	}
	if n := strings.Count(jwt, "."); n != 2 {
		t.Fatalf("JWT has %d dots, want 2: %q", n, jwt)
	}
	payload := strings.Split(jwt, ".")[1]
	decoded, err := exec.Command("sh", "-c",
		"printf '%s' "+payload+" | tr '_-' '/+' | base64 -d 2>/dev/null").CombinedOutput()
	if err != nil {
		t.Fatalf("payload is not base64url: %v\n%s", err, decoded)
	}
	p := string(decoded)
	for _, want := range []string{`"iss":"987654"`, `"iat"`, `"exp"`} {
		if !strings.Contains(p, want) {
			t.Errorf("payload missing %s: %s", want, p)
		}
	}
}

func TestMinterFailsHelpfully(t *testing.T) {
	key, bindir, _ := setupAppFixtures(t)
	base := map[string]string{"PATH": bindir + string(os.PathListSeparator) + os.Getenv("PATH")}

	cases := map[string]struct {
		env  map[string]string
		want string
	}{
		"no app id":   {map[string]string{"GH_REPO": "acme/demo", "GH_APP_KEY": key}, "GH_APP_ID (or GH_APP_CLIENT_ID) is required"},
		"no repo":     {map[string]string{"GH_APP_ID": "1", "GH_APP_KEY": key}, "GH_REPO is required"},
		"no key file": {map[string]string{"GH_APP_ID": "1", "GH_APP_KEY": "/tmp/nope.pem", "GH_REPO": "acme/demo"}, "no private key"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			for k, v := range tc.env {
				env[k] = v
			}
			out, code := runMinter(t, env)
			if code == 0 {
				t.Fatalf("expected failure, got success:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("error should say %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestMinterPrintModeNeedsNoTargetDirectory(t *testing.T) {
	key, bindir, _ := setupAppFixtures(t)
	out, code := runMinter(t, map[string]string{
		"PATH":       bindir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GH_APP_ID":  "42",
		"GH_APP_KEY": key,
		"GH_REPO":    "acme/demo",
	}, "--print")
	if code != 0 {
		t.Fatalf("--print failed (%d):\n%s", code, out)
	}
	if strings.TrimSpace(out) != "ghs_minted" {
		t.Errorf("--print output = %q", out)
	}
}

func TestTokenUnitsAreEmbedded(t *testing.T) {
	units, err := Units()
	if err != nil {
		t.Fatal(err)
	}
	svc, ok := units["issuewright-token.service"]
	if !ok {
		t.Fatalf("token service unit missing: %v", keys(units))
	}
	timer, ok := units["issuewright-token.timer"]
	if !ok {
		t.Fatal("token timer unit missing")
	}
	if !strings.Contains(string(svc), "gh-app-token.sh") {
		t.Error("token service does not run the minter")
	}
	if !strings.Contains(string(timer), "OnCalendar=*:0/30") {
		t.Error("token timer should refresh well inside the one-hour token lifetime")
	}
	// The main unit must be able to see the minted token.
	main, ok := units["issuewright@.service"]
	if !ok {
		t.Fatal("main unit missing")
	}
	if !strings.Contains(string(main), "EnvironmentFile=-/srv/agent/token.env") {
		t.Error("main unit does not load the minted token (and would ignore it)")
	}
}

// Running the minter by hand on a provisioned box failed with "GH_APP_ID is
// required" even though gh-app.env had it: the unit supplies that file, a human
// shell does not. The script must read it itself.
func TestMinterReadsTheEnvFileWhenByHand(t *testing.T) {
	key, bindir, _ := setupAppFixtures(t)
	envFile := filepath.Join(t.TempDir(), "gh-app.env")
	if err := os.WriteFile(envFile, []byte(
		"GH_APP_ID=4242\nGH_REPO=acme/demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := runMinter(t, map[string]string{
		"PATH":            bindir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GH_APP_KEY":      key,
		"GH_APP_ENV_FILE": envFile,
	}, "--print")
	if code != 0 {
		t.Fatalf("minter failed (%d):\n%s", code, out)
	}
	if strings.TrimSpace(out) != "ghs_minted" {
		t.Errorf("output = %q", out)
	}
}

func TestMinterPrefersTheEnvironmentOverTheFile(t *testing.T) {
	key, bindir, calls := setupAppFixtures(t)
	envFile := filepath.Join(t.TempDir(), "gh-app.env")
	if err := os.WriteFile(envFile, []byte("GH_APP_ID=999999\nGH_REPO=ignored/repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := runMinter(t, map[string]string{
		"PATH":            bindir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GH_APP_ID":       "42",
		"GH_REPO":         "acme/demo",
		"GH_APP_KEY":      key,
		"GH_APP_ENV_FILE": envFile,
	}, "--print")
	if code != 0 {
		t.Fatalf("minter failed (%d):\n%s", code, out)
	}
	log, _ := os.ReadFile(calls)
	if !strings.Contains(string(log), "repos/acme/demo/installation") {
		t.Errorf("the environment was overridden by the file:\n%s", log)
	}
	if strings.Contains(string(log), "ignored/repo") {
		t.Errorf("the file won over the environment:\n%s", log)
	}
}
