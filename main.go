package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/cbrgm/githubevents/v2/githubevents"
	"github.com/google/go-github/v84/github"
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

	commitHashRe = regexp.MustCompile(`(commit_hash\s+")[a-fA-F0-9]{40}(")`)
	commitExtractRe = regexp.MustCompile(`commit_hash\s+"([a-fA-F0-9]{40})"`)
)

func getEnv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	checkCommitUpToDate(context.Background())

	handle := githubevents.New(getEnv("GITHUB_SECRETKEY", "secret"))

	handle.OnPushEventAny(func(ctx context.Context, deliveryID string, eventName string, event *github.PushEvent) error {
		newHash := event.GetAfter()

		ref := event.GetRef()

		// Only act on pushes to main branch
		if !strings.EqualFold(ref, "refs/heads/main") {
			log.Println("Push event is not for main branch. Ref:", ref)
			return nil
		}

		log.Println("Push received. Commit:", newHash)

		repoName := "unknown"
		if repo := event.GetRepo(); repo != nil {
			repoName = repo.GetFullName()
		}

		updateErr := updateCaddyfile(newHash)
		if updateErr != nil {
			log.Println("Failed to update Caddyfile:", updateErr)
		} else {
			log.Println("Caddyfile updated successfully.")
		}

		reloadErr := reloadCaddyInContainer(dockerSock, caddyContainer)
		if reloadErr != nil {
			log.Println("Failed to trigger Caddy reload:", reloadErr)
		} else {
			log.Println("Caddy reload triggered successfully.")
		}

		reportDiscordOutcome(ctx, repoName, newHash, updateErr, reloadErr)

		if reloadErr != nil {
			return reloadErr
		}
		return nil
	})

	http.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		if err := handle.HandleEventRequest(r); err != nil {
			log.Println("webhook error:", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	})

	if err := http.ListenAndServe(":8080", nil); err != nil {
		panic(err)
	}
}

func updateCaddyfile(newHash string) error {
	data, err := os.ReadFile(caddyFilePath)
	if err != nil {
		return err
	}

	content := string(data)

	// Pattern: commit_hash variable in vars block, e.g.
	// commit_hash "d340f16ba1256ec563d7b08c0396645d555e65b8"
	updated := commitHashRe.ReplaceAllString(content, fmt.Sprintf("${1}%s${2}", newHash))

	return os.WriteFile(caddyFilePath, []byte(updated), 0644)
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

	return nil
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
			Value:  formatDiscordStatus(reloadErr, "Reload triggered."),
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

func truncateForDiscord(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit <= 3 {
		return s[:limit]
	}
	return s[:limit-3] + "..."
}

func sendDiscordMessage(ctx context.Context, payload discordPayload) error {
	if discordWebhook == "" {
		return nil
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal discord payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, discordWebhook, bytes.NewReader(body))
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

	updateErr := updateCaddyfile(remoteHash)
	if updateErr != nil {
		log.Println("Startup update failed:", updateErr)
		reportDiscordStartup(ctx, hash, remoteHash, true, nil, updateErr, nil)
		return
	}
	log.Println("Caddyfile updated to remote head.")
	hash = remoteHash

	reloadErr := reloadCaddyInContainer(dockerSock, caddyContainer)
	if reloadErr != nil {
		log.Println("Startup reload failed:", reloadErr)
		reportDiscordStartup(ctx, hash, remoteHash, true, nil, nil, reloadErr)
		return
	}
	log.Println("Caddy reload triggered after startup update.")
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
	if payload.SHA == "" {
		return "", errors.New("github response missing commit sha")
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
