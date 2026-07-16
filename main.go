package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/tomatome/grdp/glog"
)

func main() {
	if len(os.Args) == 1 {
		usage()
		os.Exit(2)
	}

	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		usage()
		os.Exit(2)
	}

	// Configure upstream library logging. glog panics ("logger not inited")
	// if SetLogger isn't called before any glog call, so initialise it first.
	// By default silence the library entirely (it logs some harmless warnings
	// like "unsupported Capability type 0x001e"); real session errors are
	// surfaced through our own connect/capture error paths.
	log.SetFlags(log.Ltime)
	glog.SetLogger(log.New(os.Stderr, "", log.Ltime))
	switch {
	case cfg.Verbose:
		glog.SetLevel(glog.DEBUG)
	case cfg.Debug:
		glog.SetLevel(glog.INFO)
	default:
		glog.SetLevel(glog.NONE)
	}

	// 1. Connect and log in.
	fmt.Fprintf(os.Stderr, "connecting to %s as %s ...\n", cfg.Server, cfg.User)
	s, err := newSession(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect failed:", err)
		os.Exit(1)
	}
	defer s.Close()
	fmt.Fprintln(os.Stderr, "connected; session ready")

	// Small grace period for the desktop to settle after login.
	time.Sleep(cfg.StepDelay)
	if cfg.Debug {
		saveShot(s, "shot_01_desktop.png")
	}

	// 2. Open the Run dialog (Win+R) and launch the shell.
	if cfg.Debug {
		fmt.Fprintf(os.Stderr, "launching %s via Win+R ...\n", cfg.Shell)
	}
	s.sendWinR(cfg.KeyDelay)
	time.Sleep(cfg.StepDelay)
	if cfg.Debug {
		saveShot(s, "shot_02_after_winr.png")
	}

	launcher := cfg.launcher()
	s.typeString(launcher, cfg.KeyDelay)
	time.Sleep(cfg.KeyDelay)
	s.sendEnter(cfg.KeyDelay)
	// Give the shell window time to appear.
	time.Sleep(cfg.StepDelay + cfg.StepDelay)
	if cfg.Debug {
		saveShot(s, "shot_03_after_shell_launch.png")
	}

	// 3. Type the command (wrapped to copy output to clipboard if capturing).
	cmd := normalizeCommand(cfg.wrapCommand())
	if cfg.Debug {
		fmt.Fprintf(os.Stderr, "typing command: %s\n", cmd)
	}
	s.typeString(cmd, cfg.KeyDelay)
	time.Sleep(cfg.KeyDelay)

	// Measure the UAC brightness baseline BEFORE pressing Enter, while the
	// desktop is still normal. A UAC prompt can appear almost instantly
	// after Enter, so measuring after Enter would capture an already-dimmed
	// frame as the "baseline" and miss the dim.
	uacBaseline, _ := s.bmp.meanBrightness()
	if cfg.Debug && cfg.UAC {
		fmt.Fprintf(os.Stderr, "uac baseline (pre-Enter) brightness=%.0f\n", uacBaseline)
	}

	s.sendEnter(cfg.KeyDelay)
	if cfg.Debug {
		saveShot(s, "shot_04_after_command_enter.png")
	}

	// 4. UAC handling: watch the screen for dimming (secure desktop) and send
	// Alt+Y to confirm, up to the UAC timeout. Skip entirely if --uac-timeout=0.
	if cfg.UAC && cfg.UACTimeout > 0 {
		if cfg.Debug {
			fmt.Fprintf(os.Stderr, "watching for UAC prompt (baseline brightness=%.0f, timeout=%s)...\n", uacBaseline, cfg.UACTimeout)
		}
		confirmed := s.bmp.watchUAC(cfg.UACTimeout, uacBaseline, 0.8, func() {
			fmt.Fprintln(os.Stderr, "UAC prompt detected; sending Alt+Y")
			if cfg.Debug {
				saveShot(s, "shot_uac_detected.png")
			}
			s.sendAltY(cfg.KeyDelay)
		})
		if confirmed {
			// Give the elevated process a moment to start.
			time.Sleep(cfg.StepDelay)
			if cfg.Debug {
				saveShot(s, "shot_06_after_uac_confirm.png")
			}
		} else if cfg.Debug {
			fmt.Fprintln(os.Stderr, "no UAC prompt detected; continuing")
		}
	}

	// 5. Capture output if requested; otherwise disconnect immediately.
	if cfg.Capture {
		// Give the command time to execute and update the clipboard.
		time.Sleep(2 * time.Second)
		if cfg.Debug {
			saveShot(s, "shot_05_before_capture.png")
		}
		if cfg.Debug {
			fmt.Fprintf(os.Stderr, "capturing output via clipboard (timeout=%s)...\n", cfg.Timeout)
		}
		out, err := s.captureOutput(cfg.Timeout)
		if err != nil {
			fmt.Fprintln(os.Stderr, "capture failed:", err)
			os.Exit(1)
		}
		// Print the captured command output to stdout (so it can be piped).
		fmt.Print(out)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			fmt.Println()
		}
	} else {
		if cfg.Debug {
			fmt.Fprintln(os.Stderr, "command launched; disconnecting (no capture)")
		}
	}

	// 6. Done. defer Close() handles teardown.
	fmt.Fprintln(os.Stderr, "done")
}

// saveShot writes the current RDP frame buffer to a PNG and logs its mean
// brightness, so we can correlate screenshots with screen state.
func saveShot(s *rdpSession, name string) {
	br, ok := s.bmp.meanBrightness()
	if err := s.bmp.savePNG(name); err != nil {
		fmt.Fprintf(os.Stderr, "  [shot %s] save error: %v\n", name, err)
		return
	}
	if ok {
		fmt.Fprintf(os.Stderr, "  [shot %s] saved (mean brightness=%.0f)\n", name, br)
	} else {
		fmt.Fprintf(os.Stderr, "  [shot %s] saved (no frame data yet)\n", name)
	}
}
