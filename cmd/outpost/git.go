package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	outgit "github.com/qiangli/outpost/internal/agent/git"
)

// outpost git … — embedded git client. Pure-Go go-git backend so the
// command works without a system `git` binary, which is the load-
// bearing case on Windows. Scope is the typical clone → edit → add →
// commit → push lifecycle plus the read/inspect siblings; rebase,
// stash, merge, tag, reset, blame, submodules, worktrees, reflog,
// bisect are intentionally out of scope.
//
// `outpost git ...` always resolves to this implementation regardless
// of whether a system `git` is on PATH.
func gitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "git",
		Short: "Embedded git client (clone, pull, status, commit, push, …)",
		Long: `outpost git is a small, self-contained git client for the typical
development cycle: clone → edit → add → commit → push, plus the
common read paths (status, diff, log, branch, show, remote, fetch,
pull).

It is implemented on top of go-git/v5 and does NOT require a system
'git' binary — that's the point: outpost stays self-sufficient on
Windows machines where setting up real git is painful.

Out of scope (use system git if you need them): rebase, stash, merge,
tag, reset, blame, submodules, worktrees, reflog, bisect.

Authentication for HTTPS remotes uses --username/--password when
supplied, otherwise falls back to $GITHUB_TOKEN or $GIT_TOKEN as the
basic-auth password (with user "oauth2", which GitHub accepts).`,
	}
	cmd.AddCommand(
		gitCloneCmd(),
		gitInitCmd(),
		gitAddCmd(),
		gitCommitCmd(),
		gitStatusCmd(),
		gitLogCmd(),
		gitPushCmd(),
		gitPullCmd(),
		gitFetchCmd(),
		gitBranchCmd(),
		gitCheckoutCmd(),
		gitDiffCmd(),
		gitRemoteCmd(),
		gitShowCmd(),
	)
	return cmd
}

func gitCloneCmd() *cobra.Command {
	var (
		depth        int
		branch       string
		singleBranch bool
		username     string
		password     string
		sshKey       string
		sshKeyPass   string
		quiet        bool
	)
	cmd := &cobra.Command{
		Use:   "clone <url> [directory]",
		Short: "Clone a repository",
		Args:  cobra.RangeArgs(1, 2),
		Example: `  outpost git clone https://github.com/user/repo.git
  outpost git clone https://github.com/user/repo.git ./my-project
  outpost git clone https://github.com/user/repo.git --depth 1
  outpost git clone https://github.com/user/repo.git --branch v1.2.3 --single-branch
  GITHUB_TOKEN=ghp_xxx outpost git clone https://github.com/org/private.git`,
		RunE: func(cmd *cobra.Command, args []string) error {
			url := args[0]
			var path string
			if len(args) > 1 {
				path = args[1]
			}
			var progress io.Writer = cmd.ErrOrStderr()
			if quiet {
				progress = nil
			}
			result, err := outgit.Clone(outgit.CloneOptions{
				URL:          url,
				Path:         path,
				Depth:        depth,
				Branch:       branch,
				SingleBranch: singleBranch,
				Auth: outgit.AuthConfig{
					Username:   username,
					Password:   password,
					SSHKey:     sshKey,
					SSHKeyPass: sshKeyPass,
				},
				Progress: progress,
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), result.Message)
			return nil
		},
	}
	cmd.Flags().IntVar(&depth, "depth", 0, "Shallow clone with depth")
	cmd.Flags().StringVarP(&branch, "branch", "b", "", "Branch or tag to check out (instead of remote HEAD)")
	cmd.Flags().BoolVar(&singleBranch, "single-branch", false, "When --branch is set, fetch only that ref")
	cmd.Flags().StringVar(&username, "username", "", "Username for HTTPS basic auth")
	cmd.Flags().StringVar(&password, "password", "", "Password/token for HTTPS basic auth")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "Path to SSH private key (use with ssh:// URLs)")
	cmd.Flags().StringVar(&sshKeyPass, "ssh-key-pass", "", "Passphrase for --ssh-key (if encrypted)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress progress output")
	return cmd
}

func gitInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init [directory]",
		Short: "Initialize a git repository",
		Args:  cobra.MaximumNArgs(1),
		Example: `  outpost git init
  outpost git init ./my-project`,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) > 0 {
				path = args[0]
			}
			result, err := outgit.Init(outgit.InitOptions{Path: path})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), result.Message)
			return nil
		},
	}
}

func gitAddCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "add <path>...",
		Short: "Add files to the staging area",
		Args:  cobra.MinimumNArgs(0),
		Example: `  outpost git add file.txt
  outpost git add .
  outpost git add -A`,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := "."
			if all || (len(args) == 1 && args[0] == ".") {
				result, err := outgit.Add(outgit.AddOptions{RepoPath: repo, All: true})
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), result.Message)
				return nil
			}
			if len(args) == 0 {
				return fmt.Errorf("nothing to add — pass a path or -A")
			}
			for _, p := range args {
				result, err := outgit.Add(outgit.AddOptions{RepoPath: repo, Path: p})
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), result.Message)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "A", false, "Stage all changes")
	return cmd
}

func gitCommitCmd() *cobra.Command {
	var (
		message     string
		amend       bool
		all         bool
		authorName  string
		authorEmail string
	)
	cmd := &cobra.Command{
		Use:   "commit [message]",
		Short: "Record staged changes as a new commit",
		Args:  cobra.MaximumNArgs(1),
		Example: `  outpost git commit -m "Initial commit"
  outpost git commit -a -m "Fix bug"        # stage tracked + commit
  outpost git commit --amend -m "Reword"
  outpost git commit --author-name "Alice" --author-email "alice@example.com" -m "..."`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if message == "" && len(args) > 0 {
				message = args[0]
			}
			result, err := outgit.Commit(outgit.CommitOptions{
				Message:     message,
				Amend:       amend,
				All:         all,
				AuthorName:  authorName,
				AuthorEmail: authorEmail,
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), result.Message)
			return nil
		},
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "Commit message")
	cmd.Flags().BoolVar(&amend, "amend", false, "Amend the previous commit")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "Stage tracked-and-modified files before committing")
	cmd.Flags().StringVar(&authorName, "author-name", "", "Override author name (otherwise read from repo/global config)")
	cmd.Flags().StringVar(&authorEmail, "author-email", "", "Override author email")
	return cmd
}

func gitStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "status [path]",
		Short:   "Show working tree status",
		Args:    cobra.MaximumNArgs(1),
		Example: `  outpost git status`,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := "."
			if len(args) > 0 {
				repo = args[0]
			}
			result, entries, err := outgit.Status(repo)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if entries == nil {
				fmt.Fprintln(out, result.Message)
				return nil
			}
			var unstaged, staged []outgit.StatusEntry
			for _, e := range entries {
				if e.Staged {
					staged = append(staged, e)
				} else {
					unstaged = append(unstaged, e)
				}
			}
			if len(staged) > 0 {
				fmt.Fprintln(out, "Changes to be committed:")
				for _, e := range staged {
					fmt.Fprintf(out, "  %s  %s\n", e.Status, e.File)
				}
			}
			if len(unstaged) > 0 {
				if len(staged) > 0 {
					fmt.Fprintln(out)
				}
				fmt.Fprintln(out, "Changes not staged for commit:")
				for _, e := range unstaged {
					fmt.Fprintf(out, "  %s  %s\n", e.Status, e.File)
				}
			}
			return nil
		},
	}
}

func gitLogCmd() *cobra.Command {
	var number int
	cmd := &cobra.Command{
		Use:   "log [path]",
		Short: "Show commit history (most recent first)",
		Args:  cobra.MaximumNArgs(1),
		Example: `  outpost git log
  outpost git log -n 20`,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := "."
			if len(args) > 0 {
				repo = args[0]
			}
			_, entries, err := outgit.Log(outgit.LogOptions{RepoPath: repo, Number: number})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, e := range entries {
				fmt.Fprintf(out, "%s  %s\n", e.Hash, e.Message)
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&number, "number", "n", 10, "Number of commits to show")
	return cmd
}

func gitPushCmd() *cobra.Command {
	var (
		force    bool
		username string
		password string
		sshKey   string
	)
	cmd := &cobra.Command{
		Use:   "push [remote] [branch]",
		Short: "Push the current branch to a remote",
		Args:  cobra.MaximumNArgs(2),
		Example: `  outpost git push
  outpost git push origin main
  outpost git push --force
  GITHUB_TOKEN=ghp_xxx outpost git push origin feature`,
		RunE: func(cmd *cobra.Command, args []string) error {
			remoteName := ""
			branch := ""
			if len(args) > 0 {
				remoteName = args[0]
			}
			if len(args) > 1 {
				branch = args[1]
			}
			result, err := outgit.Push(outgit.PushOptions{
				Remote: remoteName,
				Branch: branch,
				Force:  force,
				Auth: outgit.AuthConfig{
					Username: username,
					Password: password,
					SSHKey:   sshKey,
				},
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), result.Message)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Force push (replaces remote history)")
	cmd.Flags().StringVar(&username, "username", "", "Username for HTTPS basic auth")
	cmd.Flags().StringVar(&password, "password", "", "Password/token for HTTPS basic auth")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "Path to SSH private key (ssh:// remotes)")
	return cmd
}

func gitPullCmd() *cobra.Command {
	var (
		username string
		password string
		sshKey   string
	)
	cmd := &cobra.Command{
		Use:   "pull [remote] [branch]",
		Short: "Fetch and integrate from a remote branch",
		Args:  cobra.MaximumNArgs(2),
		Example: `  outpost git pull
  outpost git pull origin main`,
		RunE: func(cmd *cobra.Command, args []string) error {
			remoteName := ""
			branch := ""
			if len(args) > 0 {
				remoteName = args[0]
			}
			if len(args) > 1 {
				branch = args[1]
			}
			result, err := outgit.Pull(outgit.PullOptions{
				Remote: remoteName,
				Branch: branch,
				Auth: outgit.AuthConfig{
					Username: username,
					Password: password,
					SSHKey:   sshKey,
				},
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), result.Message)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "Username for HTTPS basic auth")
	cmd.Flags().StringVar(&password, "password", "", "Password/token for HTTPS basic auth")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "Path to SSH private key (ssh:// remotes)")
	return cmd
}

func gitFetchCmd() *cobra.Command {
	var (
		username string
		password string
		sshKey   string
	)
	cmd := &cobra.Command{
		Use:   "fetch [remote]",
		Short: "Download objects and refs from a remote",
		Args:  cobra.MaximumNArgs(1),
		Example: `  outpost git fetch
  outpost git fetch origin`,
		RunE: func(cmd *cobra.Command, args []string) error {
			remoteName := ""
			if len(args) > 0 {
				remoteName = args[0]
			}
			result, err := outgit.Fetch(outgit.FetchOptions{
				Remote: remoteName,
				Auth: outgit.AuthConfig{
					Username: username,
					Password: password,
					SSHKey:   sshKey,
				},
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), result.Message)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "Username for HTTPS basic auth")
	cmd.Flags().StringVar(&password, "password", "", "Password/token for HTTPS basic auth")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "Path to SSH private key (ssh:// remotes)")
	return cmd
}

func gitBranchCmd() *cobra.Command {
	var (
		del   bool
		force bool
	)
	cmd := &cobra.Command{
		Use:   "branch [name]",
		Short: "List, create, or delete branches",
		Args:  cobra.MaximumNArgs(1),
		Example: `  outpost git branch                     # list, marks current with *
  outpost git branch feature-x          # create
  outpost git branch -d old-branch      # delete`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			result, branches, err := outgit.Branch(outgit.BranchOptions{
				Name:   name,
				Delete: del,
				Force:  force,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if branches != nil {
				for _, b := range branches {
					fmt.Fprintln(out, b)
				}
				return nil
			}
			fmt.Fprintln(out, result.Message)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&del, "delete", "d", false, "Delete the named branch")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Force operation (with --delete)")
	return cmd
}

func gitCheckoutCmd() *cobra.Command {
	var create bool
	cmd := &cobra.Command{
		Use:   "checkout <branch>",
		Short: "Switch to a branch (optionally create it)",
		Args:  cobra.ExactArgs(1),
		Example: `  outpost git checkout main
  outpost git checkout -b feature-x      # create and switch`,
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := outgit.Checkout(outgit.CheckoutOptions{
				Branch: args[0],
				Create: create,
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), result.Message)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&create, "branch", "b", false, "Create the branch before switching")
	return cmd
}

func gitDiffCmd() *cobra.Command {
	var staged bool
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Show changes (file-level summary)",
		Example: `  outpost git diff
  outpost git diff --staged`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, entries, err := outgit.Status(".")
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			label := "Working tree changes:"
			wantStaged := staged
			if wantStaged {
				label = "Staged changes:"
			}
			matched := false
			for _, e := range entries {
				if e.Staged == wantStaged {
					if !matched {
						fmt.Fprintln(out, label)
						matched = true
					}
					fmt.Fprintf(out, "  %s  %s\n", e.Status, e.File)
				}
			}
			if !matched {
				fmt.Fprintln(out, "no changes")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&staged, "staged", false, "Show staged (index) changes instead of working-tree changes")
	return cmd
}

func gitRemoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remote",
		Short:   "List configured remotes",
		Example: `  outpost git remote`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, remotes, err := outgit.Remotes(".")
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, r := range remotes {
				for _, u := range r.URLs {
					fmt.Fprintf(out, "%s\t%s\n", r.Name, u)
				}
			}
			return nil
		},
	}
}

func gitShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [commit]",
		Short: "Show details of a commit (defaults to HEAD)",
		Args:  cobra.MaximumNArgs(1),
		Example: `  outpost git show
  outpost git show HEAD~1
  outpost git show v1.2.3`,
		RunE: func(cmd *cobra.Command, args []string) error {
			commit := ""
			if len(args) > 0 {
				commit = args[0]
			}
			_, info, err := outgit.Show(outgit.ShowOptions{Commit: commit})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "commit %s\n", info.Hash)
			fmt.Fprintf(out, "Author: %s <%s>\n", info.Author, info.Email)
			fmt.Fprintf(out, "Date:   %s\n\n", info.Date)
			fmt.Fprintln(out, info.Message)
			return nil
		},
	}
}
