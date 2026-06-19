package utils

import (
	"os/exec"
	"strings"
)

func Run(cmd string, args ...string) (string, error) {
	out, err := exec.Command(cmd, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func RunBash(script string) (string, error) {
	out, err := exec.Command("/bin/bash", "-c", script).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
