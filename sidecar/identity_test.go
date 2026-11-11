package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openziti/identity"
	"github.com/openziti/sdk-golang/ziti"
)

func fakeConfig() *ziti.Config {
	return &ziti.Config{
		ZtAPI: "https://ctrl.example.invalid/edge/client/v1",
		ID:    identity.Config{Cert: "pem:cert", Key: "pem:key", CA: "pem:ca"},
	}
}

func failEnroll(t *testing.T) enrollFunc {
	return func(string, string) (*ziti.Config, error) {
		t.Error("enroll must not be called")
		return nil, errors.New("unexpected")
	}
}

func TestLoadExistingIdentityIgnoresJWT(t *testing.T) {
	dir := t.TempDir()
	idFile := filepath.Join(dir, "identity.json")
	if err := writeIdentity(idFile, fakeConfig()); err != nil {
		t.Fatal(err)
	}
	missingJWT := filepath.Join(dir, "does-not-exist.jwt")
	cfg, err := loadOrEnroll(idFile, missingJWT, "EC", failEnroll(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ZtAPI != fakeConfig().ZtAPI {
		t.Errorf("ZtAPI = %q", cfg.ZtAPI)
	}
}

func TestMissingIdentityAndJWT(t *testing.T) {
	dir := t.TempDir()
	idFile := filepath.Join(dir, "identity.json")

	_, err := loadOrEnroll(idFile, "", "EC", failEnroll(t))
	if err == nil || !strings.Contains(err.Error(), idFile) || !strings.Contains(err.Error(), "jwt") {
		t.Fatalf("unclear error: %v", err)
	}

	jwtFile := filepath.Join(dir, "missing.jwt")
	_, err = loadOrEnroll(idFile, jwtFile, "EC", failEnroll(t))
	if err == nil || !strings.Contains(err.Error(), idFile) || !strings.Contains(err.Error(), jwtFile) {
		t.Fatalf("error should name both paths: %v", err)
	}

	// An empty identity file counts as missing.
	if err := os.WriteFile(idFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = loadOrEnroll(idFile, jwtFile, "EC", failEnroll(t))
	if err == nil || !strings.Contains(err.Error(), jwtFile) {
		t.Fatalf("empty identity file should fall through to JWT: %v", err)
	}
}

func TestEmptyJWTFileIsRejected(t *testing.T) {
	dir := t.TempDir()
	jwtFile := filepath.Join(dir, "a.jwt")
	if err := os.WriteFile(jwtFile, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadOrEnroll(filepath.Join(dir, "id.json"), jwtFile, "EC", failEnroll(t))
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-JWT error, got %v", err)
	}
}

// testJWT is an opaque placeholder; the fake enroll function never parses it.
const testJWT = "header.payload.signature"

func TestEnrollmentWritesIdentity(t *testing.T) {
	dir := t.TempDir()
	jwtFile := filepath.Join(dir, "a.jwt")
	if err := os.WriteFile(jwtFile, []byte(testJWT+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	idFile := filepath.Join(dir, "sub", "dir", "identity.json")

	var gotAlg string
	var gotToken string
	enrollFn := func(token, alg string) (*ziti.Config, error) {
		gotToken, gotAlg = token, alg
		return fakeConfig(), nil
	}
	cfg, err := loadOrEnroll(idFile, jwtFile, "RSA", enrollFn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || gotAlg != "RSA" || gotToken != testJWT {
		t.Fatalf("cfg=%v alg=%q token=%q", cfg, gotAlg, gotToken)
	}
	fi, err := os.Stat(idFile)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("identity mode = %v", fi.Mode().Perm())
	}
	if di, _ := os.Stat(filepath.Dir(idFile)); di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v", di.Mode().Perm())
	}
	if _, err := os.Stat(idFile + ".tmp"); !os.IsNotExist(err) {
		t.Error(".tmp file left behind")
	}
	if _, err := os.Stat(jwtFile); err != nil {
		t.Error("JWT file must not be deleted")
	}
	// Second run reuses the identity without enrolling.
	if _, err := loadOrEnroll(idFile, jwtFile, "EC", failEnroll(t)); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentFailureAndWriteFailure(t *testing.T) {
	dir := t.TempDir()
	jwtFile := filepath.Join(dir, "a.jwt")
	if err := os.WriteFile(jwtFile, []byte(testJWT), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadOrEnroll(filepath.Join(dir, "id.json"), jwtFile, "EC", func(string, string) (*ziti.Config, error) {
		return nil, errors.New("boom")
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}

	// Make the identity path unwritable: parent is a regular file.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = loadOrEnroll(filepath.Join(blocker, "id.json"), jwtFile, "EC", func(string, string) (*ziti.Config, error) {
		return fakeConfig(), nil
	})
	if err == nil || !strings.Contains(err.Error(), "consumed") || !strings.Contains(err.Error(), "new one must be issued") {
		t.Fatalf("write failure must explain the consumed JWT: %v", err)
	}
	if _, err := os.Stat(jwtFile); err != nil {
		t.Error("JWT file must be kept")
	}
}

func TestParseConfig(t *testing.T) {
	t.Setenv("ZITI_IDENTITY_FILE", "/env/id.json")
	t.Setenv("ZITI_ALLOWED_SERVICES", "a, b")
	c, err := parseConfig([]string{"--dial-timeout", "3s"})
	if err != nil {
		t.Fatal(err)
	}
	if c.identityFile != "/env/id.json" || len(c.allowed) != 2 || c.dialTimeout.Seconds() != 3 || c.listen != "127.0.0.1:8900" || c.keyAlg != "EC" {
		t.Errorf("%+v", c)
	}
	c, err = parseConfig([]string{"--identity-file", "/flag/id.json"})
	if err != nil || c.identityFile != "/flag/id.json" {
		t.Errorf("flag should win: %+v %v", c, err)
	}
	for _, args := range [][]string{
		{"--key-alg", "DSA"},
		{"--dial-timeout", "x"},
		{"--listen", "nonsense"},
		{"--listen", "unix://rel"},
		{"--insecure-skip-verify", "maybe"},
	} {
		if _, err := parseConfig(append([]string{"--identity-file", "/x"}, args...)); err == nil {
			t.Errorf("expected error for %v", args)
		}
	}
	t.Setenv("ZITI_IDENTITY_FILE", "")
	if _, err := parseConfig(nil); err == nil {
		t.Error("identity file must be required")
	}
}
