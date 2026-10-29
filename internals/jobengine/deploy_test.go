package jobengine

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestSanitizeRepoURL(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		github  bool
		wantErr bool
	}{
		{
			name:   "https credentials stripped",
			in:     "https://user:secret@github.com/owner/repo.git",
			want:   "https://github.com/owner/repo.git",
			github: true,
		},
		{
			name:   "github ssh converted to https",
			in:     "git@github.com:owner/repo.git",
			want:   "https://github.com/owner/repo.git",
			github: true,
		},
		{
			name:   "non github ssh kept",
			in:     "git@gitlab.com:owner/repo.git",
			want:   "git@gitlab.com:owner/repo.git",
			github: false,
		},
		{
			name:    "empty rejected",
			in:      "",
			wantErr: true,
		},
		{
			name:    "invalid rejected",
			in:      "not a url",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, github, err := sanitizeRepoURL(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want || github != tt.github {
				t.Errorf("got %q github=%v, want %q github=%v", got, github, tt.want, tt.github)
			}
		})
	}
}

func TestGitAuthEnvDoesNotExposeRawToken(t *testing.T) {
	const token = "ghs_exampletokenvalue1234567890"
	env := gitAuthEnv(token)
	if len(env) != 3 {
		t.Fatalf("auth env length = %d, want 3", len(env))
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, token) {
		t.Error("raw token must not appear in git environment")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	if !strings.Contains(joined, encoded) {
		t.Error("base64 credential missing from git environment")
	}
	if !strings.Contains(joined, "http.extraheader") {
		t.Error("http.extraheader config key missing")
	}
}

func TestRedactingWriterHidesSecrets(t *testing.T) {
	const token = "ghs_exampletokenvalue1234567890"
	var buf bytes.Buffer
	w := newRedactingWriter(&buf, token)

	_, _ = w.Write([]byte("fatal: unable to access 'https://x-access-token:" + token + "@github.com/o/r.git/'\n"))
	_, _ = w.Write([]byte("Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)) + "\n"))
	_, _ = w.Write([]byte("plain " + token + " token\n"))

	out := buf.String()
	if strings.Contains(out, token) {
		t.Fatalf("token leaked in output: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected redaction marker in output: %s", out)
	}
}

func TestRedactingWriterKeepsOrdinaryOutput(t *testing.T) {
	var buf bytes.Buffer
	w := newRedactingWriter(&buf, "")
	_, _ = w.Write([]byte("Cloning into 'repo'...\n"))
	if got := buf.String(); got != "Cloning into 'repo'...\n" {
		t.Errorf("ordinary output changed: %q", got)
	}
}
