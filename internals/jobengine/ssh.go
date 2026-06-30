package jobengine

import (
	"fmt"
	"os"
	"strings"
)

// addSSHKey appends a public key to root's authorized_keys, avoiding duplicates.
func addSSHKey(publicKey string) error {
	if publicKey == "" {
		return fmt.Errorf("public key is required")
	}
	sshDir := "/root/.ssh"
	os.MkdirAll(sshDir, 0700)

	authorizedKeysPath := sshDir + "/authorized_keys"

	// Read existing keys to avoid duplicates
	if data, err := os.ReadFile(authorizedKeysPath); err == nil {
		if strings.Contains(string(data), publicKey) {
			// Key already exists — no-op
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
