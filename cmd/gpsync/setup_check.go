package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gmizrahi/gpsync/internal/auth"
	"github.com/gmizrahi/gpsync/internal/config"
)

// A check is one thing that has to be true before an upload can work, and
// what to do when it is not. Every check reports even when it passes, so a
// check that quietly stops running cannot be mistaken for one that passed.
type check struct {
	name    string
	ok      bool
	skipped bool // did not run; neither a pass nor a failure
	detail  string
	remedy  string // only shown when ok is false
}

const (
	consentScreenURL = "https://console.cloud.google.com/apis/credentials/consent"
	oauthClientsURL  = "https://console.cloud.google.com/apis/credentials"
	enableAPIURL     = "https://console.cloud.google.com/apis/library/photoslibrary.googleapis.com"

	// One cheap authenticated request, used only to tell the failure modes apart.
	photosProbeURL = "https://photoslibrary.googleapis.com/v1/mediaItems?pageSize=1"
)

// runSetupCheck reports whether this installation can actually upload,
// naming the first thing that would stop it rather than letting a run fail
// later with a less obvious error.
func runSetupCheck() error {
	checks := []check{checkConfig(), checkClientSecret(), checkToken()}

	// Only probe the API when a token already exists. auth.GetHTTPClient
	// starts the interactive browser consent flow when it finds none, and a
	// command called "check" must never do that as a side effect.
	if checks[len(checks)-1].ok {
		checks = append(checks, checkAPI())
	} else {
		checks = append(checks, check{
			name:    "Google Photos API",
			detail:  "not checked -- sign in first",
			skipped: true,
		})
	}

	failed := 0
	for _, c := range checks {
		mark, colour := "ok", colOK
		switch {
		case c.skipped:
			mark, colour = "--", colDim
		case !c.ok:
			mark, colour = "FAIL", colErr
			failed++
		}
		fmt.Printf("%-24s %s  %s\n", c.name, colour(fmt.Sprintf("%-4s", mark)), c.detail)
		if !c.ok && !c.skipped && c.remedy != "" {
			for _, line := range strings.Split(c.remedy, "\n") {
				fmt.Printf("%-24s      %s\n", "", colDim(line))
			}
		}
	}

	fmt.Println()
	if failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(checks))
	}
	fmt.Println("Ready to sync. Try: gpsync sync")
	return nil
}

func checkConfig() check {
	cfg, err := config.Load()
	if err != nil {
		return check{name: "Configuration", detail: err.Error(),
			remedy: "Run `gpsync setup` to create one."}
	}
	if len(cfg.SourceFolders) == 0 {
		return check{name: "Configuration", detail: "no source folders configured",
			remedy: "Run `gpsync setup`, or pass folders directly: gpsync sync \"C:\\Photos\\2026\""}
	}
	return check{name: "Configuration", ok: true,
		detail: fmt.Sprintf("%s (%d source folder(s))", config.ConfigPath, len(cfg.SourceFolders))}
}

func checkClientSecret() check {
	if !auth.HasCredentials() {
		return check{name: "OAuth client", detail: "no client_secret.json",
			remedy: "Run `gpsync setup` to create Google credentials, or\n" +
				"`gpsync import-rclone` to reuse an existing rclone remote."}
	}
	id, ok := auth.CurrentClientID()
	if !ok {
		return check{name: "OAuth client", detail: "client_secret.json is unreadable",
			remedy: "Run `gpsync setup` again to replace it."}
	}
	return check{name: "OAuth client", ok: true, detail: id}
}

func checkToken() check {
	if !auth.HasToken() {
		return check{name: "Sign-in token", detail: "not signed in yet",
			remedy: "Run `gpsync setup`; it opens a browser to authorise gpsync."}
	}
	return check{name: "Sign-in token", ok: true, detail: auth.TokenPath}
}

// checkAPI makes one real request, which is the only way to tell a project
// with the Photos Library API switched off from one that is simply waiting
// for its first upload.
func checkAPI() check {
	client, err := auth.GetHTTPClient(context.Background())
	if err != nil {
		return check{name: "Google Photos API", detail: "could not use the stored token: " + err.Error(),
			remedy: "Run `gpsync setup` to sign in again."}
	}

	resp, err := client.Get(photosProbeURL)
	if err != nil {
		return check{name: "Google Photos API", detail: "could not reach Google: " + err.Error(),
			remedy: "Check your network connection and try again."}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return classifyAPIProbe(resp.StatusCode, body)
}

// classifyAPIProbe turns one probe response into a verdict. Split out from
// checkAPI so the interesting part -- telling a disabled API from the 403
// that an append-only credential is SUPPOSED to get -- is testable without a
// network or a signed-in account.
func classifyAPIProbe(status int, body []byte) check {
	var parsed struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)

	switch {
	case status == http.StatusOK:
		// A token imported from rclone can carry read scope; gpsync itself
		// never asks for it.
		return check{name: "Google Photos API", ok: true, detail: "reachable, token accepted"}

	case parsed.Error.Status == "SERVICE_DISABLED",
		strings.Contains(parsed.Error.Message, "SERVICE_DISABLED"),
		strings.Contains(parsed.Error.Message, "not been used in project"):
		return check{name: "Google Photos API", detail: "the API is not enabled for this project",
			remedy: "Enable it, then try again:\n  " + enableAPIURL}

	case status == http.StatusUnauthorized:
		return check{name: "Google Photos API", detail: "Google rejected the stored token",
			remedy: "Run `gpsync setup` to sign in again."}

	case status == http.StatusForbidden:
		// The expected answer. gpsync requests an append-only scope, so
		// reading media items back is refused -- which still proves the
		// token was accepted and the API is enabled.
		return check{name: "Google Photos API", ok: true,
			detail: "reachable; read access refused, which is expected (gpsync uploads only)"}

	default:
		return check{name: "Google Photos API",
			detail: fmt.Sprintf("unexpected response: HTTP %d %s", status, parsed.Error.Message),
			remedy: "Run `gpsync setup --check` again; if it persists, see docs/troubleshooting.md"}
	}
}

// printGcloudScript emits the part of Google Cloud setup that can be
// scripted. The rest genuinely cannot: Google exposes no API for creating an
// OAuth consent screen or a Desktop client, so any tool claiming to automate
// the whole thing is wrong. Being explicit about the boundary is more useful
// than pretending it is not there.
func printGcloudScript() {
	script := `# gpsync — Google Cloud setup
#
# Steps 1-3 can be scripted, and are below. Steps 4-6 cannot: Google
# provides no API for the OAuth consent screen or for creating a Desktop
# client, so those are done once in the browser.
#
# Requires the gcloud CLI: https://cloud.google.com/sdk/docs/install

# 1. A project of your own. The ID must be globally unique.
PROJECT_ID="gpsync-$(date +%Y%m%d)-$RANDOM"
gcloud projects create "$PROJECT_ID" --name="gpsync"

# 2. Use it for the commands that follow.
gcloud config set project "$PROJECT_ID"

# 3. Switch on the Photos Library API.
gcloud services enable photoslibrary.googleapis.com

echo
echo "Now finish in the browser, for this project ($PROJECT_ID):"
echo
echo "  4. Configure the OAuth consent screen (type: External)."
echo "     Add your own Google account under 'Test users'."
echo "     ` + consentScreenURL + `"
echo
echo "  5. Create credentials -> OAuth client ID -> Desktop app."
echo "     ` + oauthClientsURL + `"
echo
echo "  6. Download the client secret JSON, then run:"
echo "       gpsync setup"
echo "     and give it the path to that file."
`
	fmt.Fprint(os.Stdout, script)
}
