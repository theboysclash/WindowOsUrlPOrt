// Command relay is the public end of the vmserver relay. Run it somewhere the
// viewer's network can reach (the repo's devcontainer starts it in a GitHub
// Codespace on port 8080) and start vmserver.exe with the link it prints.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/theboysclash/WindowOsUrlPOrt/internal/relay"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen    = flag.String("listen", ":8080", "address to listen on")
		keyFile   = flag.String("key-file", defaultKeyFile(), "where the shared key is kept between restarts")
		publicURL = flag.String("public-url", "", "public URL of this relay (detected automatically in a Codespace)")
		linkFile  = flag.String("link-file", "", "also write the vmserver link to this file")
	)
	flag.Parse()

	key, err := loadOrCreateKey(*keyFile)
	if err != nil {
		return err
	}
	pub := *publicURL
	if pub == "" {
		pub = codespaceURL(*listen)
	}
	if pub == "" {
		pub = "https://YOUR-RELAY-HOST"
	}
	pub = strings.TrimRight(pub, "/")
	link := pub + "#" + key

	fmt.Println()
	fmt.Println("  ==================== VM relay is running ====================")
	fmt.Println("  1. On the host PC, run vmserver.exe once with this link:")
	fmt.Println()
	fmt.Printf("     .\\vmserver.exe -relay \"%s\"\n", link)
	fmt.Println()
	fmt.Println("  2. On the Chromebook, open:")
	fmt.Println("     " + pub)
	fmt.Println()
	fmt.Println("  Keep the part after # secret: it lets a PC attach to this relay.")
	fmt.Println("  ==============================================================")
	fmt.Println()
	if *linkFile != "" {
		msg := fmt.Sprintf("On the host PC, run this in PowerShell (in the folder with vmserver.exe):\n\n.\\vmserver.exe -relay \"%s\"\n\n"+
			"Then open this on the Chromebook:\n\n%s\n\n"+
			"Only needed once: vmserver.exe remembers the link. Keep the part after # secret.\n"+
			"The Codespace goes to sleep after 30 idle minutes. Raise \"Default idle timeout\" to 240 at\n"+
			"https://github.com/settings/codespaces, and reopen the Codespace to wake it up.\n", link, pub)
		if err := os.WriteFile(*linkFile, []byte(msg), 0o600); err != nil {
			return err
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := &http.Server{
		Addr:              *listen,
		Handler:           relay.NewServer(key, log),
		ReadHeaderTimeout: 30 * time.Second,
	}
	return srv.ListenAndServe()
}

func defaultKeyFile() string {
	if st, err := os.Stat("/workspaces"); err == nil && st.IsDir() {
		return "/workspaces/.vmrelay-key"
	}
	return "vmrelay-key.txt"
}

func loadOrCreateKey(path string) (string, error) {
	if raw, err := os.ReadFile(path); err == nil {
		if k := strings.TrimSpace(string(raw)); len(k) >= 16 {
			return k, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	k := base64.RawURLEncoding.EncodeToString(b)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return k, os.WriteFile(path, []byte(k+"\n"), 0o600)
}

// codespaceURL builds the forwarded-port URL from the Codespaces environment.
func codespaceURL(listen string) string {
	name := os.Getenv("CODESPACE_NAME")
	if name == "" {
		return ""
	}
	domain := os.Getenv("GITHUB_CODESPACES_PORT_FORWARDING_DOMAIN")
	if domain == "" {
		domain = "app.github.dev"
	}
	port := listen[strings.LastIndex(listen, ":")+1:]
	return fmt.Sprintf("https://%s-%s.%s", name, port, domain)
}
