package jobengine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func overrideAccountFiles(t *testing.T, passwd, group string) {
	t.Helper()
	oldPasswd, oldGroup := passwdPath, groupPath
	passwdPath, groupPath = passwd, group
	t.Cleanup(func() {
		passwdPath, groupPath = oldPasswd, oldGroup
	})
}

func TestSSHKeyAccountsIncludesSudoMembers(t *testing.T) {
	base := t.TempDir()
	rootHome := filepath.Join(base, "root")
	forgeHome := filepath.Join(base, "forge")
	ubuntuHome := filepath.Join(base, "ubuntu")
	gitHome := filepath.Join(base, "git")
	for _, home := range []string{rootHome, forgeHome, ubuntuHome, gitHome} {
		if err := os.MkdirAll(home, 0755); err != nil {
			t.Fatal(err)
		}
	}

	passwd := writeFixture(t, "passwd", strings.Join([]string{
		"root:x:0:0:root:" + rootHome + ":/bin/bash",
		"forge:x:1000:1000::" + forgeHome + ":/bin/bash",
		"ubuntu:x:1001:1001::" + ubuntuHome + ":/bin/bash",
		"git:x:1002:1002::" + gitHome + ":/bin/bash",
		"nobody:x:65534:65534::/nonexistent:/usr/sbin/nologin",
	}, "\n"))
	group := writeFixture(t, "group", strings.Join([]string{
		"root:x:0:",
		"sudo:x:27:forge",
		"forge:x:1000:",
	}, "\n"))
	overrideAccountFiles(t, passwd, group)

	accounts := sshKeyAccounts()
	names := accountNames(accounts)
	if names != "root, forge" {
		t.Fatalf("expected root and forge, got %q", names)
	}
}

func TestSSHKeyAccountsFallsBackToLoginUsers(t *testing.T) {
	base := t.TempDir()
	rootHome := filepath.Join(base, "root")
	ubuntuHome := filepath.Join(base, "ubuntu")
	daemonHome := filepath.Join(base, "daemon")
	for _, home := range []string{rootHome, ubuntuHome, daemonHome} {
		if err := os.MkdirAll(home, 0755); err != nil {
			t.Fatal(err)
		}
	}

	passwd := writeFixture(t, "passwd", strings.Join([]string{
		"root:x:0:0:root:" + rootHome + ":/bin/bash",
		"daemon:x:1:1:daemon:" + daemonHome + ":/usr/sbin/nologin",
		"ubuntu:x:1000:1000::" + ubuntuHome + ":/bin/bash",
	}, "\n"))
	group := writeFixture(t, "group", strings.Join([]string{
		"root:x:0:",
		"sudo:x:27:",
	}, "\n"))
	overrideAccountFiles(t, passwd, group)

	accounts := sshKeyAccounts()
	names := accountNames(accounts)
	if names != "root, ubuntu" {
		t.Fatalf("expected root and ubuntu, got %q", names)
	}
}

func TestAddAndRemoveSSHKeyForAccount(t *testing.T) {
	home := t.TempDir()
	account := sshAccount{
		Name: "forge",
		UID:  os.Getuid(),
		GID:  os.Getgid(),
		Home: home,
	}
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey user@example"

	if err := addKeyForAccount(account, key); err != nil {
		t.Fatalf("add key: %v", err)
	}
	// Adding the same key twice must not duplicate it.
	if err := addKeyForAccount(account, key); err != nil {
		t.Fatalf("add duplicate key: %v", err)
	}

	sshDir := filepath.Join(home, ".ssh")
	keyPath := filepath.Join(sshDir, "authorized_keys")

	info, err := os.Stat(sshDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0700 {
		t.Fatalf(".ssh mode = %o, want 700", got)
	}
	info, err = os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("authorized_keys mode = %o, want 600", got)
	}

	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), key); got != 1 {
		t.Fatalf("key appears %d times, want 1:\n%s", got, data)
	}

	if err := removeKeyForAccount(account, key); err != nil {
		t.Fatalf("remove key: %v", err)
	}
	data, err = os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), key) {
		t.Fatalf("key still present after removal:\n%s", data)
	}
}

func TestAddKeyWithoutTrailingNewline(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sshDir, "authorized_keys")
	if err := os.WriteFile(path, []byte("ssh-rsa EXISTING existing@example"), 0600); err != nil {
		t.Fatal(err)
	}

	account := sshAccount{Name: "forge", UID: os.Getuid(), GID: os.Getgid(), Home: home}
	key := "ssh-ed25519 AAAA NewKey new@example"
	if err := addKeyForAccount(account, key); err != nil {
		t.Fatalf("add key: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d:\n%s", len(lines), data)
	}
	if lines[1] != key {
		t.Fatalf("second line = %q, want %q", lines[1], key)
	}
}
