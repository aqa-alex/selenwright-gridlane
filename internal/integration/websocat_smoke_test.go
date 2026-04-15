//go:build websocat

package integration

import (
	"bytes"
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"

	"gridlane/internal/config"
)

func TestWebsocatSmoke(t *testing.T) {
	if _, err := exec.LookPath("websocat"); err != nil {
		t.Skip("websocat is not installed")
	}

	backend := newFakeSelenwright(t, "sw-websocat")
	defer backend.Close()

	router := newRouter(t, testRouterConfig(backendNode{
		ID:        "sw-websocat",
		Endpoint:  backend.URL(),
		Region:    "local",
		Weight:    1,
		Protocols: []config.Protocol{config.ProtocolPlaywright},
	}))
	defer router.Close()

	wsURL := "ws" + strings.TrimPrefix(router.URL, "http") + "/playwright/chrome/stable?smoke=websocat"
	cmd := exec.Command(
		"websocat",
		"-q",
		"-E",
		"-n",
		"--basic-auth", base64.StdEncoding.EncodeToString([]byte("alice:wonderland")),
		"--protocol", "playwright-json",
		wsURL,
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdin = strings.NewReader("hello\n")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("websocat smoke failed: %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	requireRecordedPath(t, backend, "GET /playwright/chrome/stable?smoke=websocat")
}
