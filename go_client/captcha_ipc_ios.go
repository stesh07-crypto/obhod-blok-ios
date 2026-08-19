//go:build ios

package main

import (
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

func requestIOSWebViewCaptcha(streamID int, captchaErr *VkCaptchaError, mode string, timeout time.Duration) (string, error) {
	dir := profileStorageDirectory()
	if dir == "" {
		return "", fmt.Errorf("iOS captcha bridge storage is unavailable")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("iOS captcha bridge mkdir: %w", err)
	}

	requestPath := filepath.Join(dir, iosCaptchaRequestFile)
	resultPath := filepath.Join(dir, iosCaptchaResultFile)
	_ = os.Remove(resultPath)

	requestID := fmt.Sprintf("%d-%d", time.Now().UnixNano(), streamID)
	req := iosCaptchaRequest{
		ID:           requestID,
		Mode:         mode,
		RedirectURI:  captchaErr.RedirectURI,
		SessionToken: captchaErr.SessionToken,
		CreatedAtMS:  time.Now().UnixMilli(),
	}
	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	if err := writeAtomicFile(requestPath, data, 0600); err != nil {
		return "", fmt.Errorf("iOS captcha request write: %w", err)
	}
	defer func() { _ = os.Remove(requestPath) }()

	log.Printf("[STREAM %d] [КАПЧА] iOS WBV %s: запрос передан приложению", streamID, mode)

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, readErr := os.ReadFile(resultPath)
		if readErr == nil {
			var result iosCaptchaResult
			if json.Unmarshal(data, &result) == nil && result.ID == requestID {
				_ = os.Remove(resultPath)
				value := strings.TrimSpace(result.Result)
				if value == "" {
					return "", fmt.Errorf("webview captcha returned empty result")
				}
				lower := strings.ToLower(value)
				if strings.HasPrefix(lower, "error:") {
					return "", fmt.Errorf("webview captcha failed: %s", value)
				}
				log.Printf("[STREAM %d] [КАПЧА] iOS WBV %s: success_token получен", streamID, mode)
				return value, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("webview captcha timed out")
}

func writeAtomicFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
