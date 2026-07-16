package main

import (
	"fmt"
	"strings"
	"time"
)

// captureOutput pulls the remote clipboard text back via the CLIPRDR channel
// (CF_UNICODETEXT). We poll the clipboard directly with CB_FORMAT_DATA_REQUEST
// rather than waiting for a server-initiated CB_FORMAT_LIST, because some
// servers don't announce clipboard changes back over RDP unless the local
// clipboard viewer is registered.
//
// Flow:
//  1. The command (already wrapped to copy to clipboard) was typed + Enter.
//  2. Give the command a moment to run and update the clipboard.
//  3. Repeatedly send CB_FORMAT_DATA_REQUEST for CF_UNICODETEXT until we get
//     non-empty text or the overall timeout elapses.
func (s *rdpSession) captureOutput(timeout time.Duration) (string, error) {
	// Drain any stale clipboard notifications so they don't confuse later reads.
	select {
	case <-s.clip.OnFormatList:
	default:
	}
	select {
	case <-s.clip.OnText:
	default:
	}

	deadline := time.Now().Add(timeout)
	attempt := 0
	for {
		attempt++
		// Request the unicode text from the remote clipboard.
		s.clip.RequestText()

		// Wait a short while for the response.
		wait := 3 * time.Second
		if remain := time.Until(deadline); remain < wait {
			wait = remain
		}
		if wait <= 0 {
			return "", fmt.Errorf("timeout waiting for clipboard data (after %d attempts)", attempt)
		}
		select {
		case text := <-s.clip.OnText:
			text = strings.TrimRight(text, "\x00")
			if text != "" {
				return text, nil
			}
			// Empty response: the command may not have finished yet. Retry.
		case <-time.After(wait):
			// No response for this attempt; try again until the deadline.
		case err := <-s.errCh:
			return "", fmt.Errorf("session error during capture: %w", err)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timeout waiting for clipboard data (after %d attempts)", attempt)
		}
		// Small pause between attempts so we don't hammer the server.
		time.Sleep(1 * time.Second)
	}
}
