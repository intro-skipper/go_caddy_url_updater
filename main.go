package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	caddyFilePath  = getEnv("CADDYFILE_PATH", "/etc/caddy/Caddyfile")
	caddyContainer = getEnv("CADDY_CONTAINER", "caddy") // container NAME, not ID
	dockerSock     = getEnv("DOCKER_SOCK", "/var/run/docker.sock")
	discordWebhook = strings.TrimSpace(getEnv("DISCORD_WEBHOOK_URL", ""))
	location       = strings.TrimSpace(getEnv("LOCATION", ""))
	githubOwner    = strings.TrimSpace(getEnv("GITHUB_REPO_OWNER", "intro-skipper"))
	githubRepo     = strings.TrimSpace(getEnv("GITHUB_REPO_NAME", "manifest"))
	githubBranch   = strings.TrimSpace(getEnv("GITHUB_REPO_BRANCH", "main"))
	githubToken    = strings.TrimSpace(getEnv("GITHUB_TOKEN", ""))

	commitHashRe    = regexp.MustCompile(`(commit_hash\s+")[a-fA-F0-9]{40}(")`)
	commitExtractRe = regexp.MustCompile(`commit_hash\s+"([a-fA-F0-9]{40})"`)
	commitSHARe     = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

	// nullCommit is what GitHub sends as "after" when a branch is deleted.
	nullCommit = strings.Repeat("0", 40)

	// caddyMu serializes the Caddyfile read-modify-write and the reload that
	// follows it: net/http serves one goroutine per delivery, so two pushes
	// arriving together would otherwise interleave.
	caddyMu sync.Mutex
)

func getEnv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	webhookSecret := strings.TrimSpace(os.Getenv("GITHUB_SECRETKEY"))
	if webhookSecret == "" {
		log.Fatal("GITHUB_SECRETKEY is required: refusing to start without a webhook secret")
	}

	checkCommitUpToDate(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/hook", webhookHandler(webhookSecret))

	srv := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	if err := srv.ListenAndServe(); err != nil {
		panic(err)
	}
}

// pushEvent is the subset of GitHub's push payload this service needs. Keeping
// our own struct means no dependency on a go-github major version, which the
// upstream webhook libraries change on every release.
type pushEvent struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// maxWebhookBody caps the request body: GitHub does not deliver payloads larger
// than 25 MB.
const maxWebhookBody = 25 << 20

func webhookHandler(secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
		if err != nil {
			log.Println("webhook: read body:", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if err := verifySignature(secret, body, r.Header.Get("X-Hub-Signature-256")); err != nil {
			log.Println("webhook: rejected delivery:", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		deliveryID := r.Header.Get("X-GitHub-Delivery")
		if eventName := r.Header.Get("X-GitHub-Event"); eventName != "push" {
			log.Printf("webhook: ignoring %q event. Delivery: %s", eventName, deliveryID)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		var event pushEvent
		if err := json.Unmarshal(body, &event); err != nil {
			log.Println("webhook: decode push payload:", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if err := handlePush(r.Context(), event); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// verifySignature checks the HMAC GitHub sends with every delivery. The
// comparison is constant time so it cannot be probed byte by byte.
func verifySignature(secret string, body []byte, header string) error {
	const prefix = "sha256="

	if !strings.HasPrefix(header, prefix) {
		return errors.New("missing or malformed X-Hub-Signature-256 header")
	}

	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return errors.New("signature is not valid hex")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(want, mac.Sum(nil)) {
		return errors.New("signature mismatch")
	}

	return nil
}

func handlePush(ctx context.Context, event pushEvent) error {
	// Only act on pushes to main branch
	if !strings.EqualFold(event.Ref, "refs/heads/main") {
		log.Println("Push event is not for main branch. Ref:", event.Ref)
		return nil
	}

	if event.Deleted {
		log.Println("Push event deletes the branch. Ignoring. Ref:", event.Ref)
		return nil
	}

	newHash := event.After
	if err := validateCommitHash(newHash); err != nil {
		log.Println("Ignoring push event:", err)
		return nil
	}

	log.Println("Push received. Commit:", newHash)

	repoName := event.Repository.FullName
	if repoName == "" {
		repoName = "unknown"
	}

	updateErr, reloadErr := applyCommit(newHash)
	if updateErr != nil {
		log.Println("Failed to update Caddyfile:", updateErr)
	} else {
		log.Println("Caddyfile updated successfully.")
	}
	if reloadErr != nil {
		log.Println("Failed to reload Caddy:", reloadErr)
	} else {
		log.Println("Caddy reloaded successfully.")
	}

	reportDiscordOutcome(ctx, repoName, newHash, updateErr, reloadErr)

	// Both failures have to reach the response: a rewrite that failed while the
	// reload succeeded would otherwise show up as a successful delivery.
	return errors.Join(updateErr, reloadErr)
}

// validateCommitHash rejects anything that is not a real commit id before it
// reaches the Caddyfile: an empty or malformed "after" field, and the null
// commit GitHub sends for branch deletions.
func validateCommitHash(hash string) error {
	if !commitSHARe.MatchString(hash) {
		return fmt.Errorf("not a commit hash: %q", truncateForDiscord(hash, 64))
	}
	if strings.EqualFold(hash, nullCommit) {
		return errors.New("refusing to write the null commit hash")
	}
	return nil
}

// applyCommit rewrites the Caddyfile and reloads Caddy under caddyMu. The
// reload runs even when the rewrite failed, so a Caddyfile edited by hand still
// takes effect.
func applyCommit(newHash string) (updateErr, reloadErr error) {
	caddyMu.Lock()
	defer caddyMu.Unlock()

	updateErr = updateCaddyfile(newHash)
	reloadErr = reloadCaddyInContainer(dockerSock, caddyContainer)
	return updateErr, reloadErr
}

// syncToRemoteHead is the startup variant of applyCommit: it skips the reload
// when the Caddyfile could not be updated.
func syncToRemoteHead(remoteHash string) (updateErr, reloadErr error) {
	caddyMu.Lock()
	defer caddyMu.Unlock()

	if err := updateCaddyfile(remoteHash); err != nil {
		return err, nil
	}
	log.Println("Caddyfile updated to remote head.")

	return nil, reloadCaddyInContainer(dockerSock, caddyContainer)
}

func updateCaddyfile(newHash string) error {
	if err := validateCommitHash(newHash); err != nil {
		return err
	}

	data, err := os.ReadFile(caddyFilePath)
	if err != nil {
		return err
	}

	content := string(data)

	// Pattern: commit_hash variable in vars block, e.g.
	// commit_hash "d340f16ba1256ec563d7b08c0396645d555e65b8"
	if !commitHashRe.MatchString(content) {
		return fmt.Errorf("no commit_hash entry found in %s", caddyFilePath)
	}

	// newHash is 40 hex digits (validated above), so it cannot be mistaken for
	// an expansion reference in the replacement template.
	updated := commitHashRe.ReplaceAllString(content, fmt.Sprintf("${1}%s${2}", newHash))
	if updated == content {
		// Already at newHash.
		return nil
	}

	return writeFileAtomic(caddyFilePath, []byte(updated))
}

// writeFileAtomic replaces path with a rename so an interrupted write can never
// leave Caddy with a truncated config. A single-file bind mount cannot be
// replaced that way, so that case falls back to an in-place write.
func writeFileAtomic(path string, data []byte) error {
	perm := os.FileMode(0644)
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		// Mounting the directory instead of the file makes this path atomic.
		log.Printf("Atomic replace of %s failed (%v); writing in place", path, err)
		return os.WriteFile(path, data, perm)
	}

	return nil
}

func reloadCaddyInContainer(sockPath, containerName string) error {
	client := httpClientForUnixSocket(sockPath)

	resp, err := client.Get("http://unix/containers/json")
	if err != nil {
		return fmt.Errorf("docker list containers: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("docker list containers failed: %s", string(b))
	}

	var containers []struct {
		ID    string   `json:"Id"`
		Names []string `json:"Names"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return fmt.Errorf("decode containers list: %w", err)
	}

	var containerID string
	for _, c := range containers {
		for _, n := range c.Names {
			if strings.TrimPrefix(n, "/") == containerName {
				containerID = c.ID
				break
			}
		}
		if containerID != "" {
			break
		}
	}
	if containerID == "" {
		return fmt.Errorf("container %q not found", containerName)
	}

	type createExecReq struct {
		AttachStdout bool     `json:"AttachStdout"`
		AttachStderr bool     `json:"AttachStderr"`
		Cmd          []string `json:"Cmd"`
	}
	reqBody := createExecReq{
		AttachStdout: false,
		AttachStderr: false,
		Cmd:          []string{"caddy", "reload", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"},
	}
	body, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("http://unix/containers/%s/exec", containerID)
	execResp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("docker create exec: %w", err)
	}
	defer execResp.Body.Close()
	if execResp.StatusCode >= 400 {
		b, _ := io.ReadAll(execResp.Body)
		return fmt.Errorf("docker create exec failed: %s", string(b))
	}

	var createResp struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(execResp.Body).Decode(&createResp); err != nil {
		return fmt.Errorf("decode create exec resp: %w", err)
	}
	if createResp.ID == "" {
		return errors.New("empty exec id")
	}

	startURL := fmt.Sprintf("http://unix/exec/%s/start", createResp.ID)
	startReq := map[string]bool{"Detach": true, "Tty": false}
	startBody, _ := json.Marshal(startReq)
	startResp, err := client.Post(startURL, "application/json", bytes.NewReader(startBody))
	if err != nil {
		return fmt.Errorf("docker start exec: %w", err)
	}
	defer startResp.Body.Close()
	if startResp.StatusCode >= 400 {
		b, _ := io.ReadAll(startResp.Body)
		return fmt.Errorf("docker start exec failed: %s", string(b))
	}

	return waitForExec(client, createResp.ID)
}

// waitForExec blocks until the exec finished, so a failing "caddy reload" (an
// invalid Caddyfile, for example) is reported instead of counted as a success.
func waitForExec(client *http.Client, execID string) error {
	deadline := time.Now().Add(30 * time.Second)

	for {
		running, exitCode, err := inspectExec(client, execID)
		if err != nil {
			return err
		}
		if !running {
			if exitCode != 0 {
				return fmt.Errorf("caddy reload exited with code %d", exitCode)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for caddy reload to finish")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func inspectExec(client *http.Client, execID string) (running bool, exitCode int, err error) {
	resp, err := client.Get(fmt.Sprintf("http://unix/exec/%s/json", execID))
	if err != nil {
		return false, 0, fmt.Errorf("docker inspect exec: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return false, 0, fmt.Errorf("docker inspect exec failed: %s", strings.TrimSpace(string(b)))
	}

	var state struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return false, 0, fmt.Errorf("decode inspect exec resp: %w", err)
	}

	return state.Running, state.ExitCode, nil
}

func httpClientForUnixSocket(sockPath string) *http.Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return net.Dial("unix", sockPath)
		},
	}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func reportDiscordOutcome(ctx context.Context, repoName, commitHash string, updateErr, reloadErr error) {
	if discordWebhook == "" {
		return
	}

	success := updateErr == nil && reloadErr == nil

	embed := discordEmbed{
		Title:       fmt.Sprintf("Caddy Update • %s", repoName),
		Description: fmt.Sprintf("Commit `%s` processed.", shortCommit(commitHash)),
		Color:       embedColor(success),
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}

	embed.Fields = appendLocationField(embed.Fields)
	embed.Fields = append(embed.Fields,
		discordEmbedField{
			Name:   "Caddyfile Update",
			Value:  formatDiscordStatus(updateErr, "Caddyfile updated."),
			Inline: false,
		},
		discordEmbedField{
			Name:   "Caddy Reload",
			Value:  formatDiscordStatus(reloadErr, "Caddy reloaded."),
			Inline: false,
		},
	)

	payload := discordPayload{Embeds: []discordEmbed{embed}}

	if err := sendDiscordMessage(ctx, payload); err != nil {
		log.Println("Failed to notify Discord:", err)
	}
}

func shortCommit(hash string) string {
	if len(hash) >= 7 {
		return hash[:7]
	}
	return hash
}

type discordPayload struct {
	Content string         `json:"content,omitempty"`
	Embeds  []discordEmbed `json:"embeds,omitempty"`
}

type discordEmbed struct {
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Color       int                 `json:"color,omitempty"`
	Timestamp   string              `json:"timestamp,omitempty"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name,omitempty"`
	Value  string `json:"value,omitempty"`
	Inline bool   `json:"inline,omitempty"`
}

func embedColor(success bool) int {
	if success {
		return 0x57F287
	}
	return 0xED4245
}

func formatDiscordStatus(err error, successMsg string) string {
	if err != nil {
		return truncateForDiscord(fmt.Sprintf("❌ %v", err), 1000)
	}
	return fmt.Sprintf("✅ %s", successMsg)
}

// truncateForDiscord shortens s to limit characters. Discord counts characters,
// and cutting bytes would split a multi-byte rune in half.
func truncateForDiscord(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}

	runes := []rune(s)
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}

func sendDiscordMessage(ctx context.Context, payload discordPayload) error {
	if discordWebhook == "" {
		return nil
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal discord payload: %w", err)
	}

	// The webhook request context is cancelled as soon as GitHub gives up on the
	// delivery, which is precisely when the failure notification matters most.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, discordWebhook, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create discord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("send discord request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("discord webhook returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	return nil
}

func checkCommitUpToDate(ctx context.Context) {
	hash, err := commitFromCaddyfile(caddyFilePath)
	if err != nil {
		log.Println("Commit check skipped:", err)
		reportDiscordStartup(ctx, "", "", false, err, nil, nil)
		return
	}

	if githubOwner == "" || githubRepo == "" || githubBranch == "" {
		err := errors.New("repository metadata incomplete")
		log.Println("Commit check skipped:", err)
		reportDiscordStartup(ctx, hash, "", false, err, nil, nil)
		return
	}

	remoteHash, err := fetchRemoteHead(ctx, githubOwner, githubRepo, githubBranch)
	if err != nil {
		log.Println("Commit check failed:", err)
		reportDiscordStartup(ctx, hash, "", false, err, nil, nil)
		return
	}

	if strings.EqualFold(remoteHash, hash) {
		log.Printf("Caddyfile commit %s matches %s/%s@%s", shortCommit(hash), githubOwner, githubRepo, githubBranch)
		reportDiscordStartup(ctx, hash, remoteHash, false, nil, nil, nil)
		return
	}

	log.Printf("Caddyfile commit %s differs from remote head %s (%s/%s@%s) — updating", shortCommit(hash), shortCommit(remoteHash), githubOwner, githubRepo, githubBranch)

	updateErr, reloadErr := syncToRemoteHead(remoteHash)
	if updateErr != nil {
		log.Println("Startup update failed:", updateErr)
		reportDiscordStartup(ctx, hash, remoteHash, true, nil, updateErr, nil)
		return
	}
	hash = remoteHash

	if reloadErr != nil {
		log.Println("Startup reload failed:", reloadErr)
		reportDiscordStartup(ctx, hash, remoteHash, true, nil, nil, reloadErr)
		return
	}

	log.Println("Caddy reloaded after startup update.")
	reportDiscordStartup(ctx, hash, remoteHash, true, nil, nil, nil)
}

func commitFromCaddyfile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read caddyfile: %w", err)
	}

	// Extract commit hash from the commit_hash variable in vars block.
	match := commitExtractRe.FindStringSubmatch(string(data))
	if len(match) != 2 {
		return "", errors.New("no commit hash found in Caddyfile")
	}

	return strings.ToLower(match[1]), nil
}

func fetchRemoteHead(ctx context.Context, owner, repo, branch string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/commits/%s", owner, repo, branch)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "go-caddy-url-updater")
	if githubToken != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", githubToken))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("github commit lookup returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	var payload struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode github response: %w", err)
	}
	if err := validateCommitHash(payload.SHA); err != nil {
		return "", fmt.Errorf("github response: %w", err)
	}

	return strings.ToLower(payload.SHA), nil
}

func reportDiscordStartup(ctx context.Context, localHash, remoteHash string, attempted bool, metaErr, updateErr, reloadErr error) {
	if discordWebhook == "" {
		return
	}

	success := metaErr == nil && updateErr == nil && reloadErr == nil

	embed := discordEmbed{
		Title:     "Startup Sync",
		Color:     embedColor(success),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	embed.Fields = appendLocationField(embed.Fields)

	switch {
	case metaErr != nil:
		embed.Description = fmt.Sprintf("Commit alignment failed: %v", metaErr)
	case !attempted:
		embed.Description = "Caddyfile already matches GitHub head."
	case updateErr != nil:
		embed.Description = "Failed updating Caddyfile to match GitHub head."
	case reloadErr != nil:
		embed.Description = "Caddyfile updated but Caddy reload failed."
	default:
		embed.Description = "Caddyfile synchronized with GitHub head."
	}

	if localHash != "" {
		embed.Fields = append(embed.Fields, discordEmbedField{
			Name:   "Caddyfile Hash",
			Value:  fmt.Sprintf("`%s`", shortCommit(localHash)),
			Inline: true,
		})
	}
	if remoteHash != "" {
		embed.Fields = append(embed.Fields, discordEmbedField{
			Name:   "GitHub Head",
			Value:  fmt.Sprintf("`%s`", shortCommit(remoteHash)),
			Inline: true,
		})
	}

	if attempted {
		embed.Fields = append(embed.Fields, discordEmbedField{
			Name:   "Update",
			Value:  formatDiscordStatus(updateErr, "Caddyfile synchronized."),
			Inline: false,
		})
		if updateErr == nil {
			embed.Fields = append(embed.Fields, discordEmbedField{
				Name:   "Reload",
				Value:  formatDiscordStatus(reloadErr, "Caddy reloaded."),
				Inline: false,
			})
		}
	}

	payload := discordPayload{Embeds: []discordEmbed{embed}}

	if err := sendDiscordMessage(ctx, payload); err != nil {
		log.Println("Failed to notify Discord:", err)
	}
}

func appendLocationField(fields []discordEmbedField) []discordEmbedField {
	if location == "" {
		return fields
	}
	return append(fields, discordEmbedField{
		Name:   "Location",
		Value:  truncateForDiscord(location, 256),
		Inline: true,
	})
}
