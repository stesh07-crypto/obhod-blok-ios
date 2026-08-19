//go:build ios

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	iosCaptchaRequestFile = "wdtt_captcha_request.json"
	iosCaptchaResultFile  = "wdtt_captcha_result.json"
)

type iosCaptchaRequest struct {
	ID           string `json:"id"`
	Mode         string `json:"mode"`
	RedirectURI  string `json:"redirect_uri"`
	SessionToken string `json:"session_token"`
	CreatedAtMS  int64  `json:"created_at_ms"`
}

type iosCaptchaResult struct {
	ID     string `json:"id"`
	Result string `json:"result"`
}

func init() {
	reader, writer, err := os.Pipe()
	if err != nil {
		return
	}
	// Android receives CAPTCHA_SOLVE through the child process stdout. On iOS
	// Go is embedded in the NetworkExtension, so intercept that same protocol
	// locally and relay it through the shared App Group container.
	os.Stdout = writer
	go consumeIOSStdout(reader)
}

func consumeIOSStdout(reader *os.File) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "CAPTCHA_SOLVE|") {
			handleIOSCaptchaSolveLine(line)
			continue
		}
		// Preserve non-captcha diagnostic stdout without feeding it back into the
		// intercepted pipe. log.Printf is already routed to the Swift log bridge.
		if line != "" {
			log.Printf("[GO] %s", line)
		}
	}
}

func handleIOSCaptchaSolveLine(line string) {
	parts := strings.SplitN(line, "|", 4)
	if len(parts) != 4 {
		select {
		case CaptchaResultChan <- "error:invalid CAPTCHA_SOLVE format":
		default:
		}
		return
	}

	mode := strings.ToLower(strings.TrimSpace(parts[1]))
	redirectURI := strings.TrimSpace(parts[2])
	sessionToken := strings.TrimSpace(parts[3])
	if redirectURI == "" || sessionToken == "" {
		select {
		case CaptchaResultChan <- "error:invalid captcha request":
		default:
		}
		return
	}

	dir := profileStorageDirectory()
	if dir == "" {
		select {
		case CaptchaResultChan <- "error:iOS App Group storage unavailable":
		default:
		}
		return
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		select {
		case CaptchaResultChan <- "error:iOS App Group storage unavailable":
		default:
		}
		return
	}

	requestPath := filepath.Join(dir, iosCaptchaRequestFile)
	resultPath := filepath.Join(dir, iosCaptchaResultFile)
	_ = os.Remove(resultPath)

	requestID := fmt.Sprintf("%d", time.Now().UnixNano())
	req := iosCaptchaRequest{
		ID:           requestID,
		Mode:         mode,
		RedirectURI:  redirectURI,
		SessionToken: sessionToken,
		CreatedAtMS:  time.Now().UnixMilli(),
	}
	data, err := json.Marshal(req)
	if err != nil || writeAtomicFile(requestPath, data, 0600) != nil {
		select {
		case CaptchaResultChan <- "error:iOS captcha request write failed":
		default:
		}
		return
	}
	defer func() { _ = os.Remove(requestPath) }()

	log.Printf("[КАПЧА] iOS WBV %s: запрос передан основному приложению", mode)

	wait := 10 * time.Second
	switch mode {
	case "manual":
		wait = 60 * time.Second
	case "selected":
		wait = 120 * time.Second
	}
	deadline := time.Now().Add(wait - 250*time.Millisecond)

	for time.Now().Before(deadline) {
		resultData, readErr := os.ReadFile(resultPath)
		if readErr == nil {
			var result iosCaptchaResult
			if json.Unmarshal(resultData, &result) == nil && result.ID == requestID {
				_ = os.Remove(resultPath)
				value := strings.TrimSpace(result.Result)
				if value == "" {
					value = "error:empty captcha result"
				}
				log.Printf("[КАПЧА] iOS WBV %s: ответ получен от приложения", mode)
				CaptchaResultChan <- value
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	select {
	case CaptchaResultChan <- "error:timeout":
	default:
	}
}

func writeAtomicFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
