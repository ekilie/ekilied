package jobengine

import (
	"fmt"
	"os"
	"path/filepath"
)

// resolveEnvPath returns the full path to the .env file for a site. env_path
// is resolved relative to the site's checkout directory and may not be
// absolute or escape that directory.
func resolveEnvPath(siteName string, params map[string]any) (string, error) {
	repoDir, err := siteRepoPath(siteName)
	if err != nil {
		return "", err
	}

	envPath, _ := params["env_path"].(string)
	if envPath == "" {
		envPath = "."
	}
	if filepath.IsAbs(envPath) {
		return "", fmt.Errorf("env_path must be relative to the repository: %q", envPath)
	}

	envDir := repoDir
	if envPath != "." {
		envDir = filepath.Join(repoDir, envPath)
	}
	if !isContained(repoDir, envDir) {
		return "", fmt.Errorf("env_path %q escapes the repository", envPath)
	}
	return filepath.Join(envDir, ".env"), nil
}

// readEnvFile reads the .env file for a site and returns its content.
func readEnvFile(siteName string, params map[string]any) (string, error) {
	envFilePath, err := resolveEnvPath(siteName, params)
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(envFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read .env: %w", err)
	}
	return string(data), nil
}

// writeEnvContent writes raw .env content to the site's .env file.
func writeEnvContent(siteName string, params map[string]any) error {
	content, ok := params["content"].(string)
	if !ok {
		return nil
	}

	envFilePath, err := resolveEnvPath(siteName, params)
	if err != nil {
		return err
	}

	parentDir := filepath.Dir(envFilePath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("mkdir env dir: %w", err)
	}

	return os.WriteFile(envFilePath, []byte(content), 0644)
}
