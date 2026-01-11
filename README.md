# doorman

A CLI tool to manage SSH access using GitHub usernames. Fetches public SSH keys from GitHub and adds/removes them from your `~/.ssh/authorized_keys` file.

## Installation

```bash
go build -o doorman doorman.go
```

Or run directly:

```bash
go run doorman.go <action> <username>
```

## Usage

### Add SSH access for a GitHub user

```bash
doorman add <github-username>
```

This fetches the user's public keys from `https://github.com/<username>.keys` and appends them to `~/.ssh/authorized_keys` with the username as a comment for easy identification.

### Remove SSH access for a GitHub user

```bash
doorman remove <github-username>
```

This removes all keys associated with the specified GitHub username from your `authorized_keys` file.

## How it works

1. Fetches public SSH keys from GitHub's public endpoint
2. Appends the GitHub username to each key as a comment
3. Writes to `~/.ssh/authorized_keys` (creates the file/directory if needed)
4. For removal, filters out lines ending with the exact username

## Notes

- The tool prompts for confirmation before making changes
- Keys are tagged with the GitHub username for easy management
- The `.ssh` directory is created with `0700` permissions if it doesn't exist
- The `authorized_keys` file is created with `0600` permissions if it doesn't exist
