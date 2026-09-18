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

func TestCommitURL(t *testing.T) {
	const hash = "d340f16ba1256ec563d7b08c0396645d555e65b8"

	tests := []struct {
		name, repoURL, hash, want string
	}{
		{"github", "https://github.com/intro-skipper/manifest", hash, "https://github.com/intro-skipper/manifest/commit/" + hash},
		{"trailing slash", "https://github.com/intro-skipper/manifest/", hash, "https://github.com/intro-skipper/manifest/commit/" + hash},
		{"missing url", "", hash, ""},
		{"enterprise host", "https://git.example.com/org/repo", hash, "https://git.example.com/org/repo/commit/" + hash},
		{"non-https url", "javascript:alert(1)", hash, ""},
		{"http url", "http://github.com/intro-skipper/manifest", hash, ""},
		{"no host", "https:///intro-skipper/manifest", hash, ""},
		{"userinfo", "https://user:pw@github.com/intro-skipper/manifest", hash, ""},
		{"query", "https://github.com/intro-skipper/manifest?x=1", hash, ""},
		{"fragment", "https://github.com/intro-skipper/manifest#x", hash, ""},
		{"closing paren", "https://evil.example/x)", hash, ""},
		{"markdown break", "https://evil.example/x](https://other", hash, ""},
		{"whitespace", "https://evil.example/x y", hash, ""},
		{"control char", "https://evil.example/x\n", hash, ""},
		{"bad hash", "https://github.com/intro-skipper/manifest", "not-a-hash", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := commitURL(tc.repoURL, tc.hash); got != tc.want {
				t.Fatalf("commitURL(%q, %q) = %q, want %q", tc.repoURL, tc.hash, got, tc.want)
			}
		})
	}
}

func TestFormatCommitRef(t *testing.T) {
	const hash = "d340f16ba1256ec563d7b08c0396645d555e65b8"

	if got, want := formatCommitRef(hash, ""), "`d340f16`"; got != want {
		t.Fatalf("formatCommitRef without link = %q, want %q", got, want)
	}
	link := "https://github.com/intro-skipper/manifest/commit/" + hash
	if got, want := formatCommitRef(hash, link), "[`d340f16`]("+link+")"; got != want {
		t.Fatalf("formatCommitRef with link = %q, want %q", got, want)
	}
}

func TestGithubRepoURL(t *testing.T) {
	if got, want := githubRepoURL("intro-skipper", "manifest"), "https://github.com/intro-skipper/manifest"; got != want {
		t.Fatalf("githubRepoURL = %q, want %q", got, want)
	}
	if got := githubRepoURL("", "manifest"); got != "" {
		t.Fatalf("githubRepoURL without owner = %q, want empty", got)
	}
	if got := githubRepoURL("intro-skipper", ""); got != "" {
		t.Fatalf("githubRepoURL without repo = %q, want empty", got)
	}
	if got, want := githubRepoURL("a/b", "c d"), "https://github.com/a%2Fb/c%20d"; got != want {
		t.Fatalf("githubRepoURL escaping = %q, want %q", got, want)
	}
}
