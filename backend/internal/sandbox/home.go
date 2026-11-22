package sandbox

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

// A session's HOME is its own: one session cannot leave a .bashrc, a git
// hook or a poisoned cache for another. The directory behind Home is only
// the seed; sessions never write to it.

// homeVolumes are the `podman run --volume` values that make home the
// container's HOME, with the shared entries read-only from the seed.
func (c RunnerConfig) homeVolumes(home string) ([]string, error) {
	seed, err := c.resolve(c.Home, false)
	if err != nil {
		return nil, err
	}
	volumes := []string{home + ":" + c.Home + ":idmap"}
	for _, rel := range c.HomeShared {
		if _, err := os.Lstat(filepath.Join(seed, rel)); err != nil {
			continue
		}
		volumes = append(volumes, filepath.Join(seed, rel)+":"+filepath.Join(c.Home, rel)+":idmap,ro")
	}
	return volumes, nil
}

// sessionHome is the host directory to mount as a launch's HOME: the
// session's own, seeded on first use, or for a launch without a session a
// fresh one removed by the returned cleanup.
func (c RunnerConfig) sessionHome(name, session string) (string, func(), error) {
	seed, err := c.resolve(c.Home, false)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(c.Homes, 0o700); err != nil {
		return "", nil, err
	}
	if session == "" {
		dir := filepath.Join(c.Homes, ".launch-"+name)
		if err := c.seedHome(seed, dir); err != nil {
			os.RemoveAll(dir)
			return "", nil, err
		}
		return dir, func() { os.RemoveAll(dir) }, nil
	}
	if err := validSessionID(session); err != nil {
		return "", nil, err
	}
	dir := filepath.Join(c.Homes, session)
	if _, err := os.Lstat(dir); err == nil {
		return dir, func() {}, nil
	}
	// Seeded aside and renamed into place, so a concurrent launch of the same
	// session never sees a half-copied HOME.
	tmp := filepath.Join(c.Homes, ".seed-"+name)
	if err := c.seedHome(seed, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", nil, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		if _, serr := os.Lstat(dir); serr != nil {
			return "", nil, err
		}
	}
	return dir, func() {}, nil
}

// seedHome copies seed to dir, keeping owners, modes and symlinks, without
// the skipped entries (caches). Shared entries become empty mount points for
// the read-only originals.
func (c RunnerConfig) seedHome(seed, dir string) error {
	return filepath.WalkDir(seed, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(seed, path)
		if err != nil {
			return err
		}
		if slices.Contains(c.HomeSkip, rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(dir, rel)
		shared := slices.Contains(c.HomeShared, rel)
		switch {
		case shared && entry.IsDir():
			// An empty mount point, owned like the original.
			if err := os.Mkdir(target, info.Mode().Perm()); err != nil {
				return err
			}
		case shared:
			if err := copyFile(os.DevNull, target, info.Mode().Perm()); err != nil {
				return err
			}
		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		case entry.IsDir():
			if err := os.Mkdir(target, info.Mode().Perm()); err != nil {
				return err
			}
			if err := os.Chmod(target, info.Mode().Perm()); err != nil {
				return err
			}
		case entry.Type().IsRegular():
			if err := copyFile(path, target, info.Mode().Perm()); err != nil {
				return err
			}
		default:
			return nil
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			if err := os.Lchown(target, int(stat.Uid), int(stat.Gid)); err != nil {
				return fmt.Errorf("seed %s: %w", rel, err)
			}
		}
		if shared && entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
