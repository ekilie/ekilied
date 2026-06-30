package jobengine

import (
	"fmt"
	"os"
	"strings"
)

const sshDir = "/root/.ssh"

var authorizedKeysPath = sshDir + "/authorized_keys"

// addSSHKey appends a public key to root's authorized_keys, avoiding duplicates.
func addSSHKey(publicKey string) error {
	if publicKey == "" {
		return fmt.Errorf("public key is required")
	}
	os.MkdirAll(sshDir, 0700)

	// Read existing keys to avoid duplicates
	if data, err := os.ReadFile(authorizedKeysPath); err == nil {
		if keyExists(string(data), publicKey) {
			return nil
		}
	}

	f, err := os.OpenFile(authorizedKeysPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open authorized_keys: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(publicKey + "\n"); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	return nil
}

// removeSSHKey removes a public key from root's authorized_keys.
func removeSSHKey(publicKey string) error {
	if publicKey == "" {
		return fmt.Errorf("public key is required")
	}

	data, err := os.ReadFile(authorizedKeysPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to remove
		}
		return fmt.Errorf("read authorized_keys: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	var kept []string
	for _, line := range lines {
		if strings.TrimSpace(line) != strings.TrimSpace(publicKey) {
			kept = append(kept, line)
		}
	}

	// If nothing changed, return early
	if len(kept) == len(lines) {
		return nil
	}

	return os.WriteFile(authorizedKeysPath, []byte(strings.Join(kept, "\n")), 0600)
}

// keyExists checks whether any line in data matches the given key (trimmed comparison).
func keyExists(data, key string) bool {
	for line := range strings.SplitSeq(data, "\n") {
		if strings.TrimSpace(line) == strings.TrimSpace(key) {
			return true
		}
	}
	return false
}
