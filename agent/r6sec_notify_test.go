package main

import (
	"strings"
	"testing"
)

// r6-security: a failed notification's reason (shown in the activity feed and Settings' notification history,
// i.e. screenshots) must never hold the Discord webhook. The Discord field isn't checked for a scheme, so a
// webhook pasted without "https://" fails with an error that has no "http" in it, and shortErr kept its
// first 60 characters: the webhook's id and the start of its token.
func TestR6ShortErrNeverShowsWebhook(t *testing.T) {
	for _, hook := range []string{
		"discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcdefghij",
		"ftp://discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcdefghij",
		"https://discord.com/api/webhooks/123456789012345678/AbCd EfGh",
	} {
		err := sendDiscord(hook, "x")
		if err == nil {
			continue
		}
		m := shortErr(err)
		if strings.Contains(m, "12345678") || strings.Contains(m, "AbCd") || strings.Contains(m, "webhooks") {
			t.Errorf("webhook %q: the shown reason holds it: %s", hook[:30], m)
		}
	}
}
