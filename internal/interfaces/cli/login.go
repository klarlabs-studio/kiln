package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.klarlabs.de/kiln/internal/infrastructure/credstore"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/github"
)

// tokenURL pre-fills GitHub's fine-grained token form with the permissions
// kiln needs, so nobody has to read a table of scopes and guess.
const tokenURL = "https://github.com/settings/personal-access-tokens/new" +
	"?name=kiln&description=Lets%20a%20kiln%20box%20post%20check%20results%20and%20open%20remediation%20pull%20requests"

// runLogin stores a GitHub token where the operating system keeps secrets.
//
// This command exists because the honest answer to "how do I run a box" used
// to include a paragraph about putting a token in a cron line. That is the
// step where people either give up or do something they should not, and no
// amount of documentation fixes it — the fix is for the tool to hold the
// credential properly and for the schedule to inherit it.
func runLogin(ctx context.Context, args []string, io IO) error {
	fs := newFlagSet("login", io)
	withToken := fs.Bool("with-token", false, "read the token from stdin instead of prompting")
	status := fs.Bool("status", false, "say whether a token is stored, and what it can do")
	logout := fs.Bool("logout", false, "remove the stored token")
	if err := fs.Parse(args); err != nil {
		return wrapExit(ExitUsage, err)
	}

	store := credstore.New(execx.NewSystem())

	switch {
	case *logout:
		if err := store.Delete(ctx); err != nil {
			return wrapExit(ExitError, err)
		}
		io.print("token removed\n")
		return nil
	case *status:
		return loginStatus(ctx, store, io)
	}

	token, err := readToken(io, *withToken)
	if err != nil {
		return err
	}

	// Checked before it is stored. A token that cannot read the repository is
	// a token somebody will spend an afternoon on, and the failure would
	// otherwise appear on the next scheduled tick, in a log nobody is reading.
	login, err := github.WhoAmI(ctx, token)
	if err != nil {
		return failWith(ExitConfig, "that token did not work: %v", err)
	}

	kind, err := store.Set(ctx, token)
	if err != nil {
		return wrapExit(ExitError, err)
	}

	io.print(fmt.Sprintf("stored for %s in the %s\n", login, kind))
	if kind == credstore.KindFile {
		// Said plainly rather than buried. The fallback is a real fallback,
		// not an equivalent option.
		io.print("  note: no keychain on this machine, so the token is a 0600 file. " +
			"Anything running as you can read it.\n")
	}
	io.print("\nNext: kiln box install   (a schedule that ticks this repository)\n")
	return nil
}

func loginStatus(ctx context.Context, store *credstore.Store, io IO) error {
	token, kind, err := store.Get(ctx)
	if errors.Is(err, credstore.ErrNotFound) {
		io.print("no token stored\n\nRun: kiln login\n")
		return failWith(ExitConfig, "not logged in")
	}
	if err != nil {
		return wrapExit(ExitError, err)
	}

	login, err := github.WhoAmI(ctx, token)
	if err != nil {
		io.print(fmt.Sprintf("a token is stored in the %s, but GitHub rejected it: %v\n", kind, err))
		return failWith(ExitConfig, "the stored token no longer works")
	}
	io.print(fmt.Sprintf("logged in as %s, from the %s\n", login, kind))
	return nil
}

// readToken prompts, or reads stdin when piped.
func readToken(io IO, fromStdin bool) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return "", failWith(ExitUsage, "no token on stdin")
		}
		return strings.TrimSpace(line), nil
	}

	io.print("kiln needs a GitHub token to post check results and, if a task opens\n" +
		"pull requests, to push a branch.\n\n")
	io.print("Create one here — the permissions are pre-filled:\n  " + tokenURL + "\n\n")
	io.print("  Repository access:  only the repositories kiln will watch\n")
	io.print("  Commit statuses:    read and write\n")
	io.print("  Contents:           read   (write, if a task opens pull requests)\n")
	io.print("  Pull requests:      read and write, for the same reason\n\n")
	io.print("Paste it here (it will not be echoed to the terminal by your shell): ")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", failWith(ExitUsage, "no token given")
	}
	io.print("\n")
	return strings.TrimSpace(line), nil
}
