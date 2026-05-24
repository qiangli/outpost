// `outpost run --label X -- <cmd>` is the supported alternative to
// `launchctl submit`, which silently no-ops inside the matrix-shell
// because the SSH session inherits a launchd system-domain context
// that doesn't have `submit` capability (see
// docs/matrix-shell-deferred-bugs.md #8).
//
// What this verb does: generate a LaunchAgent plist for the operator's
// command, bootstrap it into the per-user `gui/<uid>` domain via
// `launchctl bootstrap`, and persist the plist under
// ~/Library/LaunchAgents so it auto-loads at next login too. Pair with
// `outpost run --list` (show outpost-managed jobs) and
// `outpost run --remove <label>` (bootout + delete plist).
//
// macOS only — `launchctl bootstrap` doesn't exist on Linux/Windows.
// The verb is registered regardless so the help text shows the recipe
// on every platform, but RunE errors out early on non-darwin so
// operator scripts get a clear message instead of a cryptic exec
// failure.
package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"

	"github.com/spf13/cobra"
)

// labelPrefix scopes the plist Label so `outpost run --list` can tell
// outpost-managed jobs apart from anything else the operator (or
// other tooling) loaded into gui/<uid>. Mirrors the brewservices
// `homebrew.mxcl.` prefix convention.
const labelPrefix = "outpost.run."

func runCmd() *cobra.Command {
	var (
		labelFlag        string
		keepAliveFlag    bool
		stdoutFlag       string
		stderrFlag       string
		workdirFlag      string
		listFlag         bool
		removeFlag       string
		throttleSecsFlag int
	)
	cmd := &cobra.Command{
		Use:   "run [flags] -- <command> [args...]",
		Short: "Submit a command as a per-user launchd agent (macOS replacement for `launchctl submit`)",
		Long: `outpost run is the supported alternative to "launchctl submit" inside the
matrix-shell. It generates a LaunchAgent plist for your command,
bootstraps it into the per-user "gui/<uid>" domain, and persists it
under ~/Library/LaunchAgents so it auto-loads at next login.

Examples
  # one-shot daemon
  outpost run --label kg3-pipeline -- /Users/me/bin/kg3 serve

  # keep it alive (relaunch on crash)
  outpost run --label kg3-pipeline --keep-alive --stdout /tmp/kg3.out -- \
      /Users/me/bin/kg3 serve

  # list outpost-managed jobs
  outpost run --list

  # bootout + delete the plist
  outpost run --remove kg3-pipeline

Labels are scoped under "outpost.run.<label>" so --list and --remove
only see outpost-managed jobs.

macOS only — launchctl bootstrap doesn't exist on Linux/Windows.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if listFlag {
				return runList(cmd.OutOrStdout())
			}
			if removeFlag != "" {
				return runRemove(removeFlag)
			}
			if labelFlag == "" {
				return errors.New("--label is required (use --list or --remove to manage existing entries)")
			}
			if len(args) == 0 {
				return errors.New("provide the command to run after --, e.g. `outpost run --label X -- /bin/foo`")
			}
			return runSubmit(runSpec{
				Label:        labelFlag,
				KeepAlive:    keepAliveFlag,
				StdoutPath:   stdoutFlag,
				StderrPath:   stderrFlag,
				WorkDir:      workdirFlag,
				Args:         args,
				ThrottleSecs: throttleSecsFlag,
			})
		},
	}
	cmd.Flags().StringVar(&labelFlag, "label", "",
		"Short label for this job (becomes the plist Label \"outpost.run.<label>\")")
	cmd.Flags().BoolVar(&keepAliveFlag, "keep-alive", false,
		"Relaunch the program if it exits (sets KeepAlive)")
	cmd.Flags().StringVar(&stdoutFlag, "stdout", "",
		"Absolute path to redirect the program's stdout into")
	cmd.Flags().StringVar(&stderrFlag, "stderr", "",
		"Absolute path to redirect the program's stderr into")
	cmd.Flags().StringVar(&workdirFlag, "workdir", "",
		"Absolute working directory for the program (default: $HOME)")
	cmd.Flags().IntVar(&throttleSecsFlag, "throttle", 10,
		"ThrottleInterval (seconds) — minimum time between relaunch attempts")
	cmd.Flags().BoolVar(&listFlag, "list", false,
		"List outpost-managed launchd agents and exit")
	cmd.Flags().StringVar(&removeFlag, "remove", "",
		"Bootout the agent for <label> and delete its plist, then exit")
	return cmd
}

type runSpec struct {
	Label        string
	KeepAlive    bool
	StdoutPath   string
	StderrPath   string
	WorkDir      string
	Args         []string
	ThrottleSecs int
}

func runSubmit(spec runSpec) error {
	if runtime.GOOS != "darwin" {
		return errors.New("outpost run is macOS-only (launchctl bootstrap doesn't exist on this OS); use systemd / Task Scheduler instead")
	}
	if err := validateLabel(spec.Label); err != nil {
		return err
	}
	if !filepath.IsAbs(spec.Args[0]) {
		return fmt.Errorf("first command argument must be an absolute path: %q", spec.Args[0])
	}
	if spec.StdoutPath != "" && !filepath.IsAbs(spec.StdoutPath) {
		return fmt.Errorf("--stdout must be an absolute path: %q", spec.StdoutPath)
	}
	if spec.StderrPath != "" && !filepath.IsAbs(spec.StderrPath) {
		return fmt.Errorf("--stderr must be an absolute path: %q", spec.StderrPath)
	}
	if spec.WorkDir != "" && !filepath.IsAbs(spec.WorkDir) {
		return fmt.Errorf("--workdir must be an absolute path: %q", spec.WorkDir)
	}
	if spec.WorkDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home dir: %w", err)
		}
		spec.WorkDir = home
	}
	if spec.ThrottleSecs <= 0 {
		spec.ThrottleSecs = 10
	}

	plistPath, err := plistPathForLabel(spec.Label)
	if err != nil {
		return err
	}
	// If the same label is already loaded, bootout first so the
	// rewrite is picked up. launchctl considers re-bootstrap an
	// error on macOS 11+ unless the domain is empty for that label.
	uid := strconv.Itoa(os.Getuid())
	fullLabel := labelPrefix + spec.Label
	_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+fullLabel).Run()

	plist, err := renderPlist(spec, fullLabel)
	if err != nil {
		return fmt.Errorf("render plist: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("mkdir LaunchAgents: %w", err)
	}
	if err := os.WriteFile(plistPath, plist, 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, plistPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	fmt.Printf("Loaded %s\n  plist: %s\n  inspect: launchctl print gui/%s/%s\n",
		fullLabel, plistPath, uid, fullLabel)
	return nil
}

func runRemove(label string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("outpost run is macOS-only")
	}
	if err := validateLabel(label); err != nil {
		return err
	}
	plistPath, err := plistPathForLabel(label)
	if err != nil {
		return err
	}
	fullLabel := labelPrefix + label
	uid := strconv.Itoa(os.Getuid())
	out, err := exec.Command("launchctl", "bootout", "gui/"+uid+"/"+fullLabel).CombinedOutput()
	// bootout returns non-zero when the service isn't loaded — we
	// still want to clean up the plist in that case, so don't return
	// early. We only report the launchctl error if the plist write
	// also fails (otherwise the user wanted "make sure it's gone").
	bootoutErr := err
	bootoutOut := strings.TrimSpace(string(out))

	rmErr := os.Remove(plistPath)
	if rmErr != nil && !os.IsNotExist(rmErr) {
		return fmt.Errorf("remove plist: %w", rmErr)
	}
	if bootoutErr != nil && bootoutOut != "" && !strings.Contains(bootoutOut, "Could not find specified service") {
		fmt.Fprintf(os.Stderr, "launchctl bootout reported: %s\n", bootoutOut)
	}
	fmt.Printf("Removed %s (plist: %s)\n", fullLabel, plistPath)
	return nil
}

func runList(out io.Writer) error {
	if runtime.GOOS != "darwin" {
		return errors.New("outpost run is macOS-only")
	}
	dir, err := launchAgentsDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LABEL\tPLIST")
	uid := strconv.Itoa(os.Getuid())
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, labelPrefix) || !strings.HasSuffix(name, ".plist") {
			continue
		}
		label := strings.TrimSuffix(strings.TrimPrefix(name, labelPrefix), ".plist")
		fmt.Fprintf(tw, "%s\t%s\n", label, filepath.Join(dir, name))
		_ = uid // reserved for future "STATUS" column from launchctl print
	}
	return tw.Flush()
}

// validateLabel keeps the label sane for both filesystem and launchd —
// no path separators, no whitespace, reasonable length.
func validateLabel(label string) error {
	if label == "" {
		return errors.New("label is empty")
	}
	if strings.ContainsAny(label, "/\\ \t\n") {
		return fmt.Errorf("label must not contain path separators or whitespace: %q", label)
	}
	if len(label) > 80 {
		return fmt.Errorf("label too long (max 80): %q", label)
	}
	return nil
}

func plistPathForLabel(label string) (string, error) {
	dir, err := launchAgentsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, labelPrefix+label+".plist"), nil
}

func launchAgentsDir() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current user: %w", err)
	}
	if u.HomeDir == "" {
		return "", errors.New("current user has no HomeDir")
	}
	return filepath.Join(u.HomeDir, "Library", "LaunchAgents"), nil
}

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
        {{- range .Args}}
        <string>{{.}}</string>
        {{- end}}
    </array>
    <key>WorkingDirectory</key><string>{{.WorkDir}}</string>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key>{{if .KeepAlive}}<true/>{{else}}<false/>{{end}}
    <key>ThrottleInterval</key><integer>{{.ThrottleSecs}}</integer>
    {{- if .StdoutPath}}
    <key>StandardOutPath</key><string>{{.StdoutPath}}</string>
    {{- end}}
    {{- if .StderrPath}}
    <key>StandardErrorPath</key><string>{{.StderrPath}}</string>
    {{- end}}
</dict>
</plist>
`

// renderPlist generates the plist XML for spec. We use text/template
// (NOT html/template — html/template would escape the leading "<?xml"
// processing instruction itself, producing a plist macOS launchd
// rejects with "Bootstrap failed: 5") and pre-escape user-supplied
// strings via encoding/xml so paths containing `&` / `<` / `>` still
// produce well-formed plist XML.
func renderPlist(spec runSpec, fullLabel string) ([]byte, error) {
	tmpl, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return nil, err
	}
	view := struct {
		Label        string
		Args         []string
		WorkDir      string
		KeepAlive    bool
		ThrottleSecs int
		StdoutPath   string
		StderrPath   string
	}{
		Label:        xmlEscape(fullLabel),
		Args:         xmlEscapeAll(spec.Args),
		WorkDir:      xmlEscape(spec.WorkDir),
		KeepAlive:    spec.KeepAlive,
		ThrottleSecs: spec.ThrottleSecs,
		StdoutPath:   xmlEscape(spec.StdoutPath),
		StderrPath:   xmlEscape(spec.StderrPath),
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, view); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

func xmlEscape(s string) string {
	if s == "" {
		return ""
	}
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

func xmlEscapeAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = xmlEscape(s)
	}
	return out
}
