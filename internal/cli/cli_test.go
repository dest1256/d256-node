package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func exec(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, env{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errb})
	return code, out.String(), errb.String()
}

func TestBasicCommands(t *testing.T) {
	cases := []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{nil, 2, "", "Usage:"},
		{[]string{"version"}, 0, "protocol 1", ""},
		{[]string{"help"}, 0, "Commands:", ""},
		{[]string{"bogus"}, 2, "", "unknown command"},
	}
	for _, c := range cases {
		code, out, errs := exec(t, "", c.args...)
		if code != c.code || !strings.Contains(out, c.stdout) || !strings.Contains(errs, c.stderr) {
			t.Errorf("%v: code=%d stdout=%q stderr=%q", c.args, code, out, errs)
		}
	}
}

func TestEnrollRequiresHTTPSAndToken(t *testing.T) {
	dir := t.TempDir()
	code, _, errs := exec(t, "", "enroll", "--state-dir", dir, "http://panel.example.com", "tok")
	if code == 0 || !strings.Contains(errs, "plain http") {
		t.Fatalf("http must be refused: %d %q", code, errs)
	}
	code, _, errs = exec(t, "", "enroll", "--state-dir", dir, "https://panel.example.com", "-")
	if code == 0 || !strings.Contains(errs, "token is empty") {
		t.Fatalf("empty token must be refused: %d %q", code, errs)
	}
}

func TestEnrollStatusResetFlow(t *testing.T) {
	var gotToken, gotCSR string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Token, CSR string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotToken, gotCSR = req.Token, req.CSR
		if r.URL.Path != "/base/d256/enroll" || req.Token != "7.secret" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"invalid or expired enrollment token"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"nodeId": 7, "cert": "CERT", "ca": "CA", "grpcAddr": "panel.example.com:8444"})
	}))
	defer srv.Close()
	dir := filepath.Join(t.TempDir(), "state")
	url := srv.URL + "/base/"

	code, _, errs := exec(t, "", "enroll", "--state-dir", dir, "--allow-insecure-http", url, "wrong")
	if code == 0 || !strings.Contains(errs, "invalid or expired") {
		t.Fatalf("wrong token: %d %q", code, errs)
	}
	if code, _, errs = exec(t, "7.secret\n", "enroll", "--state-dir", dir, "--allow-insecure-http", url, "-"); code != 0 {
		t.Fatalf("enroll failed: %q", errs)
	}
	if gotToken != "7.secret" || !strings.Contains(gotCSR, "CERTIFICATE REQUEST") {
		t.Fatalf("server saw token=%q csr=%q", gotToken, gotCSR)
	}
	st, err := os.Stat(filepath.Join(dir, "client.key"))
	if err != nil {
		t.Fatal(err)
	}
	if runtimeIsUnix() && st.Mode().Perm() != 0o600 {
		t.Fatalf("client.key mode = %v", st.Mode().Perm())
	}
	if code, _, errs = exec(t, "7.secret\n", "enroll", "--state-dir", dir, "--allow-insecure-http", url, "-"); code == 0 || !strings.Contains(errs, "already enrolled") {
		t.Fatalf("re-enroll without --force: %d %q", code, errs)
	}
	code, out, _ := exec(t, "", "status", "--state-dir", dir)
	if code != 0 || !strings.Contains(out, "node 7") || !strings.Contains(out, "not running") {
		t.Fatalf("status: %d %q", code, out)
	}
	if code, _, _ = exec(t, "", "reset", "--state-dir", dir); code == 0 {
		t.Fatal("reset must require --yes")
	}
	if code, _, errs = exec(t, "", "reset", "--state-dir", dir, "--yes"); code != 0 {
		t.Fatalf("reset: %q", errs)
	}
	if code, _, _ = exec(t, "", "run", "--state-dir", dir); code == 0 {
		t.Fatal("run must fail when not enrolled")
	}
}

func TestLogsTail(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node.log"), []byte("a\nb\nc\nd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := exec(t, "", "logs", "--state-dir", dir, "-n", "2")
	if code != 0 || out != "c\nd\n" {
		t.Fatalf("logs: %d %q", code, out)
	}
}

func runtimeIsUnix() bool { return os.PathSeparator == '/' }
