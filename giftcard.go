package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Gift card states. Every transition is also recorded in giftcard_events.
const (
	StateRejected    = "rejected"     // failed format validation, never queued
	StateQueued      = "queued"       // waiting for the redeem worker
	StateRedeeming   = "redeeming"    // worker is currently redeeming it
	StateRedeemed    = "redeemed"     // Amazon confirmed the balance was added
	StateFailed      = "failed"       // Amazon rejected the code (invalid, already used, ...)
	StateNeedsReview = "needs_review" // outcome unknown, check the Amazon balance manually
)

const maxCodeLen = 64

// normalizeCode uppercases the code and strips spaces and dashes.
func normalizeCode(s string) string {
	c := strings.Map(func(r rune) rune {
		if r == '-' || unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToUpper(r)
	}, s)
	if len(c) > maxCodeLen {
		c = c[:maxCodeLen]
	}
	return c
}

// validateCode checks the shape of an amazon.de gift card claim code (XXXX-XXXXXX-XXXX).
// It deliberately does not contact Amazon: the redeem attempt is the real validation.
func validateCode(c string) error {
	if len(c) < 14 || len(c) > 15 {
		return fmt.Errorf("code must be 14 or 15 characters, got %d", len(c))
	}
	for _, r := range c {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return errors.New("code may only contain letters and digits")
		}
	}
	return nil
}

// formatCode renders a normalized code the way Amazon prints it.
func formatCode(c string) string {
	if len(c) == 14 {
		return c[:4] + "-" + c[4:10] + "-" + c[10:]
	}
	return c
}

// maskCode hides all but the last four characters.
func maskCode(c string) string {
	if len(c) <= 4 {
		return strings.Repeat("*", len(c))
	}
	return strings.Repeat("*", len(c)-4) + c[len(c)-4:]
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
