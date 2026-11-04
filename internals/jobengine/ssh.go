package jobengine

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var (
	passwdPath = "/etc/passwd"
	groupPath  = "/etc/group"
)

// sshAccount is a local account that receives managed SSH keys.
type sshAccount struct {
	Name  string
	UID   int
	GID   int
	Home  string
	Shell string
}

// sshKeyAccounts returns the accounts that should receive an assigned key:
// root plus the members of the sudo and wheel groups. The agent runs as root,
// so writing only to root's authorized_keys left users who log in as
// ubuntu/forge locked out of their own servers.
//
// When neither admin group has members (minimal or unusual images), fall back
// to regular login accounts: UID 1000 and above with a real shell.
func sshKeyAccounts() []sshAccount {
	accounts := readPasswd()
	byName := make(map[string]sshAccount, len(accounts))
	for _, account := range accounts {
		byName[account.Name] = account
	}

	selected := make(map[string]sshAccount)
	if root, ok := byName["root"]; ok {
		selected[root.Name] = root
	} else {
		selected["root"] = sshAccount{Name: "root", Home: "/root", Shell: "/bin/bash"}
	}

	for _, group := range []string{"sudo", "wheel"} {
		for _, name := range groupMembers(group) {
			account, ok := byName[name]
			if !ok || account.Home == "" || !dirExists(account.Home) {
				continue
			}
			selected[name] = account
		}
	}

	if len(selected) == 1 {
		for _, account := range accounts {
			if account.UID < 1000 || account.UID == 65534 {
				continue
			}
			if account.Home == "" || !dirExists(account.Home) || !isLoginShell(account.Shell) {
				continue
			}
			selected[account.Name] = account
		}
	}

	result := make([]sshAccount, 0, len(selected))
	for _, account := range selected {
		result = append(result, account)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].UID < result[j].UID
	})
	return result
}

func accountNames(accounts []sshAccount) string {
	names := make([]string, 0, len(accounts))
	for _, account := range accounts {
		names = append(names, account.Name)
	}
	return strings.Join(names, ", ")
}

// addSSHKey appends a public key to every managed account.
func addSSHKey(publicKey string) error {
	return addSSHKeyForAccounts(publicKey, sshKeyAccounts())
}

func addSSHKeyForAccounts(publicKey string, accounts []sshAccount) error {
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return fmt.Errorf("public key is required")
	}
	if len(accounts) == 0 {
		return fmt.Errorf("no SSH key target accounts found")
	}

	var failures []string
	for _, account := range accounts {
		if err := addKeyForAccount(account, publicKey); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", account.Name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("add SSH key: %s", strings.Join(failures, "; "))
	}

	log.Printf("ssh key added for: %s", accountNames(accounts))
	return nil
}

func addKeyForAccount(account sshAccount, publicKey string) error {
	if account.Home == "" {
		return fmt.Errorf("no home directory")
	}

	sshDir := filepath.Join(account.Home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return fmt.Errorf("create %s: %w", sshDir, err)
	}
	// MkdirAll leaves the mode and owner of an existing directory untouched.
	if err := os.Chmod(sshDir, 0700); err != nil {
		return fmt.Errorf("chmod %s: %w", sshDir, err)
	}
	if err := os.Chown(sshDir, account.UID, account.GID); err != nil {
		return fmt.Errorf("chown %s: %w", sshDir, err)
	}

	path := filepath.Join(sshDir, "authorized_keys")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err == nil && keyExists(string(data), publicKey) {
		return nil
	}

	prefix := ""
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		prefix = "\n"
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	_, writeErr := f.WriteString(prefix + publicKey + "\n")
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("write %s: %w", path, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", path, closeErr)
	}

	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := os.Chown(path, account.UID, account.GID); err != nil {
		return fmt.Errorf("chown %s: %w", path, err)
	}
	return nil
}

// removeSSHKey removes a public key from every managed account.
func removeSSHKey(publicKey string) error {
	return removeSSHKeyForAccounts(publicKey, sshKeyAccounts())
}

func removeSSHKeyForAccounts(publicKey string, accounts []sshAccount) error {
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return fmt.Errorf("public key is required")
	}

	var failures []string
	for _, account := range accounts {
		if err := removeKeyForAccount(account, publicKey); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", account.Name, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("remove SSH key: %s", strings.Join(failures, "; "))
	}

	log.Printf("ssh key removed for: %s", accountNames(accounts))
	return nil
}

func removeKeyForAccount(account sshAccount, publicKey string) error {
	path := filepath.Join(account.Home, ".ssh", "authorized_keys")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines))
	removed := false
	for _, line := range lines {
		if strings.TrimSpace(line) == publicKey {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return nil
	}

	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func readPasswd() []sshAccount {
	data, err := os.ReadFile(passwdPath)
	if err != nil {
		return nil
	}

	var accounts []sshAccount
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			continue
		}
		uid, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		gid, err := strconv.Atoi(fields[3])
		if err != nil {
			continue
		}
		accounts = append(accounts, sshAccount{
			Name:  fields[0],
			UID:   uid,
			GID:   gid,
			Home:  fields[5],
			Shell: fields[6],
		})
	}
	return accounts
}

func groupMembers(group string) []string {
	data, err := os.ReadFile(groupPath)
	if err != nil {
		return nil
	}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 || fields[0] != group {
			continue
		}
		var members []string
		for _, name := range strings.Split(fields[3], ",") {
			if name = strings.TrimSpace(name); name != "" {
				members = append(members, name)
			}
		}
		return members
	}
	return nil
}

func isLoginShell(shell string) bool {
	switch shell {
	case "", "/bin/false", "/usr/bin/false",
		"/sbin/nologin", "/usr/sbin/nologin", "/usr/bin/nologin",
		"/bin/sync", "/sbin/shutdown", "/sbin/halt":
		return false
	}
	return true
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
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
