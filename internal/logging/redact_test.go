package logging

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestSecretsNeverReachTheLog(t *testing.T) {
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	const keyLine = "MIIEowIBAAKCAQEAuS3cretKeyMaterialLine"
	key := "-----BEGIN RSA PRIVATE KEY-----\n" + keyLine + "\n-----END RSA PRIVATE KEY-----"
	const jit = "eyJhbGciOiJIUzI1NiJ9.jitconfigpayload"

	r := NewRedactor()
	r.Add(token)
	r.Add(key)
	r.Add(jit)

	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelDebug, r).With("auth", "Bearer "+token)

	// The shape of an error the scaleset client returns for a failed request.
	reqErr := fmt.Errorf("request POST https://api.github.com/x failed: %w", errors.New("bad credentials "+token))
	log.Error("request failed "+jit, "error", reqErr, slog.Group("g", "key", key, "frag", "…"+keyLine+"…"))
	log.Debug("debug", "raw", []byte(jit), "any", map[string]string{"t": token})

	out := buf.String()
	for _, s := range []string{token, keyLine, jit} {
		if strings.Contains(out, s) {
			t.Fatalf("secret %q leaked into log:\n%s", s, out)
		}
	}
	if !strings.Contains(out, placeholder) {
		t.Fatalf("expected placeholder in log:\n%s", out)
	}

	r.Remove(jit)
	buf.Reset()
	log.Info(jit)
	if !strings.Contains(buf.String(), jit) {
		t.Fatalf("removed secret should no longer be redacted")
	}
}
