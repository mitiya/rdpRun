package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config holds parsed command-line options for rdpRun.
type Config struct {
	Server     string // host:port
	User       string // login (DOMAIN\user or user)
	Password   string
	Command    string // shell command to execute on the remote machine
	Shell      string // "cmd" or "powershell"
	Capture    bool   // capture command output via clipboard
	RawCmd     bool   // do not auto-wrap command in "| clip" / "| Set-Clipboard"
	Timeout    time.Duration
	Auth       string // "nla" | "standard" | "auto"
	Width      int
	Height     int
	KeyDelay   time.Duration // delay between keypresses
	StepDelay  time.Duration // delay between macro steps (Win+R, type, enter...)
	UAC        bool          // auto-confirm UAC via Alt+Y
	UACTimeout time.Duration
	Verbose    bool
	Debug      bool // save diagnostic screenshots + extra state output
}

func parseArgs(args []string) (*Config, error) {
	// Go's flag package stops parsing flags at the first non-flag argument, so
	// `rdprun host user pass cmd --capture` would leave --capture unparsed.
	// Reorder so all flag tokens come first and positional args come after;
	// this lets users put flags anywhere on the command line.
	args = reorderFlags(args)

	fs := flag.NewFlagSet("rdpRun", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	cfg := &Config{}
	fs.StringVar(&cfg.Server, "server", "", "RDP server as host:port (required)")
	fs.StringVar(&cfg.User, "user", "", "login (DOMAIN\\user or user) (required)")
	fs.StringVar(&cfg.Password, "pass", "", "password (required)")
	fs.StringVar(&cfg.Command, "cmd", "", "command to execute on the remote machine (required)")
	fs.StringVar(&cfg.Shell, "shell", "cmd", "shell to launch via Win+R: cmd | powershell")
	fs.BoolVar(&cfg.Capture, "capture", false, "capture command output via RDP clipboard and print it locally")
	fs.BoolVar(&cfg.RawCmd, "raw-cmd", false, "do not auto-wrap the command to copy its output to clipboard (use with --capture)")
	fs.DurationVar(&cfg.Timeout, "timeout", 30*time.Second, "timeout for capture / session wait")
	fs.StringVar(&cfg.Auth, "auth", "auto", "auth mode: nla | standard | auto")
	fs.IntVar(&cfg.Width, "width", 1024, "desktop width")
	fs.IntVar(&cfg.Height, "height", 768, "desktop height")
	fs.DurationVar(&cfg.KeyDelay, "key-delay", 40*time.Millisecond, "delay between individual keypresses")
	fs.DurationVar(&cfg.StepDelay, "step-delay", 700*time.Millisecond, "delay between macro steps")
	fs.BoolVar(&cfg.UAC, "uac", true, "auto-confirm UAC/elevation prompts via Alt+Y")
	fs.DurationVar(&cfg.UACTimeout, "uac-timeout", 5*time.Second, "how long to watch for a UAC prompt after a command (set 0 to skip)")
	fs.BoolVar(&cfg.Verbose, "verbose", false, "enable verbose RDP library logging")
	fs.BoolVar(&cfg.Debug, "debug", false, "save diagnostic screenshots (shot_NN_*.png) and print extra state")

	// Also accept positional args: server user pass "command".
	// Flags must come BEFORE the positional args (Go's flag parser stops at
	// the first non-flag argument), so the command is taken as the single
	// 4th positional arg (quote it if it contains spaces).
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	rest := fs.Args()
	if len(rest) >= 1 && cfg.Server == "" {
		cfg.Server = rest[0]
	}
	if len(rest) >= 2 && cfg.User == "" {
		cfg.User = rest[1]
	}
	if len(rest) >= 3 && cfg.Password == "" {
		cfg.Password = rest[2]
	}
	if len(rest) >= 4 && cfg.Command == "" {
		// Join only the trailing positional args into the command so that a
		// multi-word command given unquoted still works. NOTE: because the
		// flag parser stops at the first non-flag, any flags appearing AFTER
		// the positional command will end up here too — so we warn if we see
		// something that looks like a flag.
		cfg.Command = strings.Join(rest[3:], " ")
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Server == "" {
		return fmt.Errorf("--server (host:port) is required")
	}
	if c.User == "" {
		return fmt.Errorf("--user is required")
	}
	if c.Command == "" {
		return fmt.Errorf("--cmd is required")
	}
	switch c.Shell {
	case "cmd", "powershell":
	default:
		return fmt.Errorf("--shell must be cmd or powershell, got %q", c.Shell)
	}
	switch c.Auth {
	case "nla", "standard", "auto":
	default:
		return fmt.Errorf("--auth must be nla, standard or auto, got %q", c.Auth)
	}
	return nil
}

// launcher returns the string to type into the Win+R "Run" dialog to open the
// chosen shell.
func (c *Config) launcher() string {
	switch c.Shell {
	case "powershell":
		return "powershell"
	default:
		return "cmd"
	}
}

// wrapCommand wraps the user command so that its output is copied to the
// clipboard (for capture mode), unless --raw-cmd is set.
func (c *Config) wrapCommand() string {
	if c.RawCmd || !c.Capture {
		return c.Command
	}
	switch c.Shell {
	case "powershell":
		// Out-String gives the full textual output as one string for Set-Clipboard.
		return c.Command + " | Out-String | Set-Clipboard"
	default:
		return c.Command + " | clip"
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `rdpRun — run a command on a remote Windows machine over RDP

Usage:
  rdprun --server host:port --user USER --pass PASS --cmd "command" [options]
  rdprun  host:port  USER  PASS  "command"  [options]

Examples:
  rdprun 192.168.1.10:3389 admin secretpass "whoami" --capture
  rdprun --server 10.0.0.5:3389 --user LAB\joe --pass pw --cmd "ipconfig /all" --shell cmd --capture
  rdprun --server srv:3389 --user joe --pass pw --cmd "Get-Process" --shell powershell --capture

Options:
`)
}

// reorderFlags moves all flag tokens (anything starting with "-") to the
// front of the slice and all positional args to the back, preserving relative
// order within each group. This makes the Go flag parser see all flags even
// when they appear after positional arguments.
//
// It treats a single "-" as positional (often stdin) and does not reorder the
// special "--" terminator (everything after "--" stays positional in order).
func reorderFlags(args []string) []string {
	var flags, positional []string
	afterDoubleDash := false
	for _, a := range args {
		if afterDoubleDash {
			positional = append(positional, a)
			continue
		}
		if a == "--" {
			afterDoubleDash = true
			continue
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
		} else {
			positional = append(positional, a)
		}
	}
	return append(flags, positional...)
}
