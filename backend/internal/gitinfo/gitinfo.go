// Package gitinfo inspects the git state of a working directory — current
// branch, origin remote, forge coordinates — so the UI can offer repo-aware
// actions like opening a pull request.
package gitinfo

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wins/jaz/backend/internal/gitjail"
)

// Info is the JSON shape served to the frontend; the zero value means "not a
// git repository".
type Info struct {
	Revision
	Git           bool   `json:"git"`
	DefaultBranch string `json:"default_branch,omitempty"`
	RemoteURL     string `json:"remote_url,omitempty"`
	// WebURL is the https page of the origin remote (no trailing slash),
	// present only when the remote parses as host/owner/repo.
	WebURL string `json:"web_url,omitempty"`
	Host   string `json:"host,omitempty"`
	Owner  string `json:"owner,omitempty"`
	Repo   string `json:"repo,omitempty"`
	// HasUpstream reports whether the current branch tracks a remote branch,
	// i.e. it has been pushed somewhere a pull request could start from.
	HasUpstream bool   `json:"has_upstream,omitempty"`
	Upstream    string `json:"upstream,omitempty"`
	PushAhead   *int   `json:"push_ahead,omitempty"`
	// NoCommits is set when the branch is confirmed to have zero commits on
	// top of the default branch — a PR opened from it would be empty.
	NoCommits bool `json:"no_commits,omitempty"`
	// NeedsPush reports commits the remote doesn't have: an unpushed branch
	// with its own commits, or an upstream the branch is ahead of.
	NeedsPush bool `json:"needs_push,omitempty"`
	// Dirty reports uncommitted changes in the working tree.
	Dirty *bool `json:"dirty,omitempty"`
	// IsWorktree marks a linked worktree (not the repository's main checkout);
	// MainBranch is the branch checked out in the main checkout, the handoff
	// destination.
	IsWorktree bool   `json:"is_worktree,omitempty"`
	MainBranch string `json:"main_branch,omitempty"`
	Ahead      *int   `json:"ahead,omitempty"`
	// Behind counts commits on MainBranch the worktree's branch doesn't
	// have yet — what an update would pull in (0 when up to date).
	Behind int `json:"behind,omitempty"`
	// WorktreeMissing reports a managed worktree path that no longer exists.
	WorktreeMissing bool `json:"worktree_missing,omitempty"`
	// WorktreeRestorable reports whether the saved session branch can recreate
	// the missing worktree.
	WorktreeRestorable bool   `json:"worktree_restorable,omitempty"`
	WorktreeBranch     string `json:"worktree_branch,omitempty"`
}

// Inspect collects Info for dir. It never errors: a non-repo (or missing git
// binary) yields the zero Info, and each field degrades independently.
func Inspect(ctx context.Context, dir string) Info {
	if _, err := git(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return Info{}
	}
	info := Info{Git: true, Revision: CurrentRevision(ctx, dir)}
	if upstream, err := git(ctx, dir, "rev-parse", "--abbrev-ref", "@{upstream}"); err == nil {
		info.HasUpstream = true
		info.Upstream = upstream
	}
	// Local state first — it must survive the no-remote return below, since
	// commit and handoff don't need a remote.
	if out, err := git(ctx, dir, "status", "--porcelain"); err == nil {
		dirty := out != ""
		info.Dirty = &dirty
	}
	_, info.MainBranch, info.IsWorktree = mainCheckout(ctx, dir)
	if info.IsWorktree {
		branch := info.MainBranch
		if branch != "" {
			if counts, err := git(ctx, dir, "rev-list", "--left-right", "--count", "HEAD...refs/heads/"+branch); err == nil {
				var ahead int
				if _, err := fmt.Sscanf(counts, "%d %d", &ahead, &info.Behind); err == nil {
					info.Ahead = &ahead
				}
			}
		}
	}
	remote, err := git(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		// No origin — fall back to the first configured remote, if any.
		remote = firstRemoteURL(ctx, dir)
	}
	if remote == "" {
		return info
	}
	info.RemoteURL = remote
	info.Host, info.Owner, info.Repo = ParseRemote(remote)
	if info.Host != "" {
		info.WebURL = "https://" + info.Host + "/" + info.Owner + "/" + info.Repo
	}
	info.DefaultBranch = defaultBranch(ctx, dir)
	if info.Branch != "" && info.DefaultBranch != "" && info.Branch != info.DefaultBranch {
		// Prefer the local default branch as base (worktrees always have it);
		// fall back to the remote-tracking ref for clones without one.
		for _, base := range []string{"refs/heads/" + info.DefaultBranch, "refs/remotes/origin/" + info.DefaultBranch} {
			if count, err := git(ctx, dir, "rev-list", "--count", base+"..HEAD"); err == nil {
				info.NoCommits = count == "0"
				break
			}
		}
	}
	if info.Branch != "" {
		if info.HasUpstream {
			if count, err := git(ctx, dir, "rev-list", "--count", "@{upstream}..HEAD"); err == nil {
				var ahead int
				if _, err := fmt.Sscanf(count, "%d", &ahead); err == nil {
					info.PushAhead = &ahead
					info.NeedsPush = ahead > 0
				}
			}
		} else if !info.NoCommits {
			// Unpushed branch: publishable once it has commits of its own.
			// (NoCommits stays false when the base is unknown — then err on
			// the side of offering the push.)
			info.NeedsPush = true
		}
	}
	return info
}

// mainCheckout returns the main checkout and whether dir is a linked worktree.
func mainCheckout(ctx context.Context, dir string) (root, branch string, ok bool) {
	gitDir, err := git(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", "", false
	}
	common, err := git(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", "", false
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	common = filepath.Clean(common)
	// git prints realpaths while dir may arrive through a symlink (macOS
	// /tmp); resolve both sides or a main checkout looks like a worktree.
	if resolved, err := filepath.EvalSymlinks(common); err == nil {
		common = resolved
	}
	if resolved, err := filepath.EvalSymlinks(gitDir); err == nil {
		gitDir = resolved
	}
	root = filepath.Dir(common)
	branch, _ = git(ctx, root, "symbolic-ref", "--short", "-q", "HEAD")
	return root, branch, gitDir != common
}

// CommitAll stages everything in dir and commits it with message; a clean
// tree is a no-op rather than an error, so callers can chain it freely.
func CommitAll(ctx context.Context, dir, message string) error {
	out, err := git(ctx, dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if out == "" {
		return nil
	}
	if _, err := git(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	_, err = git(ctx, dir, "commit", "-m", message)
	return err
}

func snapshotAll(ctx context.Context, dir, message string) error {
	out, err := git(ctx, dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if out == "" {
		return nil
	}
	if _, err := git(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	_, err = git(ctx, dir, "commit", "--no-verify", "-m", message)
	return err
}

// Handoff applies the worktree's changes — committed and uncommitted,
// relative to where it branched off — onto the repository's main checkout as
// committed history: any dirty work in the worktree is committed on its
// branch first (with message), then that branch is merged into the branch the
// main checkout has checked out — fast-forward when main hasn't moved, a
// merge commit otherwise. A conflicting merge is aborted and reported rather
// than left half-done, so the main checkout never ends up mid-merge. Returns
// the main checkout path.
func MergeIntoMain(ctx context.Context, dir, message string) (string, error) {
	root, branch, ok := mainCheckout(ctx, dir)
	if !ok {
		return "", fmt.Errorf("the session is not running on a worktree")
	}
	if branch == "" {
		return "", fmt.Errorf("the main checkout is on a detached HEAD")
	}
	worktreeBranch, _ := git(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if worktreeBranch == "" {
		return "", fmt.Errorf("the worktree is on a detached HEAD")
	}
	if err := CommitAll(ctx, dir, message); err != nil {
		return "", err
	}
	return mergeBranch(ctx, root, branch, "refs/heads/"+worktreeBranch)
}

func mergeBranch(ctx context.Context, root, branch, source string) (string, error) {
	if _, err := git(ctx, root, "rev-parse", "--verify", "--quiet", "MERGE_HEAD"); err == nil {
		return "", fmt.Errorf("the destination already has an unfinished merge")
	}
	count, err := git(ctx, root, "rev-list", "--count", "refs/heads/"+branch+".."+source)
	if err != nil {
		return "", err
	}
	if count == "0" {
		return "", fmt.Errorf("nothing to merge — %s already has everything from %s", branch, source)
	}
	current, err := git(ctx, root, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil || current != branch {
		return "", fmt.Errorf("merge destination changed or is unavailable")
	}
	if _, err := git(ctx, root, "merge", "--no-edit", source); err != nil {
		// A conflicted merge leaves MERGE_HEAD state behind; abort it so the
		// main checkout stays pristine. (Refusals — e.g. local changes would
		// be overwritten — never start a merge; the abort then is a no-op.)
		_, _ = git(ctx, root, "merge", "--abort")
		return "", fmt.Errorf("merging %s into %s: %w", source, branch, err)
	}
	return root, nil
}

func MergeFromMain(ctx context.Context, dir, message string) error {
	_, branch, ok := mainCheckout(ctx, dir)
	if !ok {
		return fmt.Errorf("the session is not running on a worktree")
	}
	if branch == "" {
		return fmt.Errorf("the main checkout is on a detached HEAD")
	}
	worktreeBranch, _ := git(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if worktreeBranch == "" {
		return fmt.Errorf("the worktree is on a detached HEAD")
	}
	source := "refs/heads/" + branch
	if err := CommitAll(ctx, dir, message); err != nil {
		return err
	}
	if count, err := git(ctx, dir, "rev-list", "--count", worktreeBranch+".."+source); err == nil && count == "0" {
		return fmt.Errorf("nothing to merge — %s already has everything from %s", worktreeBranch, branch)
	}
	if _, err := git(ctx, dir, "merge", "--no-edit", source); err != nil {
		// Abort a conflicted merge so the worktree never sits mid-merge.
		_, _ = git(ctx, dir, "merge", "--abort")
		return fmt.Errorf("merging %s into %s: %w", branch, worktreeBranch, err)
	}
	return nil
}

// Push publishes the current branch to the repository's remote (origin, or
// the first configured remote) with -u, so a pull request can be opened from
// it and later pushes need no flags.
func Push(ctx context.Context, dir string) error {
	remote := "origin"
	if _, err := git(ctx, dir, "remote", "get-url", remote); err != nil {
		names, err := git(ctx, dir, "remote")
		if err != nil || names == "" {
			return fmt.Errorf("no git remote configured")
		}
		name, _, _ := strings.Cut(names, "\n")
		remote = strings.TrimSpace(name)
	}
	url, err := git(ctx, dir, "remote", "get-url", remote)
	if err != nil {
		return err
	}
	access, err := pushAccess(dir, url)
	if err != nil {
		return err
	}
	_, err = gitIn(ctx, dir, access, nil, "push", "-u", remote, "HEAD")
	return err
}

// pushAccess is what a push to url takes: the network, or for a repository
// on this machine, writing it through git's shell-run receive-pack. Only a
// bare repository qualifies, so the repository's own config cannot point the
// push at an arbitrary directory.
func pushAccess(dir, url string) (gitjail.Policy, error) {
	path, local := strings.CutPrefix(url, "file://")
	if !local {
		host, _, _ := strings.Cut(url, "/")
		local = !strings.Contains(url, "://") && !strings.Contains(host, ":")
	}
	if !local {
		return gitjail.Policy{Network: true}, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return gitjail.Policy{}, err
	}
	head, headErr := os.Stat(filepath.Join(path, "HEAD"))
	objects, objectsErr := os.Stat(filepath.Join(path, "objects"))
	if headErr != nil || objectsErr != nil || !head.Mode().IsRegular() || !objects.IsDir() {
		return gitjail.Policy{}, fmt.Errorf("push to %s: only bare repositories on this machine are supported", url)
	}
	return gitjail.Policy{Writable: []string{path}, Shell: true}, nil
}

func BranchExists(ctx context.Context, dir, branch string) bool {
	if strings.TrimSpace(branch) == "" {
		return false
	}
	_, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func RemoveManagedWorktree(ctx context.Context, dir, branch, message string) error {
	root, _, ok := mainCheckout(ctx, dir)
	if !ok {
		return fmt.Errorf("the session is not running on a worktree")
	}
	current, _ := git(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if current == "" {
		return fmt.Errorf("the worktree is on a detached HEAD")
	}
	if current != branch {
		return fmt.Errorf("worktree branch %q does not match %q", current, branch)
	}
	reflog, err := git(ctx, dir, "reflog", "show", "--format=%H", "HEAD")
	if err != nil {
		return err
	}
	if revisions := strings.Fields(reflog); len(revisions) > 0 {
		args := append([]string{"rev-list"}, revisions...)
		unreferenced, err := git(ctx, dir, append(args, "--not", "--all")...)
		if err != nil {
			return err
		}
		if unreferenced != "" {
			return fmt.Errorf("worktree has reflog commits without a durable ref; preserve them before pruning")
		}
	}
	if err := snapshotAll(ctx, dir, message); err != nil {
		return err
	}
	// Removing the worktree directory itself takes write access to its parent.
	if _, err := gitIn(ctx, root, gitjail.Policy{Writable: []string{dir, filepath.Dir(dir)}}, nil, "worktree", "remove", "--force", dir); err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	_, _ = git(ctx, root, "worktree", "prune")
	return nil
}

func RestoreManagedWorktree(ctx context.Context, repo, worktree, branch string) error {
	root, err := git(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("restore worktree requires a git repository at %s: %w", repo, err)
	}
	if !BranchExists(ctx, root, branch) {
		return fmt.Errorf("worktree branch %q was not found", branch)
	}
	if _, err := os.Stat(worktree); err == nil {
		return fmt.Errorf("worktree already exists: %s", worktree)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		return err
	}
	_, _ = git(ctx, root, "worktree", "prune")
	if err := addWorktree(ctx, root, worktree, "worktree", "add", worktree, branch); err != nil {
		return fmt.Errorf("restore worktree: %w", err)
	}
	return nil
}

func PruneWorktreeMetadata(ctx context.Context, repo string) error {
	root, err := git(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	_, err = git(ctx, root, "worktree", "prune")
	return err
}

func firstRemoteURL(ctx context.Context, dir string) string {
	names, err := git(ctx, dir, "remote")
	if err != nil || names == "" {
		return ""
	}
	name, _, _ := strings.Cut(names, "\n")
	url, _ := git(ctx, dir, "remote", "get-url", strings.TrimSpace(name))
	return url
}

// defaultBranch resolves the remote's default branch: origin/HEAD when the
// clone recorded it, otherwise whichever of main/master exists on origin.
func defaultBranch(ctx context.Context, dir string) string {
	if ref, err := git(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(ref, "origin/")
	}
	for _, name := range []string{"main", "master"} {
		if _, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+name); err == nil {
			return name
		}
	}
	return ""
}

// ParseRemote extracts (host, owner, repo) from a git remote URL in https,
// ssh://, or scp-like (git@host:owner/repo.git) form. Forges with nested
// namespaces keep everything but the last path segment as the owner.
// Unparseable remotes (local paths, bare names) yield empty strings.
func ParseRemote(remote string) (host, owner, repo string) {
	remote = strings.TrimSpace(remote)
	var path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Hostname() == "" {
			return "", "", ""
		}
		host = u.Hostname()
		path = u.Path
	} else {
		// scp-like: [user@]host:path — a remote without a colon is a local path.
		rest := remote
		if _, after, found := strings.Cut(remote, "@"); found {
			rest = after
		}
		var ok bool
		host, path, ok = strings.Cut(rest, ":")
		if !ok || host == "" || strings.Contains(host, "/") {
			return "", "", ""
		}
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	segments := strings.Split(path, "/")
	if len(segments) < 2 {
		return "", "", ""
	}
	owner = strings.Join(segments[:len(segments)-1], "/")
	repo = segments[len(segments)-1]
	if owner == "" || repo == "" {
		return "", "", ""
	}
	return host, owner, repo
}

// AddWorktree creates a disposable git worktree of the repository containing
// dir at workspace/.worktrees/<slug>, on a fresh "jaz/<slug>" branch. baseRef
// selects the branch point; empty uses dir's current HEAD.
func AddWorktree(ctx context.Context, workspace, dir, slug, baseRef string) (string, string, error) {
	repo, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("worktree requires a git repository at %s: %w", dir, err)
	}
	projectPath := repo
	if root, _, ok := mainCheckout(ctx, dir); ok {
		projectPath = root
	}
	worktree := filepath.Join(workspace, ".worktrees", slug)
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		return "", "", err
	}
	args := []string{"worktree", "add", "-b", "jaz/" + slug, worktree}
	if baseRef = strings.TrimSpace(baseRef); baseRef != "" {
		args = append(args, baseRef)
	}
	if err := addWorktree(ctx, repo, worktree, args...); err != nil {
		return "", "", fmt.Errorf("create worktree: %w", err)
	}
	return worktree, projectPath, nil
}

// addWorktree runs `git worktree add` into a directory Jaz creates, so jailed
// git writes only that directory besides the repository.
func addWorktree(ctx context.Context, repo, worktree string, args ...string) error {
	if err := os.Mkdir(worktree, 0o755); err != nil {
		return err
	}
	_, err := gitIn(ctx, repo, gitjail.Policy{Writable: []string{worktree}}, nil, args...)
	if err != nil {
		_ = os.Remove(worktree)
	}
	return err
}

// ListFiles returns the repository's non-ignored files under dir (tracked
// plus untracked), slash-separated and relative to dir — the same view of the
// tree a `.gitignore`-respecting walker would produce. Errors when dir is not
// inside a git working tree (or git is unavailable); callers fall back to a
// plain filesystem walk.
func ListFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := git(ctx, dir, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	return splitNul(out), nil
}

// splitNul splits NUL-terminated git output (-z flags) into its non-empty
// records.
func splitNul(out string) []string {
	parts := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	fields := parts[:0]
	for _, part := range parts {
		if part != "" {
			fields = append(fields, part)
		}
	}
	return fields
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	return gitWithOptions(ctx, dir, nil, args...)
}

func gitWithOptions(ctx context.Context, dir string, options []string, args ...string) (string, error) {
	return gitIn(ctx, dir, gitjail.Policy{}, options, args...)
}

// command runs git in dir under gitjail, with extra granting what the
// command needs beyond dir's repository. Repository settings that would run
// commands are switched off where Jaz can do without them, so jailed git does
// not fail on them.
func command(ctx context.Context, dir string, extra gitjail.Policy, options []string, args ...string) (*gitjail.Cmd, error) {
	policy, err := gitjail.ForRepo(dir)
	if err != nil {
		return nil, err
	}
	if len(args) > 0 && args[0] == "init" {
		policy.Writable = append(policy.Writable, dir)
	}
	policy.Writable = append(policy.Writable, extra.Writable...)
	policy.Network, policy.Shell = extra.Network, extra.Shell
	if len(args) > 0 && (args[0] == "commit" || args[0] == "verify-commit") {
		policy.Shell = true
		policy.Readable = append(policy.Readable, signingPaths(ctx, dir)...)
	}
	gitArgs := []string{"-C", dir, "-c", "core.fsmonitor=false"}
	if len(args) == 0 || args[0] != "commit" || slices.Contains(args, "--no-verify") {
		gitArgs = append(gitArgs, "-c", "core.hooksPath=/dev/null")
	}
	gitArgs = append(gitArgs, options...)
	if len(args) > 0 && args[0] == "diff" {
		args = append([]string{"diff", "--no-ext-diff", "--no-textconv"}, args[1:]...)
	}
	cmd, err := gitjail.Command(ctx, policy, append(gitArgs, args...)...)
	if err == nil {
		cmd.Env = append(os.Environ(), "TMPDIR="+dir)
	}
	return cmd, err
}

func signingPaths(ctx context.Context, dir string) []string {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get-regexp", `^(user\.signingkey|gpg\.ssh\.allowedSignersFile)$`).Output()
	if err != nil {
		return nil
	}
	var paths []string
	for line := range strings.Lines(strings.TrimSpace(string(out))) {
		_, path, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if strings.HasPrefix(path, "~/") {
			home, err := os.UserHomeDir()
			if err == nil {
				path = filepath.Join(home, path[2:])
			}
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		paths = append(paths, path)
	}
	return paths
}

func gitIn(ctx context.Context, dir string, extra gitjail.Policy, options []string, args ...string) (string, error) {
	cmd, err := command(ctx, dir, extra, options, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Surface git's own message (e.g. "branch already exists") over the
		// bare exit status; probes that expect failure just discard it.
		detail := strings.TrimSpace(out.String())
		if extra := strings.TrimSpace(stderr.String()); extra != "" {
			if detail != "" {
				detail += "\n"
			}
			detail += extra
		}
		if detail != "" {
			return "", fmt.Errorf("git %s: %s", args[0], detail)
		}
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}
