package jobengine

import (
	"fmt"
	"os"
	"path/filepath"
)

// resolveEnvPath returns the full path to the .env file for a site, respecting env_path.
func resolveEnvPath(siteName string, params map[string]any) string {
	siteDir := fmt.Sprintf("/opt/ekilie/sites/%s", siteName)
	repoDir := siteDir + "/current"

	envPath, _ := params["env_path"].(string)
	if envPath == "" {
		envPath = "."
	}

	envDir := repoDir
	if envPath != "." && envPath != "" {
		envDir = filepath.Join(repoDir, envPath)
	}
	return filepath.Join(envDir, ".env")
}

// readEnvFile reads the .env file for a site and returns its content.
func readEnvFile(siteName string, params map[string]any) (string, error) {
	envFilePath := resolveEnvPath(siteName, params)

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

	envFilePath := resolveEnvPath(siteName, params)

	parentDir := filepath.Dir(envFilePath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("mkdir env dir: %w", err)
	}

	return os.WriteFile(envFilePath, []byte(content), 0644)
}
