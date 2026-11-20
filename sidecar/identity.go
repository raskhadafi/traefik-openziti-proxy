package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/sdk-golang/ziti/enroll"
)

// enrollFunc performs a one-time enrollment with the given JWT string and key
// algorithm. enrollWithSDK is the real implementation; tests replace it.
type enrollFunc func(token, keyAlg string) (*ziti.Config, error)

// enrollWithSDK parses the JWT (which verifies it against the controller's
// certificate, so it needs network access to the controller) and enrolls.
func enrollWithSDK(token, keyAlg string) (*ziti.Config, error) {
	claims, jwtToken, err := enroll.ParseToken(token)
	if err != nil {
		return nil, fmt.Errorf("parse enrollment JWT: %w", err)
	}
	flags := enroll.EnrollmentFlags{Token: claims, JwtToken: jwtToken}
	if err := flags.KeyAlg.Set(keyAlg); err != nil {
		return nil, fmt.Errorf("key algorithm %q: %w", keyAlg, err)
	}
	return enroll.Enroll(flags)
}

// loadOrEnroll returns the Ziti config stored in identityFile. If the file is
// missing or empty it enrolls with the JWT in jwtFile and persists the result.
func loadOrEnroll(identityFile, jwtFile, keyAlg string, enrollFn enrollFunc) (*ziti.Config, error) {
	if identityFile == "" {
		return nil, errors.New("identity file path is required")
	}
	if fi, err := os.Stat(identityFile); err == nil && fi.Size() > 0 {
		cfg, err := ziti.NewConfigFromFile(identityFile)
		if err != nil {
			return nil, fmt.Errorf("load identity file %q: %w", identityFile, err)
		}
		slog.Info("identity loaded", "file", identityFile)
		return cfg, nil
	}

	if jwtFile == "" {
		return nil, fmt.Errorf("identity file %q does not exist and no enrollment JWT file is configured (set --jwt-file)", identityFile)
	}
	raw, err := os.ReadFile(jwtFile)
	if err != nil {
		return nil, fmt.Errorf("identity file %q does not exist and the enrollment JWT file %q cannot be read: %w", identityFile, jwtFile, err)
	}
	tokenStr := strings.TrimSpace(string(raw))
	if tokenStr == "" {
		return nil, fmt.Errorf("identity file %q does not exist and the enrollment JWT file %q is empty", identityFile, jwtFile)
	}

	cfg, err := enrollFn(tokenStr, keyAlg)
	if err != nil {
		return nil, fmt.Errorf("enrollment failed: %w", err)
	}
	slog.Info("enrollment performed")

	if err := writeIdentity(identityFile, cfg); err != nil {
		return nil, fmt.Errorf("enrollment succeeded but the identity could not be saved to %q: the JWT in %q is now consumed and a new one must be issued: %w", identityFile, jwtFile, err)
	}
	return cfg, nil
}

func writeIdentity(path string, cfg *ziti.Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// WriteFile does not change the mode of a pre-existing file.
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
