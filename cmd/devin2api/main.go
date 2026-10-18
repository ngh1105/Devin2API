package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/ngh1105/Devin2API/internal/auth"
	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/server"
	"github.com/ngh1105/Devin2API/internal/store"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-h", "--help":
			fmt.Println("Usage: devin2api [COMMAND]")
			fmt.Println()
			fmt.Println("Commands:")
			fmt.Println("  login    OAuth login flow")
			fmt.Println("         (default)   Start the web panel server")
			os.Exit(0)
		case "login":
			cfg := config.Load()
			if err := runLogin(context.Background(), os.Stdout, os.Stdin, cfg); err != nil {
				fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
				os.Exit(1)
			}
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
			os.Exit(1)
		}
	}

	serve(config.Load())
}

// ---------------------------------------------------------------------------
// Login subcommand
// ---------------------------------------------------------------------------

// runLogin prompts for email/password on stdin, runs the full auth flow,
// saves the account to disk, and prints <filepath>.
func runLogin(ctx context.Context, stdout io.Writer, stdin io.Reader, cfg config.Config) error {
	scanner := bufio.NewScanner(stdin)

	// --- email ---------------------------------------------------------------
	fmt.Fprint(stdout, "Email: ")
	if !scanner.Scan() {
		return fmt.Errorf("login: no input from stdin")
	}
	email := scanner.Text()

	// --- password ------------------------------------------------------------
	var password string
	if t, ok := stdin.(*os.File); ok && term.IsTerminal(int(t.Fd())) {
		pw, err := term.ReadPassword(int(t.Fd()))
		if err != nil {
			return fmt.Errorf("password prompt: %w", err)
		}
		password = string(pw)
	} else {
		// Non-TTY: read a line (e.g. piped input in tests / CI).
		fmt.Fprintln(stdout, "") // echo nothing; move to next line
		if !scanner.Scan() {
			return fmt.Errorf("login: no password from stdin")
		}
		password = scanner.Text()
	}
	fmt.Fprintln(stdout, "") // newline after hidden input

	// --- auth flow -----------------------------------------------------------
	var c *auth.Client
	if cfg.BaseURL != "" {
		c = auth.NewClient(auth.WithBaseURL(cfg.BaseURL))
	} else {
		c = auth.NewClient()
	}
	tokens, err := c.Login(ctx, email, password)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	profile, err := c.UserInfo(ctx, tokens.AccessToken)
	if err != nil {
		return fmt.Errorf("userinfo: %w", err)
	}

	// --- persist -------------------------------------------------------------
	accDir := cfg.DataDir + "/accounts"
	accounts, err := store.NewAccountStore(accDir)
	if err != nil {
		return fmt.Errorf("account store: %w", err)
	}

	acct := store.AccountFromLogin(email, tokens, profile)
	if err := accounts.Save(acct); err != nil {
		return fmt.Errorf("save account: %w", err)
	}

	filepath := accDir + "/" + acct.ID + ".json"
	fmt.Fprintf(stdout, "saved %s\n", filepath)
	return nil
}

// ---------------------------------------------------------------------------
// Serve mode
// ---------------------------------------------------------------------------

func serve(cfg config.Config) {
	accounts, err := store.NewAccountStore(cfg.DataDir + "/accounts")
	if err != nil {
		fmt.Fprintf(os.Stderr, "init accounts: %v\n", err)
		os.Exit(1)
	}
	tokens, err := store.NewTokenStore(cfg.DataDir + "/tokens")
	if err != nil {
		fmt.Fprintf(os.Stderr, "init tokens: %v\n", err)
		os.Exit(1)
	}

	srv := server.New(cfg, accounts, tokens, nil)
	fmt.Printf("Serving on %s, data at %s\n", cfg.Listen, cfg.DataDir)
	if err := srv.Start(cfg.Listen); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
