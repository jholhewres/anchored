package main

import (
	"fmt"
	"os"
	"os/exec"
)

func runIdentity(args []string) {
	fs := newFlagSet("identity")
	fs.Parse(args)

	subcmd := fs.Arg(0)
	if subcmd == "" {
		home, _ := os.UserHomeDir()
		path := home + "/.anchored/identity.md"
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Println("No identity file found. Use 'anchored identity edit' to create one.")
				return
			}
			fmt.Fprintf(os.Stderr, "error reading identity: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(string(data))
		return
	}

	if subcmd == "edit" {
		home, _ := os.UserHomeDir()
		path := home + "/.anchored/identity.md"

		if _, err := os.Stat(path); os.IsNotExist(err) {
			os.MkdirAll(home+"/.anchored", 0o755)
			os.WriteFile(path, []byte(""), 0o644)
		}

		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "nano"
		}

		cmd := exec.Command(editor, path)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "editor error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	fmt.Fprintf(os.Stderr, "Usage: anchored identity [edit]\n")
	os.Exit(1)
}
