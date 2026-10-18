package main

import (
	"fmt"
	"os"

	"github.com/ngh1105/Devin2API/internal/config"
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
			fmt.Println("not implemented yet")
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
			os.Exit(1)
		}
	}

	cfg := config.Load()
	fmt.Printf("Serving on %s, data at %s\n", cfg.Listen, cfg.DataDir)
	fmt.Println("not implemented yet")
}
