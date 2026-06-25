package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var (
	version = "0.1.0"
	commit  = "dev"
)

func SetVersion(v, c string) {
	version = v
	commit = c
}

type GitHubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// CheckForUpdate checks GitHub Releases for a newer version.
// Returns the latest release if an update is available, nil if up to date.
func CheckForUpdate(repo, currentVersion string) (*GitHubRelease, bool, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ekilied/"+currentVersion)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("github api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, false, nil
	}
	if resp.StatusCode != 200 {
		return nil, false, fmt.Errorf("github api: HTTP %d", resp.StatusCode)
	}

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, false, fmt.Errorf("decode: %w", err)
	}
	if release.TagName == "" {
		return nil, false, nil
	}

	latest := strings.TrimPrefix(release.TagName, "v")
	current := strings.TrimPrefix(currentVersion, "v")
	if latest == current {
		return nil, false, nil
	}

	return &release, true, nil
}

// SelfUpdate downloads the latest binary, verifies checksum, and replaces itself.
// The running process calls systemctl restart and exits — systemd starts the new binary.
func SelfUpdate(repo string, release *GitHubRelease) error {
	platform := fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH)

	// Find the archive URL for our platform
	var archiveURL, archiveName string
	for _, asset := range release.Assets {
		if strings.Contains(asset.Name, platform) && strings.HasSuffix(asset.Name, ".tar.gz") {
			archiveURL = asset.BrowserDownloadURL
			archiveName = asset.Name
			break
		}
	}
	if archiveURL == "" {
		return fmt.Errorf("no binary found for %s in release %s", platform, release.TagName)
	}

	// Find checksums.txt
	var checksumURL string
	for _, asset := range release.Assets {
		if asset.Name == "checksums.txt" {
			checksumURL = asset.BrowserDownloadURL
			break
		}
	}

	tmpDir, err := os.MkdirTemp("", "ekilied-update-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	archivePath := filepath.Join(tmpDir, archiveName)
	log.Printf("[update] downloading %s ...", archiveURL)

	if err := downloadFile(archivePath, archiveURL); err != nil {
		return fmt.Errorf("download archive: %w", err)
	}

	// Verify checksum
	if checksumURL != "" {
		log.Printf("[update] verifying checksum...")
		checksumPath := filepath.Join(tmpDir, "checksums.txt")
		if err := downloadFile(checksumPath, checksumURL); err == nil {
			if err := verifyChecksum(archivePath, checksumPath, archiveName); err != nil {
				return fmt.Errorf("checksum: %w", err)
			}
			log.Printf("[update] checksum verified")
		} else {
			log.Printf("[update] warning: could not download checksums.txt: %v", err)
		}
	}

	// Extract the binary
	extractDir := filepath.Join(tmpDir, "out")
	os.MkdirAll(extractDir, 0755)

	extractCmd := exec.Command("tar", "-xzf", archivePath, "-C", extractDir)
	if out, err := extractCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extract: %s: %w", string(out), err)
	}

	binaryPath := filepath.Join(extractDir, "ekilied")
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		return fmt.Errorf("extracted binary not found at %s", binaryPath)
	}

	// Verify the new binary runs
	verifyOut, err := exec.Command(binaryPath, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("new binary verification failed: %s: %w", string(verifyOut), err)
	}
	log.Printf("[update] new binary verified: %s", strings.TrimSpace(string(verifyOut)))

	// Replace the running binary
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}

	backupPath := selfPath + ".bak"
	os.Remove(backupPath)

	if err := os.Rename(selfPath, backupPath); err != nil {
		return fmt.Errorf("backup current binary: %w", err)
	}

	if err := os.Rename(binaryPath, selfPath); err != nil {
		os.Rename(backupPath, selfPath)
		return fmt.Errorf("replace binary: %w", err)
	}

	os.Chmod(selfPath, 0755)
	log.Printf("[update] updated: %s → %s", release.TagName, selfPath)

	// Restart via systemd — this process will be replaced
	log.Printf("[update] restarting service...")
	exec.Command("systemctl", "restart", "ekilied").Start()

	return nil
}

func downloadFile(dst, url string) error {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func verifyChecksum(archivePath, checksumPath, archiveName string) error {
	hash := sha256.New()
	data, err := os.ReadFile(archivePath)
	if err != nil {
		return err
	}
	hash.Write(data)
	actual := hex.EncodeToString(hash.Sum(nil))

	checksumData, err := os.ReadFile(checksumPath)
	if err != nil {
		return err
	}

	for _, line := range strings.Split(string(checksumData), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, archiveName) {
			parts := strings.Fields(line)
			if len(parts) > 0 && parts[0] == actual {
				return nil
			}
			return fmt.Errorf("expected %s, got %s for %s", parts[0], actual, archiveName)
		}
	}
	return fmt.Errorf("checksum for %s not found", archiveName)
}
