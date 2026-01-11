package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// Test helpers for mocking
func setupTestEnv(t *testing.T) (tempDir string, cleanup func()) {
	t.Helper()

	tempDir, err := os.MkdirTemp("", "doorman-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	sshDir := filepath.Join(tempDir, ".ssh")
	if err := os.Mkdir(sshDir, 0700); err != nil {
		t.Fatalf("failed to create .ssh dir: %v", err)
	}

	// Save original globals
	origUserCurrent := userCurrent
	origStdin := stdin
	origStdout := stdout
	origHttpGet := httpGet
	origOsExit := osExit

	// Mock userCurrent to use temp directory
	userCurrent = func() (*user.User, error) {
		return &user.User{HomeDir: tempDir}, nil
	}

	cleanup = func() {
		os.RemoveAll(tempDir)
		userCurrent = origUserCurrent
		stdin = origStdin
		stdout = origStdout
		httpGet = origHttpGet
		osExit = origOsExit
		resetStdinReader()
	}

	return tempDir, cleanup
}

func mockStdin(input string) {
	stdin = strings.NewReader(input)
	resetStdinReader() // Reset the buffered reader when stdin changes
}

func mockStdout() *bytes.Buffer {
	buf := &bytes.Buffer{}
	stdout = buf
	return buf
}

func mockHttpGet(statusCode int, body string) {
	httpGet = func(url string) (*http.Response, error) {
		return &http.Response{
			StatusCode: statusCode,
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}

func mockHttpGetError(err error) {
	httpGet = func(url string) (*http.Response, error) {
		return nil, err
	}
}

// Tests for run()
func TestRunInvalidArgs(t *testing.T) {
	_, cleanup := setupTestEnv(t)
	defer cleanup()

	tests := []struct {
		name string
		args []string
	}{
		{"no args", []string{"doorman"}},
		{"one arg", []string{"doorman", "add"}},
		{"too many args", []string{"doorman", "add", "user", "extra"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockStdout()
			err := run(tt.args)
			if err == nil {
				t.Error("expected error for invalid args")
			}
		})
	}
}

func TestRunInvalidAction(t *testing.T) {
	_, cleanup := setupTestEnv(t)
	defer cleanup()

	mockStdout()
	mockHttpGet(http.StatusOK, "ssh-rsa AAAAB3...")

	err := run([]string{"doorman", "invalid", "user"})
	if err == nil {
		t.Error("expected error for invalid action")
	}
	if !strings.Contains(err.Error(), "invalid action") {
		t.Errorf("expected 'invalid action' error, got: %v", err)
	}
}

func TestRunFetchError(t *testing.T) {
	_, cleanup := setupTestEnv(t)
	defer cleanup()

	mockStdout()
	mockHttpGetError(errors.New("network error"))

	err := run([]string{"doorman", "add", "user"})
	if err == nil {
		t.Error("expected error for fetch failure")
	}
	if !strings.Contains(err.Error(), "error fetching keys") {
		t.Errorf("expected 'error fetching keys' error, got: %v", err)
	}
}

func TestRunEmptyKeys(t *testing.T) {
	_, cleanup := setupTestEnv(t)
	defer cleanup()

	mockStdout()
	mockHttpGet(http.StatusOK, "   \n\n  ")

	err := run([]string{"doorman", "add", "user"})
	if err == nil {
		t.Error("expected error for empty keys")
	}
	if !strings.Contains(err.Error(), "no public keys found") {
		t.Errorf("expected 'no public keys found' error, got: %v", err)
	}
}

func TestRunAddSuccess(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")

	out := mockStdout()
	mockHttpGet(http.StatusOK, "ssh-rsa AAAAB3...")
	mockStdin("yes\nyes\n") // First for create file, second for add keys

	err := run([]string{"doorman", "add", "testuser"})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if !strings.Contains(out.String(), "Keys added successfully") {
		t.Error("expected success message")
	}

	content, _ := os.ReadFile(authorizedKeysPath)
	if !strings.Contains(string(content), "testuser") {
		t.Error("keys should be added with username")
	}
}

func TestRunRemoveSuccess(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")
	os.WriteFile(authorizedKeysPath, []byte("ssh-rsa KEY1... testuser\nssh-rsa KEY2... other"), 0600)

	out := mockStdout()
	mockHttpGet(http.StatusOK, "ssh-rsa KEY1...")
	mockStdin("yes\n")

	err := run([]string{"doorman", "remove", "testuser"})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if !strings.Contains(out.String(), "Keys removed successfully") {
		t.Error("expected success message")
	}

	content, _ := os.ReadFile(authorizedKeysPath)
	if strings.Contains(string(content), "testuser") {
		t.Error("testuser keys should be removed")
	}
	if !strings.Contains(string(content), "other") {
		t.Error("other user keys should remain")
	}
}

// Tests for main()
func TestMain(t *testing.T) {
	_, cleanup := setupTestEnv(t)
	defer cleanup()

	mockStdout()

	exitCode := -1
	osExit = func(code int) {
		exitCode = code
	}

	// Test with invalid args (will trigger osExit)
	os.Args = []string{"doorman"}
	main()

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
}

// Tests for fetchKeys()
func TestFetchKeys(t *testing.T) {
	tests := []struct {
		name        string
		statusCode  int
		body        string
		expectError bool
		errContains string
	}{
		{"success", http.StatusOK, "ssh-rsa AAAAB3...", false, ""},
		{"not found", http.StatusNotFound, "Not Found", true, "HTTP 404"},
		{"server error", http.StatusInternalServerError, "Error", true, "HTTP 500"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.body))
			}))
			defer server.Close()

			// Reset httpGet to use real HTTP
			origHttpGet := httpGet
			httpGet = http.Get
			defer func() { httpGet = origHttpGet }()

			keys, err := fetchKeys(server.URL)

			if tt.expectError {
				if err == nil {
					t.Error("expected error")
				} else if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("expected error containing %q, got %v", tt.errContains, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if string(keys) != tt.body {
					t.Errorf("expected %q, got %q", tt.body, string(keys))
				}
			}
		})
	}
}

func TestFetchKeysNetworkError(t *testing.T) {
	origHttpGet := httpGet
	httpGet = http.Get
	defer func() { httpGet = origHttpGet }()

	_, err := fetchKeys("http://localhost:99999/invalid")
	if err == nil {
		t.Error("expected network error")
	}
}

// Tests for appendUsernameToKeys()
func TestAppendUsernameToKeys(t *testing.T) {
	tests := []struct {
		name     string
		keys     string
		username string
		expected string
	}{
		{"single key", "ssh-rsa AAAAB3...", "user", "ssh-rsa AAAAB3... user"},
		{"multiple keys", "ssh-rsa KEY1...\nssh-ed25519 KEY2...", "user", "ssh-rsa KEY1... user\nssh-ed25519 KEY2... user"},
		{"trailing newline", "ssh-rsa KEY...\n", "user", "ssh-rsa KEY... user"},
		{"empty lines", "ssh-rsa KEY1...\n\nssh-rsa KEY2...", "user", "ssh-rsa KEY1... user\nssh-rsa KEY2... user"},
		{"whitespace", "  ssh-rsa KEY...  ", "user", "ssh-rsa KEY... user"},
		{"empty input", "", "user", ""},
		{"only whitespace", "   \n   ", "user", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := appendUsernameToKeys([]byte(tt.keys), tt.username)
			if string(result) != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, string(result))
			}
		})
	}
}

// Tests for removeKeysByUsername()
func TestRemoveKeysByUsername(t *testing.T) {
	tests := []struct {
		name     string
		keys     string
		username string
		expected string
	}{
		{"remove single", "ssh-rsa KEY... user", "user", ""},
		{"remove multiple", "ssh-rsa KEY1... user\nssh-rsa KEY2... other", "user", "ssh-rsa KEY2... other"},
		{"no match", "ssh-rsa KEY... other", "user", "ssh-rsa KEY... other"},
		{"partial no match", "ssh-rsa KEY... user123", "user", "ssh-rsa KEY... user123"},
		{"prefix no match", "ssh-rsa KEY... myuser", "user", "ssh-rsa KEY... myuser"},
		{"empty", "", "user", ""},
		{"remove all", "ssh-rsa KEY1... user\nssh-rsa KEY2... user", "user", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := removeKeysByUsername([]byte(tt.keys), tt.username)
			if string(result) != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, string(result))
			}
		})
	}
}

// Tests for getAuthorizedKeysPath()
func TestGetAuthorizedKeysPath(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	path, err := getAuthorizedKeysPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := filepath.Join(tempDir, ".ssh", "authorized_keys")
	if path != expected {
		t.Errorf("expected %q, got %q", expected, path)
	}
}

func TestGetAuthorizedKeysPathError(t *testing.T) {
	origUserCurrent := userCurrent
	userCurrent = func() (*user.User, error) {
		return nil, errors.New("user error")
	}
	defer func() { userCurrent = origUserCurrent }()

	_, err := getAuthorizedKeysPath()
	if err == nil {
		t.Error("expected error")
	}
}

// Tests for ensureSSHDir()
func TestEnsureSSHDir(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	// Remove .ssh to test creation
	sshDir := filepath.Join(tempDir, ".ssh")
	os.RemoveAll(sshDir)

	err := ensureSSHDir()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	info, err := os.Stat(sshDir)
	if err != nil {
		t.Fatalf("ssh dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
	if info.Mode().Perm() != 0700 {
		t.Errorf("expected permissions 0700, got %o", info.Mode().Perm())
	}
}

func TestEnsureSSHDirExists(t *testing.T) {
	_, cleanup := setupTestEnv(t)
	defer cleanup()

	// .ssh already exists from setup
	err := ensureSSHDir()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureSSHDirUserError(t *testing.T) {
	origUserCurrent := userCurrent
	userCurrent = func() (*user.User, error) {
		return nil, errors.New("user error")
	}
	defer func() { userCurrent = origUserCurrent }()

	err := ensureSSHDir()
	if err == nil {
		t.Error("expected error")
	}
}

// Tests for promptConfirmation()
func TestPromptConfirmation(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"yes", "yes\n", true},
		{"YES", "YES\n", true},
		{"Yes", "Yes\n", true},
		{"yes with space", "  yes  \n", true},
		{"no", "no\n", false},
		{"empty", "\n", false},
		{"other", "maybe\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockStdin(tt.input)
			mockStdout()

			result, err := promptConfirmation("Test: ")
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

// Tests for confirmAndAddKeys()
func TestConfirmAndAddKeysNewFile(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")
	// File doesn't exist initially (not created by setup)

	mockStdout()
	mockStdin("yes\nyes\n") // First for create file, second for add keys

	err := confirmAndAddKeys([]byte("ssh-rsa AAAAB3..."), "testuser")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(authorizedKeysPath)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	if !strings.Contains(string(content), "testuser") {
		t.Error("keys should contain username")
	}
}

func TestConfirmAndAddKeysExistingFile(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")
	os.WriteFile(authorizedKeysPath, []byte("ssh-rsa EXISTING... existinguser"), 0600)

	mockStdout()
	mockStdin("yes\n")

	err := confirmAndAddKeys([]byte("ssh-rsa NEW..."), "newuser")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(authorizedKeysPath)
	if !strings.Contains(string(content), "existinguser") {
		t.Error("existing keys should be preserved")
	}
	if !strings.Contains(string(content), "newuser") {
		t.Error("new keys should be added")
	}
}

func TestConfirmAndAddKeysAbortCreate(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")

	out := mockStdout()
	mockStdin("no\n")

	err := confirmAndAddKeys([]byte("ssh-rsa AAAAB3..."), "testuser")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out.String(), "Operation aborted") {
		t.Error("expected abort message")
	}

	if _, err := os.Stat(authorizedKeysPath); !os.IsNotExist(err) {
		t.Error("file should not be created")
	}
}

func TestConfirmAndAddKeysAbortAdd(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")
	os.WriteFile(authorizedKeysPath, []byte("existing"), 0600)

	out := mockStdout()
	mockStdin("no\n")

	err := confirmAndAddKeys([]byte("ssh-rsa AAAAB3..."), "testuser")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out.String(), "Operation aborted") {
		t.Error("expected abort message")
	}

	content, _ := os.ReadFile(authorizedKeysPath)
	if strings.Contains(string(content), "testuser") {
		t.Error("keys should not be added")
	}
}

func TestConfirmAndAddKeysUserError(t *testing.T) {
	origUserCurrent := userCurrent
	userCurrent = func() (*user.User, error) {
		return nil, errors.New("user error")
	}
	defer func() { userCurrent = origUserCurrent }()

	mockStdout()
	err := confirmAndAddKeys([]byte("ssh-rsa AAAAB3..."), "testuser")
	if err == nil {
		t.Error("expected error")
	}
}

func TestConfirmAndAddKeysEmptyExistingFile(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")
	os.WriteFile(authorizedKeysPath, []byte(""), 0600)

	mockStdout()
	mockStdin("yes\n")

	err := confirmAndAddKeys([]byte("ssh-rsa AAAAB3..."), "testuser")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(authorizedKeysPath)
	if !strings.Contains(string(content), "testuser") {
		t.Error("keys should be added")
	}
}

// Tests for confirmAndRemoveKeys()
func TestConfirmAndRemoveKeysSuccess(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")
	os.WriteFile(authorizedKeysPath, []byte("ssh-rsa KEY1... user1\nssh-rsa KEY2... user2"), 0600)

	mockStdout()
	mockStdin("yes\n")

	err := confirmAndRemoveKeys([]byte("ssh-rsa KEY1..."), "user1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, _ := os.ReadFile(authorizedKeysPath)
	if strings.Contains(string(content), "user1") {
		t.Error("user1 keys should be removed")
	}
	if !strings.Contains(string(content), "user2") {
		t.Error("user2 keys should remain")
	}
}

func TestConfirmAndRemoveKeysNoFile(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	// Don't create authorized_keys
	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")
	os.Remove(authorizedKeysPath)

	out := mockStdout()

	err := confirmAndRemoveKeys([]byte("ssh-rsa KEY..."), "user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out.String(), "does not exist") {
		t.Error("expected 'does not exist' message")
	}
}

func TestConfirmAndRemoveKeysAbort(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")
	original := "ssh-rsa KEY... user"
	os.WriteFile(authorizedKeysPath, []byte(original), 0600)

	out := mockStdout()
	mockStdin("no\n")

	err := confirmAndRemoveKeys([]byte("ssh-rsa KEY..."), "user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out.String(), "Operation aborted") {
		t.Error("expected abort message")
	}

	content, _ := os.ReadFile(authorizedKeysPath)
	if string(content) != original {
		t.Error("file should not be modified")
	}
}

func TestConfirmAndRemoveKeysUserError(t *testing.T) {
	origUserCurrent := userCurrent
	userCurrent = func() (*user.User, error) {
		return nil, errors.New("user error")
	}
	defer func() { userCurrent = origUserCurrent }()

	mockStdout()
	err := confirmAndRemoveKeys([]byte("ssh-rsa KEY..."), "user")
	if err == nil {
		t.Error("expected error")
	}
}

// Integration tests
func TestIntegrationFullFlow(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")

	// Add user1
	mockStdout()
	mockHttpGet(http.StatusOK, "ssh-rsa KEY1...")
	mockStdin("yes\nyes\n")

	err := run([]string{"doorman", "add", "user1"})
	if err != nil {
		t.Fatalf("add user1 failed: %v", err)
	}

	// Add user2
	mockStdout()
	mockHttpGet(http.StatusOK, "ssh-rsa KEY2...")
	mockStdin("yes\n")

	err = run([]string{"doorman", "add", "user2"})
	if err != nil {
		t.Fatalf("add user2 failed: %v", err)
	}

	// Verify both users
	content, _ := os.ReadFile(authorizedKeysPath)
	if !strings.Contains(string(content), "user1") || !strings.Contains(string(content), "user2") {
		t.Error("both users should exist")
	}

	// Remove user1
	mockStdout()
	mockHttpGet(http.StatusOK, "ssh-rsa KEY1...")
	mockStdin("yes\n")

	err = run([]string{"doorman", "remove", "user1"})
	if err != nil {
		t.Fatalf("remove user1 failed: %v", err)
	}

	// Verify only user2 remains
	content, _ = os.ReadFile(authorizedKeysPath)
	if strings.Contains(string(content), "user1") {
		t.Error("user1 should be removed")
	}
	if !strings.Contains(string(content), "user2") {
		t.Error("user2 should remain")
	}
}

// Edge case tests
func TestConfirmAndAddKeysEnsureSSHDirError(t *testing.T) {
	// Save originals
	origUserCurrent := userCurrent
	origStdin := stdin
	origStdout := stdout

	defer func() {
		userCurrent = origUserCurrent
		stdin = origStdin
		stdout = origStdout
		resetStdinReader()
	}()

	// Use a path where we can't create directories
	userCurrent = func() (*user.User, error) {
		return &user.User{HomeDir: "/nonexistent/path/that/does/not/exist"}, nil
	}

	stdout = &bytes.Buffer{}
	stdin = strings.NewReader("yes\nyes\n")
	resetStdinReader()

	err := confirmAndAddKeys([]byte("ssh-rsa KEY..."), "user")
	if err == nil {
		t.Error("expected error when ensureSSHDir fails")
	}
}

// Test error paths for run()
func TestRunAddError(t *testing.T) {
	// Save originals
	origUserCurrent := userCurrent
	origStdin := stdin
	origStdout := stdout
	origHttpGet := httpGet

	defer func() {
		userCurrent = origUserCurrent
		stdin = origStdin
		stdout = origStdout
		httpGet = origHttpGet
		resetStdinReader()
	}()

	// Mock to fail during confirmAndAddKeys
	userCurrent = func() (*user.User, error) {
		return nil, errors.New("user lookup failed")
	}
	httpGet = func(url string) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ssh-rsa KEY...")),
		}, nil
	}
	stdout = &bytes.Buffer{}
	stdin = strings.NewReader("yes\n")
	resetStdinReader()

	err := run([]string{"doorman", "add", "user"})
	if err == nil {
		t.Error("expected error when confirmAndAddKeys fails")
	}
	if !strings.Contains(err.Error(), "error adding keys") {
		t.Errorf("expected 'error adding keys' error, got: %v", err)
	}
}

func TestRunRemoveError(t *testing.T) {
	// Save originals
	origUserCurrent := userCurrent
	origStdin := stdin
	origStdout := stdout
	origHttpGet := httpGet

	defer func() {
		userCurrent = origUserCurrent
		stdin = origStdin
		stdout = origStdout
		httpGet = origHttpGet
		resetStdinReader()
	}()

	// Mock to fail during confirmAndRemoveKeys
	userCurrent = func() (*user.User, error) {
		return nil, errors.New("user lookup failed")
	}
	httpGet = func(url string) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ssh-rsa KEY...")),
		}, nil
	}
	stdout = &bytes.Buffer{}

	err := run([]string{"doorman", "remove", "user"})
	if err == nil {
		t.Error("expected error when confirmAndRemoveKeys fails")
	}
	if !strings.Contains(err.Error(), "error removing keys") {
		t.Errorf("expected 'error removing keys' error, got: %v", err)
	}
}

// Test io.ReadAll error in fetchKeys
func TestFetchKeysReadError(t *testing.T) {
	origHttpGet := httpGet
	defer func() { httpGet = origHttpGet }()

	// Create a reader that fails after some reads
	httpGet = func(url string) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(&errorReader{}),
		}, nil
	}

	_, err := fetchKeys("http://example.com/test.keys")
	if err == nil {
		t.Error("expected read error")
	}
}

type errorReader struct{}

func (e *errorReader) Read(p []byte) (n int, err error) {
	return 0, errors.New("read error")
}

// Test promptConfirmation with read error
func TestPromptConfirmationReadError(t *testing.T) {
	origStdin := stdin
	origStdout := stdout

	defer func() {
		stdin = origStdin
		stdout = origStdout
		resetStdinReader()
	}()

	// Use a reader that returns an error
	stdin = &errorReader{}
	stdout = &bytes.Buffer{}
	resetStdinReader()

	_, err := promptConfirmation("Test: ")
	if err == nil {
		t.Error("expected read error")
	}
}

// Test confirmAndAddKeys with prompt error
func TestConfirmAndAddKeysPromptError(t *testing.T) {
	_, cleanup := setupTestEnv(t)
	defer cleanup()

	// File doesn't exist, so first prompt will be called
	// Use error reader for stdin
	stdin = &errorReader{}
	resetStdinReader()
	mockStdout()

	err := confirmAndAddKeys([]byte("ssh-rsa KEY..."), "user")
	if err == nil {
		t.Error("expected prompt error")
	}
}

// Test confirmAndAddKeys with second prompt error
func TestConfirmAndAddKeysSecondPromptError(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	// Create file so we skip first prompt
	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")
	_ = os.WriteFile(authorizedKeysPath, []byte("existing"), 0600)

	// First read succeeds (would show keys), then error
	stdin = &limitedErrorReader{remaining: 0}
	resetStdinReader()
	mockStdout()

	err := confirmAndAddKeys([]byte("ssh-rsa KEY..."), "user")
	if err == nil {
		t.Error("expected prompt error")
	}
}

type limitedErrorReader struct {
	remaining int
}

func (l *limitedErrorReader) Read(p []byte) (n int, err error) {
	if l.remaining > 0 {
		n = copy(p, "yes\n")
		l.remaining--
		return n, nil
	}
	return 0, errors.New("read error")
}

// Test confirmAndAddKeys with file stat error after write prompt
func TestConfirmAndAddKeysStatError(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	// Create file then make it unreadable
	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")
	os.WriteFile(authorizedKeysPath, []byte("existing"), 0600)

	mockStdout()
	mockStdin("yes\n")

	// Make file unwritable after confirmation
	os.Chmod(authorizedKeysPath, 0000)
	defer os.Chmod(authorizedKeysPath, 0600) // Restore for cleanup

	err := confirmAndAddKeys([]byte("ssh-rsa KEY..."), "user")
	if err == nil {
		t.Error("expected file write error")
	}
}

// Test confirmAndRemoveKeys with prompt error
func TestConfirmAndRemoveKeysPromptError(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")
	os.WriteFile(authorizedKeysPath, []byte("ssh-rsa KEY... user"), 0600)

	stdin = &errorReader{}
	resetStdinReader()
	mockStdout()

	err := confirmAndRemoveKeys([]byte("ssh-rsa KEY..."), "user")
	if err == nil {
		t.Error("expected prompt error")
	}
}

// Test confirmAndRemoveKeys with file read error
func TestConfirmAndRemoveKeysReadError(t *testing.T) {
	tempDir, cleanup := setupTestEnv(t)
	defer cleanup()

	authorizedKeysPath := filepath.Join(tempDir, ".ssh", "authorized_keys")
	os.WriteFile(authorizedKeysPath, []byte("ssh-rsa KEY... user"), 0600)

	mockStdout()
	mockStdin("yes\n")

	// Make file unreadable after confirmation check
	os.Chmod(authorizedKeysPath, 0000)
	defer os.Chmod(authorizedKeysPath, 0600)

	err := confirmAndRemoveKeys([]byte("ssh-rsa KEY..."), "user")
	if err == nil {
		t.Error("expected file read error")
	}
}

// Benchmarks
func BenchmarkAppendUsernameToKeys(b *testing.B) {
	keys := []byte("ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQC...\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...")
	for i := 0; i < b.N; i++ {
		appendUsernameToKeys(keys, "testuser")
	}
}

func BenchmarkRemoveKeysByUsername(b *testing.B) {
	keys := []byte("ssh-rsa KEY1... user1\nssh-rsa KEY2... user2\nssh-rsa KEY3... user1\nssh-rsa KEY4... user3")
	for i := 0; i < b.N; i++ {
		removeKeysByUsername(keys, "user1")
	}
}

// Test HTTP integration with real server
func TestFetchKeysWithServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/testuser.keys" {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "ssh-rsa KEY...")
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	origHttpGet := httpGet
	httpGet = http.Get
	defer func() { httpGet = origHttpGet }()

	// Success
	keys, err := fetchKeys(server.URL + "/testuser.keys")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(keys) != "ssh-rsa KEY..." {
		t.Errorf("unexpected keys: %q", keys)
	}

	// 404
	_, err = fetchKeys(server.URL + "/nonexistent.keys")
	if err == nil {
		t.Error("expected error for 404")
	}
}
