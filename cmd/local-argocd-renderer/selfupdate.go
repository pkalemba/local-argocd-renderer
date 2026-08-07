package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/creativeprojects/go-selfupdate"
	"github.com/creativeprojects/go-selfupdate/update"
)

// updateRepo is where the release assets live.
const updateRepo = "pkalemba/local-argocd-renderer"

// releaseVersion matches the tags semantic-release creates, and nothing else.
//
// The Makefile stamps local builds with `git describe --tags --always --dirty`,
// which yields either a bare commit SHA or something like v1.1.0-3-gabc1234 — and
// semver reads the latter as a *prerelease* of v1.1.0, so it sorts below the tag it
// was built from. Updating such a build would quietly move it backwards.
var releaseVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

func isReleaseVersion(version string) bool {
	return releaseVersion.MatchString(version)
}

// managedPaths hold binaries owned by a package manager: the store is read-only, and
// replacing the file would desync it from the manager that put it there.
var managedPaths = []string{
	"/nix/store",
	"/opt/homebrew/Cellar",
	"/usr/local/Cellar",
}

// selfUpdate replaces the running binary with the newest release. With checkOnly it
// stops after reporting what it found, without downloading anything.
func selfUpdate(ctx context.Context, current string, checkOnly bool) error {
	if !isReleaseVersion(current) {
		return fmt.Errorf("this is a %q build rather than a release, so there is no version to compare against; "+
			"download a binary from https://github.com/%s/releases to use self-update", current, updateRepo)
	}

	// The library reads GITHUB_TOKEN itself, but passing it explicitly keeps the
	// difference between an authenticated and an anonymous run visible here. Without
	// a token GitHub allows 60 requests an hour per IP; an update costs about three.
	source, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{APIToken: os.Getenv("GITHUB_TOKEN")})
	if err != nil {
		return fmt.Errorf("reaching GitHub: %w", err)
	}

	updater, err := selfupdate.NewUpdater(selfupdate.Config{
		Source: source,
		// The release ships a checksums.txt written by sha256sum, which is the
		// format this validator parses. The asset is verified before anything is
		// written to disk.
		Validator: &selfupdate.ChecksumValidator{UniqueFilename: "checksums.txt"},
	})
	if err != nil {
		return fmt.Errorf("preparing the updater: %w", err)
	}

	latest, found, err := updater.DetectLatest(ctx, selfupdate.ParseSlug(updateRepo))
	if err != nil {
		return fmt.Errorf("looking for a newer release: %w", err)
	}
	if !found {
		// A 404 from the releases endpoint is swallowed and surfaces here, so a
		// repository that was renamed or made private looks the same as a platform
		// with no asset. Name both possibilities.
		return fmt.Errorf("no release with a %s/%s binary found in %s", runtime.GOOS, runtime.GOARCH, updateRepo)
	}

	if latest.LessOrEqual(current) {
		fmt.Printf("%s is already the latest version\n", current)
		return nil
	}

	fmt.Printf("%s is available (currently %s)\n", latest.Version(), current)
	if checkOnly {
		return nil
	}

	// Resolves symlinks, so this is the file that actually gets replaced.
	executable, err := selfupdate.ExecutablePath()
	if err != nil {
		return fmt.Errorf("locating the running binary: %w", err)
	}

	mode, err := replaceableMode(executable)
	if err != nil {
		return err
	}

	if err := updater.UpdateTo(ctx, latest, executable); err != nil {
		if rollbackErr := update.RollbackError(err); rollbackErr != nil {
			return fmt.Errorf("the update failed and could not be rolled back, so %s may be missing; "+
				"reinstall from https://github.com/%s/releases: %w (rollback: %v)",
				executable, updateRepo, err, rollbackErr)
		}
		if errors.Is(err, selfupdate.ErrChecksumValidationFailed) {
			return fmt.Errorf("the downloaded %s does not match its checksum, nothing was installed: %w", latest.AssetName, err)
		}
		return fmt.Errorf("installing %s: %w", latest.Version(), err)
	}

	// The updater installs with a fixed 0755 and offers no way to pass the mode
	// through, so anything more restrictive has to be put back afterwards.
	if err := os.Chmod(executable, mode); err != nil {
		return fmt.Errorf("restoring permissions on %s: %w", executable, err)
	}

	fmt.Printf("updated to %s\n", latest.Version())

	return nil
}

// replaceableMode reports the file mode to restore after an update, and fails early
// when the binary cannot be replaced at all. The updater writes into the directory
// holding the executable and has no pre-flight check of its own, so this runs before
// anything is downloaded.
func replaceableMode(executable string) (os.FileMode, error) {
	for _, managed := range managedPaths {
		if strings.HasPrefix(executable, managed+string(os.PathSeparator)) {
			return 0, fmt.Errorf("%s is installed by a package manager, which owns that path; update it there instead", executable)
		}
	}

	info, err := os.Stat(executable)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", executable, err)
	}

	directory := filepath.Dir(executable)
	probe, err := os.CreateTemp(directory, ".local-argocd-renderer-update-*")
	if err != nil {
		return 0, fmt.Errorf("%s is not writable, so the binary cannot be replaced; "+
			"re-run with sudo or install somewhere you own: %w", directory, err)
	}
	probe.Close()
	os.Remove(probe.Name())

	return info.Mode().Perm(), nil
}
