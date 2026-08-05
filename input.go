package main

import (
	"strings"
	"time"
	"unicode"
)

// PS/2 set-1 scancodes for the special/hardware keys we use. Text characters
// are always sent as Unicode events (see typeRune), which deliver the literal
// code point and are inserted by Windows regardless of the active keyboard
// layout. Scancodes are only used for keys that have no Unicode form:
// modifier keys (Win, Alt) and control keys (Enter, Esc, Backspace).
const (
	scRWin      = 0x5B // Left Win key (extended)
	scD         = 0x20 // 'D' (used with Win to show the desktop)
	scR         = 0x13 // 'R' (used with Win for the Win+R shell hotkey)
	scReturn    = 0x1C // Enter
	scLAlt      = 0x38 // Left Alt
	scY         = 0x15 // 'Y' (used with Alt for the UAC Yes hotkey)
	scEscape    = 0x01 // Esc
	scBackspace = 0x0E // Backspace
)

// sendWinR presses and releases Win+R to open the Run dialog.
func (s *rdpSession) sendWinR(keyDelay time.Duration) {
	s.keyDown(scRWin, true)
	time.Sleep(keyDelay)
	s.keyPress(scR, false, keyDelay)
	s.keyUp(scRWin, true)
	time.Sleep(keyDelay)
}

// sendWinD presses and releases Win+D to show the desktop. Unlike Win+M, it
// works when another application refuses to minimize. Its result is only a
// best-effort preflight; callers must still verify the following UI state.
func (s *rdpSession) sendWinD(keyDelay time.Duration) {
	s.keyDown(scRWin, true)
	time.Sleep(keyDelay)
	s.keyPress(scD, false, keyDelay)
	s.keyUp(scRWin, true)
	time.Sleep(keyDelay)
}

// sendEnter presses and releases the Return key.
func (s *rdpSession) sendEnter(keyDelay time.Duration) {
	s.keyPress(scReturn, false, keyDelay)
}

// sendAltY presses Alt+Y (used to confirm a Yes/No UAC prompt).
func (s *rdpSession) sendAltY(keyDelay time.Duration) {
	s.keyDown(scLAlt, false)
	time.Sleep(keyDelay)
	s.keyPress(scY, false, keyDelay)
	s.keyUp(scLAlt, false)
	time.Sleep(keyDelay)
}

// sendEscape presses and releases Esc (to dismiss dialogs).
func (s *rdpSession) sendEscape(keyDelay time.Duration) {
	s.keyPress(scEscape, false, keyDelay)
}

// sendBackspace presses and releases Backspace.
func (s *rdpSession) sendBackspace(keyDelay time.Duration) {
	s.keyPress(scBackspace, false, keyDelay)
}

// typeString types a string character by character using Unicode input
// events. Unicode events deliver the literal code point, so the text is
// inserted correctly regardless of the remote machine's keyboard layout.
func (s *rdpSession) typeString(str string, keyDelay time.Duration) {
	for _, r := range str {
		s.typeRune(r, keyDelay)
	}
}

// typeRune types a single rune as a Unicode key event. This works on any
// keyboard layout; no Shift handling is needed because the server inserts the
// exact character rather than translating a physical key position.
func (s *rdpSession) typeRune(r rune, keyDelay time.Duration) {
	s.unicodeDown(r)
	time.Sleep(keyDelay)
	s.unicodeUp(r)
	time.Sleep(keyDelay)
}

// normalizeCommand strips a trailing newline / CR so we don't send an extra
// Enter inside the typed string (we send Enter separately).
func normalizeCommand(cmd string) string {
	cmd = strings.TrimRight(cmd, "\r\n")
	return cmd
}

// isPrintable is a sanity check used in verbose logging.
func isPrintable(r rune) bool {
	return unicode.IsPrint(r)
}
