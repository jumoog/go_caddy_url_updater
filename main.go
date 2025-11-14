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
	"github.com/google/go-github/v78/github"
)

var (
	caddyFilePath  = getEnv("CADDYFILE_PATH", "/etc/caddy/Caddyfile")
	caddyContainer = getEnv("CADDY_CONTAINER", "caddy") // container NAME, not ID
	dockerSock     = getEnv("DOCKER_SOCK", "/var/run/docker.sock")
)

func getEnv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
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

		if err := updateCaddyfile(newHash); err != nil {
			log.Println("Failed to update Caddyfile:", err)
		} else {
			log.Println("Caddyfile updated successfully.")
		}
		if err := reloadCaddyInContainer(dockerSock, caddyContainer); err != nil {
			return err
		}
		return nil
	})

	// add a http handleFunc
	http.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
		err := handle.HandleEventRequest(r)
		if err != nil {
			fmt.Println("error")
		}
	})

	// start the server listening on port 8080
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

	// Pattern 1: fixed version number, e.g.
	// @<hash>/10.11/manifest.json
	re1 := regexp.MustCompile(`(@)[a-fA-F0-9]{40}(/10\.11/manifest\.json)`)

	// Pattern 2: Caddy placeholder version, e.g.
	// @<hash>/{http.regexp.VER.1}/manifest.json
	re2 := regexp.MustCompile(`(@)[a-fA-F0-9]{40}(/\{http\.regexp\.VER\.1\}/manifest\.json)`)

	// Apply replacements
	updated := re1.ReplaceAllString(content, fmt.Sprintf("@%s$2", newHash))
	updated = re2.ReplaceAllString(updated, fmt.Sprintf("@%s$2", newHash))

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
