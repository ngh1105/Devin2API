package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ngh1105/Devin2API/internal/config"
	"github.com/ngh1105/Devin2API/internal/devin"
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
			fmt.Println("  login    CLI OAuth login flow")
			fmt.Println("         (default)   Start the web panel server")
			os.Exit(0)
		case "login":
			cfg := config.Load()
			if err := runLogin(context.Background(), os.Stdout, cfg); err != nil {
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

// runLogin runs the CLI OAuth flow: start a local callback server, print the
// login URL to stdout, wait for authentication, exchange code for token, fetch
// profile, best-effort usage, save account, and print filepath.
// For tests, pass notifyPort non-nil to receive the bound callback port.
func runLogin(ctx context.Context, stdout io.Writer, cfg config.Config, notifyPort ...func(int)) error {
	appBase := ""
	if cfg.BaseURL != "" {
		appBase = cfg.BaseURL
	}

	s, err := devin.StartLogin(appBase)
	if err != nil {
		return fmt.Errorf("start login: %w", err)
	}

	if len(notifyPort) > 0 && notifyPort[0] != nil {
		notifyPort[0](s.Port)
	}
	defer s.Stop()

	fmt.Fprintln(stdout, s.LoginURL)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	result, err := s.Wait(ctx, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("wait: %w", err)
	}

	apiBase := ""
	if cfg.BaseURL != "" {
		apiBase = cfg.BaseURL
	}
	rawToken, err := devin.ExchangeCode(ctx, apiBase, result.Code, s.Verifier)
	if err != nil {
		return fmt.Errorf("exchange: %w", err)
	}

	formatted := devin.FormatSessionToken(rawToken)

	p, err := devin.FetchSelfProfile(ctx, "", formatted)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: fetch profile: %v\n", err)
	}

	var usage *store.Usage
	if p.UserID != "" {
		u, ue := devin.FetchUserStatus(ctx, "", formatted)
		if ue != nil {
			fmt.Fprintf(os.Stderr, "warning: fetch usage: %v\n", ue)
		} else {
			usage = u
		}
	}

	accDir := cfg.DataDir + "/accounts"
	accounts, err := store.NewAccountStore(accDir)
	if err != nil {
		return fmt.Errorf("account store: %w", err)
	}

	acct := store.AccountFromLogin(p.UserName, p.UserID, p.OrgID, formatted, p.Email, usage)
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

	srv := server.New(cfg, accounts, tokens)
	fmt.Printf("Serving on %s, data at %s\n", cfg.Listen, cfg.DataDir)
	if err := srv.Start(cfg.Listen); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
