package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/config"
	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/keyring"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// runSmoke is the M0 gate: connect to a JMAP account and dump the session
// and mailbox tree. Credentials resolve through the keyring package (FR-J2);
// they are never printed.
func runSmoke(args []string) error {
	fs := flag.NewFlagSet("smoke", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path (default: $XDG_CONFIG_HOME/jmap-tui/config.toml)")
	accountID := fs.String("account", "", "account id from config (default: default_account, else the only account)")
	url := fs.String("url", "", "server base URL (overrides config)")
	user := fs.String("user", "", "username (overrides config)")
	passwordFile := fs.String("password-file", "", "chmod-600 file containing the app password (overrides config)")
	timeout := fs.Duration("timeout", 30*time.Second, "network timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("smoke: unexpected arguments: %v", fs.Args())
	}

	if *configPath == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return err
		}
		*configPath = p
	}

	var acct *config.Account
	cfg, err := config.Load(*configPath)
	switch {
	case err == nil:
		id := *accountID
		if id == "" {
			id, acct, err = cfg.PrimaryAccount()
			if err != nil {
				return err
			}
			*accountID = id
		} else {
			var ok bool
			if acct, ok = cfg.Account(id); !ok {
				return fmt.Errorf("smoke: account %q not found in %s", id, *configPath)
			}
		}
	case errors.Is(err, os.ErrNotExist) && (*url != "" || *user != ""):
		// Flags-only mode; config is optional there.
		if *accountID == "" {
			*accountID = "default"
		}
	default:
		return fmt.Errorf("smoke: %w (or pass --url/--user for ad-hoc use)", err)
	}

	serverURL := first(*url, accountURL(acct))
	username := first(*user, accountUsername(acct))
	if serverURL == "" || username == "" {
		return errors.New("smoke: both --url and --user (or a configured account) are required")
	}

	secret, warnings, err := keyring.Password(*accountID, first(*passwordFile, accountPasswordFile(acct)), nil)
	if err != nil {
		return fmt.Errorf("smoke: %w", err)
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}

	client := jmapclient.New(jmapclient.Options{
		ServerURL:  serverURL,
		SessionURL: accountSessionURL(acct),
		Username:   username,
		Password:   secret,
		Timeout:    *timeout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("smoke: connect to %s: %w", serverURL, err)
	}
	info, err := client.SessionInfo()
	if err != nil {
		return fmt.Errorf("smoke: %w", err)
	}
	printSession(info)

	mailboxes, err := client.Mailboxes(ctx)
	if err != nil {
		return fmt.Errorf("smoke: mailboxes: %w", err)
	}
	printMailboxTree(mailboxes.Mailboxes)
	return nil
}

func printSession(info jmapclient.SessionInfo) {
	fmt.Println("Session")
	fmt.Printf("  state:         %s\n", info.State)
	fmt.Printf("  username:      %s\n", info.Username)
	fmt.Printf("  capabilities:  %s\n", strings.Join(info.Capabilities, ", "))
	fmt.Printf("  accounts:      %d\n", len(info.Accounts))
	for _, a := range info.Accounts {
		fmt.Printf("    %s  %s", a.ID, a.Name)
		if a.IsPersonal {
			fmt.Print("  (personal)")
		}
		fmt.Println()
	}
	fmt.Printf("  mail account:  %s\n", info.PrimaryMailAccount)
	fmt.Printf("  api:           %s\n", info.APIURL)
	fmt.Printf("  event source:  %s\n", info.EventSourceURL)
}

func printMailboxTree(mailboxes []mail.Mailbox) {
	unread, total := 0, 0
	for _, mb := range mailboxes {
		unread += mb.UnreadEmails
		total += mb.TotalEmails
	}
	fmt.Printf("Mailboxes (%d, %d unread of %d)\n", len(mailboxes), unread, total)

	children := make(map[mail.ID][]mail.Mailbox)
	for _, mb := range mailboxes {
		children[mb.ParentID] = append(children[mb.ParentID], mb)
	}
	for _, kids := range children {
		sort.Slice(kids, func(i, j int) bool {
			if kids[i].SortOrder != kids[j].SortOrder {
				return kids[i].SortOrder < kids[j].SortOrder
			}
			return kids[i].Name < kids[j].Name
		})
	}

	var walk func(parent mail.ID, prefix string)
	walk = func(parent mail.ID, prefix string) {
		kids := children[parent]
		for i, mb := range kids {
			branch, next := "├── ", "│   "
			if i == len(kids)-1 {
				branch, next = "└── ", "    "
			}
			line := prefix + branch + mb.Name
			if mb.Role != "" {
				line += fmt.Sprintf("  [%s]", mb.Role)
			}
			if mb.TotalEmails > 0 {
				line += fmt.Sprintf("  %d/%d", mb.UnreadEmails, mb.TotalEmails)
			}
			fmt.Println(line)
			walk(mb.ID, prefix+next)
		}
	}
	walk("", "")
}

// first returns the first non-empty string.
func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// account accessors tolerate a nil account (flags-only mode).
func accountURL(a *config.Account) string {
	if a == nil {
		return ""
	}
	return a.URL
}

func accountUsername(a *config.Account) string {
	if a == nil {
		return ""
	}
	return a.Username
}

func accountPasswordFile(a *config.Account) string {
	if a == nil {
		return ""
	}
	return a.PasswordFile
}

func accountSessionURL(a *config.Account) string {
	if a == nil {
		return ""
	}
	return a.SessionURL
}
