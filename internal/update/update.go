// Package update implements agentmesh's self-update path: ask GitHub for
// the latest release, compare it with the running binary's version, and
// (if newer) download the matching release asset, verify it against the
// release's checksums.txt, and atomically swap it in place of the
// currently-running executable.
//
// Designed to be safe to call both synchronously (from the `self-update`
// subcommand) and asynchronously (from a background goroutine in
// `serve`). Logs progress to stderr (one short line per phase) so the
// user can follow along in the harness's MCP transcript.
//
// stdlib-only; no third-party deps.
package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// githubAPI is the base URL for GitHub's REST API. Package-level so tests
// could override it if we ever want to; the public API still talks to the
// real GitHub.
const githubAPI = "https://api.github.com"

// httpTimeout caps every individual HTTP roundtrip. The metadata request
// needs ~10s by the spec; archive downloads can be larger so we use a
// separate client with a longer timeout for those.
const (
	metaTimeout     = 10 * time.Second
	downloadTimeout = 5 * time.Minute
)

// LatestRelease returns the tag of the latest published release on the
// given owner/repo. Uses GitHub's /releases/latest endpoint which only
// surfaces non-prerelease, non-draft releases - which is exactly the set
// we want to auto-update to.
func LatestRelease(ctx context.Context, repo string) (string, error) {
	if repo == "" {
		return "", errors.New("update: empty repo")
	}
	url := githubAPI + "/repos/" + repo + "/releases/latest"

	cctx, cancel := context.WithTimeout(ctx, metaTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("update: build request: %w", err)
	}
	// Identify ourselves; GitHub recommends a UA, and it's helpful in
	// their abuse logs if we ever cause noise.
	req.Header.Set("User-Agent", "agentmesh-self-update")
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: metaTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("update: fetch latest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("update: github returned %s", resp.Status)
	}

	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("update: decode latest: %w", err)
	}
	if payload.TagName == "" {
		return "", errors.New("update: empty tag_name in latest release")
	}
	return payload.TagName, nil
}

// IsNewer reports whether latestTag represents a newer release than
// currentVersion. Both inputs are tolerated with or without a leading
// "v". The comparison is a simple semver-by-numeric-parts: split on ".",
// parse each component as an int, compare lexicographically.
//
// Conservative on weirdness: if either side has a non-numeric suffix
// anywhere (e.g. "v0.5.0-rc1" or "0.5.0+build7"), we return false. We'd
// rather skip an update than auto-replace into a prerelease.
func IsNewer(currentVersion, latestTag string) bool {
	cur, ok := parseSemver(currentVersion)
	if !ok {
		return false
	}
	latest, ok := parseSemver(latestTag)
	if !ok {
		return false
	}
	// Pad to the same length so "0.5" vs "0.5.0" compares cleanly.
	for len(cur) < len(latest) {
		cur = append(cur, 0)
	}
	for len(latest) < len(cur) {
		latest = append(latest, 0)
	}
	for i := range latest {
		if latest[i] > cur[i] {
			return true
		}
		if latest[i] < cur[i] {
			return false
		}
	}
	return false
}

// parseSemver splits "v0.5.1" / "0.5.1" into [0 5 1]. Returns false if
// any part has non-numeric characters (this is what makes "v0.5.0-rc1"
// fail closed in IsNewer).
func parseSemver(s string) ([]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// SelfReplace downloads the release archive for the current OS/arch from
// the given tag, verifies its sha256 against the release's
// checksums.txt, extracts the agentmesh binary, and atomically replaces
// the binary at the path of the currently-running executable.
//
// Defensive: on any error after the original binary has been moved aside
// (Windows path), we attempt to put it back so the user never ends up
// with a missing executable.
func SelfReplace(ctx context.Context, repo, tag string) error {
	logf("checking for updates")

	if repo == "" {
		return errors.New("update: empty repo")
	}
	if tag == "" {
		return errors.New("update: empty tag")
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("update: locate self: %w", err)
	}
	// Resolve symlinks so we replace the actual file, not the link. A
	// /usr/local/bin/agentmesh symlink -> ~/.local/bin/agentmesh setup
	// should update the real file, not turn the symlink into a regular
	// file.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	exeDir := filepath.Dir(exe)

	verNoV := strings.TrimPrefix(tag, "v")
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	var archiveName string
	switch goos {
	case "windows":
		archiveName = fmt.Sprintf("agentmesh_%s_%s_%s.zip", verNoV, goos, goarch)
	default:
		archiveName = fmt.Sprintf("agentmesh_%s_%s_%s.tar.gz", verNoV, goos, goarch)
	}

	base := fmt.Sprintf("https://github.com/%s/releases/download/%s", repo, tag)
	archiveURL := base + "/" + archiveName
	sumsURL := base + "/checksums.txt"

	// --- download archive into a sibling temp file ----------------------
	// Sibling so the eventual rename is on the same filesystem (atomic).
	tmpArchive, err := os.CreateTemp(exeDir, ".agentmesh-dl-*")
	if err != nil {
		return fmt.Errorf("update: temp file: %w", err)
	}
	tmpArchivePath := tmpArchive.Name()
	// Best-effort cleanup of the archive temp; we always remove it.
	defer os.Remove(tmpArchivePath)

	written, err := downloadTo(ctx, archiveURL, tmpArchive)
	closeErr := tmpArchive.Close()
	if err != nil {
		return fmt.Errorf("update: download archive: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("update: close archive: %w", closeErr)
	}
	logf("downloaded %d MB", (written+512*1024)/(1024*1024))

	// --- fetch + parse checksums.txt, verify sha256 --------------------
	sum, err := fetchChecksum(ctx, sumsURL, archiveName)
	if err != nil {
		return fmt.Errorf("update: checksum lookup: %w", err)
	}
	got, err := sha256File(tmpArchivePath)
	if err != nil {
		return fmt.Errorf("update: hash archive: %w", err)
	}
	if !strings.EqualFold(got, sum) {
		return fmt.Errorf("update: sha256 mismatch (want %s, got %s)", sum, got)
	}
	logf("verified checksum")

	// --- extract the agentmesh binary into a sibling temp file ---------
	tmpBin, err := os.CreateTemp(exeDir, ".agentmesh-new-*")
	if err != nil {
		return fmt.Errorf("update: temp bin: %w", err)
	}
	tmpBinPath := tmpBin.Name()
	// If anything below fails before the rename, clean it up.
	keepTmpBin := false
	defer func() {
		if !keepTmpBin {
			os.Remove(tmpBinPath)
		}
	}()

	wantName := "agentmesh"
	if goos == "windows" {
		wantName = "agentmesh.exe"
	}

	if strings.HasSuffix(archiveName, ".zip") {
		err = extractFromZip(tmpArchivePath, wantName, tmpBin)
	} else {
		err = extractFromTarGz(tmpArchivePath, wantName, tmpBin)
	}
	closeErr = tmpBin.Close()
	if err != nil {
		return fmt.Errorf("update: extract: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("update: close new bin: %w", closeErr)
	}
	if err := os.Chmod(tmpBinPath, 0o755); err != nil {
		return fmt.Errorf("update: chmod new bin: %w", err)
	}

	// --- atomic swap ----------------------------------------------------
	// On macOS/Linux the kernel keeps the inode of the currently-running
	// binary alive after rename, so we can just clobber the path.
	// On Windows we can't replace a live exe directly; rename the live
	// one aside first, then put the new one in place, then best-effort
	// remove the .old. If the second rename fails, restore the .old.
	if goos == "windows" {
		oldPath := exe + ".old"
		// Pre-clean a stale .old from an earlier failed update so we
		// don't trip over it.
		_ = os.Remove(oldPath)
		if err := os.Rename(exe, oldPath); err != nil {
			return fmt.Errorf("update: rename current exe aside: %w", err)
		}
		if err := os.Rename(tmpBinPath, exe); err != nil {
			// Try to restore - we'd rather leave the user on the old
			// version than on no version at all.
			if rerr := os.Rename(oldPath, exe); rerr != nil {
				return fmt.Errorf("update: install failed (%v) AND restore failed (%v); old binary is at %s",
					err, rerr, oldPath)
			}
			return fmt.Errorf("update: install new exe: %w (restored previous version)", err)
		}
		keepTmpBin = true
		// Best-effort: Windows often holds a lock on the running .old
		// until the process exits. Don't fail the update on this.
		_ = os.Remove(oldPath)
	} else {
		if err := os.Rename(tmpBinPath, exe); err != nil {
			return fmt.Errorf("update: install new binary: %w", err)
		}
		keepTmpBin = true
	}

	logf("replaced binary at %s", exe)
	return nil
}

// downloadTo streams url into w. Returns bytes written.
func downloadTo(ctx context.Context, url string, w io.Writer) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "agentmesh-self-update")

	client := &http.Client{Timeout: downloadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("http %s", resp.Status)
	}
	return io.Copy(w, resp.Body)
}

// fetchChecksum downloads checksums.txt and returns the hex sha256 for
// archiveName. checksums.txt is the goreleaser format: one line per
// asset, "<hex>  <filename>".
func fetchChecksum(ctx context.Context, url, archiveName string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, metaTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "agentmesh-self-update")

	client := &http.Client{Timeout: metaTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// "<hex><space><space><filename>" - but tolerate any whitespace.
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[1] == archiveName || fields[1] == "*"+archiveName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum entry for %s", archiveName)
}

// sha256File returns the hex-encoded sha256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractFromTarGz scans the gzipped tar at archivePath for a regular
// file with basename == wantName and copies its contents into out.
func extractFromTarGz(archivePath, wantName string, out io.Writer) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		if filepath.Base(hdr.Name) != wantName {
			continue
		}
		if _, err := io.Copy(out, tr); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("archive does not contain %s", wantName)
}

// extractFromZip scans the zip at archivePath for a file with basename
// == wantName and copies its contents into out.
func extractFromZip(archivePath, wantName string, out io.Writer) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if filepath.Base(f.Name) != wantName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		_, err = io.Copy(out, rc)
		closeErr := rc.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		return nil
	}
	return fmt.Errorf("archive does not contain %s", wantName)
}

// logf prints a single self-update progress line to stderr. Quiet and
// uniform so the harness transcript stays scannable.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agentmesh self-update: "+format+"\n", args...)
}
