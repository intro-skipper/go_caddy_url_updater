package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestWebhookHandler covers the signature check that replaced go-github's
// ValidatePayload: no delivery may reach the Caddyfile without a valid HMAC.
func TestWebhookHandler(t *testing.T) {
	const secret = "s3cr3t"

	dir := t.TempDir()
	path := filepath.Join(dir, "Caddyfile")
	oldHash := strings.Repeat("a", 40)
	if err := os.WriteFile(path, []byte("vars {\n\tcommit_hash \""+oldHash+"\"\n}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	caddyFilePath = path
	// Point the Docker client at nothing, so the reload fails deterministically
	// instead of reaching a daemon that happens to be running.
	dockerSock = filepath.Join(dir, "absent.sock")

	newHash := strings.Repeat("b", 40)
	body := `{"ref":"refs/heads/main","after":"` + newHash + `","deleted":false,"repository":{"full_name":"intro-skipper/manifest"}}`

	do := func(method, event, signature, payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/hook", strings.NewReader(payload))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-GitHub-Delivery", "test-delivery")
		if signature != "" {
			req.Header.Set("X-Hub-Signature-256", signature)
		}
		rec := httptest.NewRecorder()
		webhookHandler(secret)(rec, req)
		return rec
	}

	hashInFile := func() string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	t.Run("rejects bad signatures", func(t *testing.T) {
		for name, signature := range map[string]string{
			"signed with another secret":  sign("other", body),
			"body tampered after signing": sign(secret, body+" "),
			"header absent":               "",
			"empty signature":             "sha256=",
			"not hex":                     "sha256=zzzz",
			"truncated":                   sign(secret, body)[:20],
			"prefix missing":              strings.TrimPrefix(sign(secret, body), "sha256="),
		} {
			if rec := do(http.MethodPost, "push", signature, body); rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: got %d, want 401", name, rec.Code)
			}
		}
		if got := hashInFile(); !strings.Contains(got, oldHash) {
			t.Fatalf("unauthenticated request modified the Caddyfile: %q", got)
		}
	})

	t.Run("rejects non-POST", func(t *testing.T) {
		rec := do(http.MethodGet, "push", sign(secret, body), body)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("got %d, want 405", rec.Code)
		}
	})

	t.Run("ignores other events", func(t *testing.T) {
		ping := `{"zen":"hi"}`
		if rec := do(http.MethodPost, "ping", sign(secret, ping), ping); rec.Code != http.StatusNoContent {
			t.Fatalf("got %d, want 204", rec.Code)
		}
	})

	t.Run("rejects malformed payload", func(t *testing.T) {
		bad := `{"ref":`
		if rec := do(http.MethodPost, "push", sign(secret, bad), bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400", rec.Code)
		}
	})

	t.Run("ignores deletions and other branches", func(t *testing.T) {
		for name, payload := range map[string]string{
			"branch deleted": `{"ref":"refs/heads/main","after":"` + strings.Repeat("0", 40) + `","deleted":true}`,
			"other branch":   `{"ref":"refs/heads/dev","after":"` + newHash + `"}`,
			"not a hash":     `{"ref":"refs/heads/main","after":"nonsense"}`,
		} {
			if rec := do(http.MethodPost, "push", sign(secret, payload), payload); rec.Code != http.StatusNoContent {
				t.Errorf("%s: got %d, want 204", name, rec.Code)
			}
		}
		if got := hashInFile(); !strings.Contains(got, oldHash) {
			t.Fatalf("ignored push modified the Caddyfile: %q", got)
		}
	})

	t.Run("accepts a signed push", func(t *testing.T) {
		// The reload fails with no Docker socket, which surfaces as a 500.
		rec := do(http.MethodPost, "push", sign(secret, body), body)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", rec.Code)
		}
		if got := hashInFile(); !strings.Contains(got, newHash) {
			t.Fatalf("Caddyfile not updated: %q", got)
		}
	})
}
