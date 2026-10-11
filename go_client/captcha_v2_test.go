package main

import (
	"strings"
	"testing"
)

func TestParseCaptchaV2Page_IIFE(t *testing.T) {
	html := `<!DOCTYPE html>
<html>
<head>
<script>
window.vk = {
	"apiConfigDomains": ["vk.com"],
	"statsMeta": {"some": "value"},
	"lang": 3,
	"brlefapmjnpg": "12345678-abcd-ef01-2345-6789abcdef01"
};
</script>
<script src="https://id.vk.com/vkid/1.1.1324/not_robot_captcha.js"></script>
</head>
<body>
<script>
window.init = {"data":{"show_captcha_type":"checkbox","captcha_settings":[]}};
}( "ABCDefgh12345678" , 4 , "error_label" , ["probe1", "probe2"] ));
var tel_hash = "fake";
</script>
</body>
</html>`

	page, err := parseCaptchaV2Page(html)
	if err != nil {
		t.Fatalf("parseCaptchaV2Page failed: %v", err)
	}

	if page.PowInput != "ABCDefgh12345678" {
		t.Errorf("got PowInput %q, want ABCDefgh12345678", page.PowInput)
	}
	if page.PowDifficulty != 4 {
		t.Errorf("got PowDifficulty %d, want 4", page.PowDifficulty)
	}
	if page.Lang != "3" {
		t.Errorf("got Lang %q, want 3", page.Lang)
	}
	if page.DebugInfo != "12345678-abcd-ef01-2345-6789abcdef01" {
		t.Errorf("got DebugInfo %q, want 12345678-abcd-ef01-2345-6789abcdef01", page.DebugInfo)
	}
	if !page.HasTelemetry {
		t.Errorf("expected HasTelemetry to be true")
	}

	result := buildPowResult(page.HasTelemetry, "0000abcd1234", 12345, 42)
	if !strings.HasPrefix(result, "v2.") {
		t.Errorf("expected v2. prefix, got %q", result)
	}
}

func TestParseCaptchaV2Page_Legacy(t *testing.T) {
	html := `<!DOCTYPE html>
<html>
<head>
<script src="https://id.vk.com/vkid/1.1.1324/not_robot_captcha.js"></script>
</head>
<body>
<script>
window.init = {"data":{"show_captcha_type":"slider","captcha_settings":[{"type":"slider","settings":"test_slider_key"}]}};
const powInput = "LEGACY_POW_INPUT";
const difficulty = 3;
</script>
</body>
</html>`

	page, err := parseCaptchaV2Page(html)
	if err != nil {
		t.Fatalf("parseCaptchaV2Page failed: %v", err)
	}

	if page.PowInput != "LEGACY_POW_INPUT" {
		t.Errorf("got PowInput %q, want LEGACY_POW_INPUT", page.PowInput)
	}
	if page.PowDifficulty != 3 {
		t.Errorf("got PowDifficulty %d, want 3", page.PowDifficulty)
	}
	if page.HasTelemetry {
		t.Errorf("expected HasTelemetry to be false")
	}
}
