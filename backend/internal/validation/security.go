// Package validation provides sensitive data detection, sanitization, and
// input validation utilities for the Terraform State Manager.
package validation

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// SensitivePatterns are regex patterns for detecting sensitive data in text.
var SensitivePatterns = []string{
	`at-[a-zA-Z0-9]{30,}`,                                // Terraform tokens
	`token[\"\s]*[:=][\"\s]*[a-zA-Z0-9+/=]{20,}`,         // Generic tokens
	`password[\"\s]*[:=][\"\s]*\S+`,                      // Passwords
	`secret[\"\s]*[:=][\"\s]*\S+`,                        // Secrets
	`key[\"\s]*[:=][\"\s]*[a-zA-Z0-9+/=]{10,}`,           // Keys
	`bearer\s+[a-zA-Z0-9+/=]{20,}`,                       // Bearer tokens
	`authorization[\"\s]*[:=][\"\s]*[a-zA-Z0-9+/=]{20,}`, // Auth headers
}

// compiledPatterns is the pre-compiled regex cache built from SensitivePatterns.
var compiledPatterns []*regexp.Regexp

func init() {
	for _, p := range SensitivePatterns {
		compiledPatterns = append(compiledPatterns, regexp.MustCompile(p))
	}
}

// testTokenPrefixes lists common dummy/placeholder token values that should
// be rejected during token validation.
var testTokenPrefixes = []string{
	"test", "dummy", "example", "fake", "placeholder",
	"sample", "changeme", "todo", "fixme",
}

// ValidateToken validates a bearer/API token. It ensures the token is between
// 20 and 500 characters, contains no control characters, and does not appear
// to be a test or dummy value.
func ValidateToken(token string) error {
	if len(token) < 20 || len(token) > 500 {
		return fmt.Errorf("token must be between 20 and 500 characters, got %d", len(token))
	}

	for _, r := range token {
		if unicode.IsControl(r) {
			return fmt.Errorf("token contains control characters")
		}
	}

	lower := strings.ToLower(token)
	for _, prefix := range testTokenPrefixes {
		if lower == prefix {
			return fmt.Errorf("token appears to be a test/dummy value")
		}
		if strings.HasPrefix(lower, prefix+"_") || strings.HasPrefix(lower, prefix+"-") || strings.HasPrefix(lower, prefix+".") {
			return fmt.Errorf("token appears to be a test/dummy value")
		}
	}

	return nil
}

// internalCIDRs lists RFC 1918 and loopback address ranges that indicate
// potentially unsafe localhost or internal-network URLs.
var internalCIDRs []*net.IPNet

func init() {
	cidrs := []string{
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
	}
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil {
			internalCIDRs = append(internalCIDRs, network)
		}
	}
}

// ValidateURL validates a URL for security. It rejects URLs that do not use
// http or https schemes, URLs containing control characters, and logs a
// warning (but does not reject) URLs pointing to localhost or internal IPs.
func ValidateURL(urlStr string) error {
	if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
		return fmt.Errorf("URL must start with http:// or https://")
	}

	for _, r := range urlStr {
		if unicode.IsControl(r) {
			return fmt.Errorf("URL contains control characters")
		}
	}

	parsed, err := url.Parse(urlStr)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("URL has no hostname")
	}

	// Check for localhost and internal IPs -- warn but do not reject.
	if host == "localhost" || host == "0.0.0.0" {
		slog.Warn("URL points to localhost, which may be unsafe in production",
			"url", urlStr)
		return nil
	}

	if ip := net.ParseIP(host); ip != nil {
		for _, cidr := range internalCIDRs {
			if cidr.Contains(ip) {
				slog.Warn("URL points to an internal/private IP address",
					"url", urlStr, "ip", host)
				return nil
			}
		}
	}

	return nil
}

// MaskSensitiveData replaces sensitive patterns in text with a masked version.
// For matches longer than 6 characters it shows the first 3 and last 3
// characters: "abc***MASKED***xyz". Shorter matches are fully masked.
func MaskSensitiveData(text string) string {
	result := text
	for _, re := range compiledPatterns {
		result = re.ReplaceAllStringFunc(result, maskMatch)
	}
	return result
}

// maskMatch is the replacement function used by MaskSensitiveData.
func maskMatch(s string) string {
	if len(s) <= 6 {
		return "***MASKED***"
	}
	return s[:3] + "***MASKED***" + s[len(s)-3:]
}

// ContainsSensitiveData checks whether text matches any of the SensitivePatterns.
func ContainsSensitiveData(text string) bool {
	for _, re := range compiledPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// SensitiveConfigKeys lists config map key substrings that indicate a value
// should be encrypted at rest.
var SensitiveConfigKeys = []string{
	"token", "key", "secret", "password", "credential", "private",
}

// IsSensitiveConfigKey returns true if the key name suggests it holds a
// sensitive value (e.g. "token", "secret_key", "password").
func IsSensitiveConfigKey(key string) bool {
	lower := strings.ToLower(key)
	for _, k := range SensitiveConfigKeys {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}
