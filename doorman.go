package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// Dependencies for testing
var (
	osExit      = os.Exit
	httpGet     = http.Get
	userCurrent = user.Current
	stdin       io.Reader = os.Stdin
	stdout      io.Writer = os.Stdout
	stdinReader *bufio.Reader
)

func getStdinReader() *bufio.Reader {
	if stdinReader == nil {
		stdinReader = bufio.NewReader(stdin)
	}
	return stdinReader
}

func resetStdinReader() {
	stdinReader = nil
}

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(stdout, err)
		osExit(1)
	}
}

func run(args []string) error {
	if len(args) != 3 {
		fmt.Fprintln(stdout, "Usage: doorman add <username>")
		fmt.Fprintln(stdout, "       doorman remove <username>")
		return fmt.Errorf("invalid arguments")
	}

	action := args[1]
	username := args[2]
	keysURL := fmt.Sprintf("https://github.com/%s.keys", username)

	keys, err := fetchKeys(keysURL)
	if err != nil {
		return fmt.Errorf("error fetching keys: %w", err)
	}

	if len(strings.TrimSpace(string(keys))) == 0 {
		return fmt.Errorf("no public keys found for user '%s'", username)
	}

	switch action {
	case "add":
		if err := confirmAndAddKeys(keys, username); err != nil {
			return fmt.Errorf("error adding keys to authorized_keys: %w", err)
		}
		fmt.Fprintln(stdout, "Keys added successfully!")
	case "remove":
		if err := confirmAndRemoveKeys(keys, username); err != nil {
			return fmt.Errorf("error removing keys from authorized_keys: %w", err)
		}
		fmt.Fprintln(stdout, "Keys removed successfully!")
	default:
		return fmt.Errorf("invalid action '%s'. Please use 'add' or 'remove'", action)
	}

	return nil
}

func fetchKeys(url string) ([]byte, error) {
	response, err := httpGet(url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch keys: HTTP %d", response.StatusCode)
	}

	keys, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}

	return keys, nil
}

func getAuthorizedKeysPath() (string, error) {
	currentUser, err := userCurrent()
	if err != nil {
		return "", err
	}
	return filepath.Join(currentUser.HomeDir, ".ssh", "authorized_keys"), nil
}

func ensureSSHDir() error {
	currentUser, err := userCurrent()
	if err != nil {
		return err
	}
	sshDir := filepath.Join(currentUser.HomeDir, ".ssh")
	if _, err := os.Stat(sshDir); os.IsNotExist(err) {
		return os.Mkdir(sshDir, 0700)
	}
	return nil
}

func promptConfirmation(prompt string) (bool, error) {
	fmt.Fprint(stdout, prompt)
	reader := getStdinReader()
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	response := strings.ToLower(strings.TrimSpace(line))
	return response == "yes", nil
}

func confirmAndAddKeys(keys []byte, username string) error {
	keysWithUsername := appendUsernameToKeys(keys, username)

	authorizedKeysPath, err := getAuthorizedKeysPath()
	if err != nil {
		return err
	}

	fileExists := true
	if _, err := os.Stat(authorizedKeysPath); os.IsNotExist(err) {
		fileExists = false
		confirmed, err := promptConfirmation("The authorized_keys file does not exist. Do you want to create it? (yes/no): ")
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Fprintln(stdout, "Operation aborted.")
			return nil
		}
	}

	fmt.Fprintf(stdout, "Keys to be added:\n%s\n", string(keysWithUsername))
	confirmed, err := promptConfirmation("Do you want to add these keys? (yes/no): ")
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Fprintln(stdout, "Operation aborted.")
		return nil
	}

	if err := ensureSSHDir(); err != nil {
		return err
	}

	// BEHAVIOR: Append keys to existing file instead of overwriting
	// Using O_APPEND to preserve existing authorized keys
	if fileExists {
		file, err := os.OpenFile(authorizedKeysPath, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer file.Close()

		// Ensure we start on a new line
		stat, err := file.Stat()
		if err != nil {
			return err
		}
		if stat.Size() > 0 {
			if _, err := file.WriteString("\n"); err != nil {
				return err
			}
		}
		_, err = file.Write(keysWithUsername)
		return err
	}

	return os.WriteFile(authorizedKeysPath, keysWithUsername, 0600)
}

func confirmAndRemoveKeys(keys []byte, username string) error {
	keysWithUsername := appendUsernameToKeys(keys, username)

	authorizedKeysPath, err := getAuthorizedKeysPath()
	if err != nil {
		return err
	}

	if _, err := os.Stat(authorizedKeysPath); os.IsNotExist(err) {
		fmt.Fprintln(stdout, "The authorized_keys file does not exist.")
		return nil
	}

	fmt.Fprintf(stdout, "Keys to be removed:\n%s\n", string(keysWithUsername))

	confirmed, err := promptConfirmation("Do you want to remove these keys? (yes/no): ")
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Fprintln(stdout, "Operation aborted.")
		return nil
	}

	existingKeys, err := os.ReadFile(authorizedKeysPath)
	if err != nil {
		return err
	}

	newKeys := removeKeysByUsername(existingKeys, username)

	return os.WriteFile(authorizedKeysPath, newKeys, 0600)
}

func appendUsernameToKeys(keys []byte, username string) []byte {
	lines := strings.Split(strings.TrimSpace(string(keys)), "\n")

	var result []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) > 0 {
			result = append(result, line+" "+username)
		}
	}

	return []byte(strings.Join(result, "\n"))
}

func removeKeysByUsername(keys []byte, username string) []byte {
	lines := strings.Split(string(keys), "\n")

	// BEHAVIOR: Match exact username suffix to avoid partial matches
	// e.g., removing "bob" should not remove keys for "bobby"
	suffix := " " + username
	var newLines []string
	for _, line := range lines {
		if !strings.HasSuffix(line, suffix) {
			newLines = append(newLines, line)
		}
	}

	return []byte(strings.Join(newLines, "\n"))
}
