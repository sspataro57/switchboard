package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sspataro57/switchboard/internal/connector/google"
	"github.com/sspataro57/switchboard/internal/store"
)

// add-app-password onboards one mailbox for the IMAP/SMTP path (criterion 4).
//
// The secret is read from STDIN and nowhere else. argv is world-readable through
// ps and lands in shell history; an env var leaks through /proc/<pid>/environ and
// every child process. stdin also makes the intended k8s one-liner natural:
//
//	kubectl get secret mail-app-passwords -o jsonpath='{.data.sspataro}' \
//	  | base64 -d | google-auth add-app-password sspataro@gmail.com

// maxAppPasswordBytes bounds the read. A Google app password is 16 characters;
// anything approaching this is a redirected file or a paste accident, and
// storing it would produce a login failure nobody could diagnose.
const maxAppPasswordBytes = 512

// readAppPassword consumes the secret from r, stripping the trailing newline.
//
// Every error message here is written to be safe to print: none of them
// interpolate the input, because this text reaches terminals, logs and CI output.
func readAppPassword(r io.Reader) (string, error) {
	br := bufio.NewReader(io.LimitReader(r, maxAppPasswordBytes+1))
	line, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read app password from stdin: %w", err)
	}
	if len(line) > maxAppPasswordBytes {
		return "", fmt.Errorf("app password on stdin exceeds %d bytes; expected a Google app password", maxAppPasswordBytes)
	}
	// Trim only line endings and surrounding blanks. Interior spaces are kept:
	// Gmail displays app passwords in four groups and accepts either spelling,
	// so silently collapsing them would change a credential the user can see.
	secret := strings.Trim(line, "\r\n")
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", errors.New("no app password on stdin (an empty secret must never be stored)")
	}
	return secret, nil
}

func addAppPasswordCmd(args []string) error {
	fs := flag.NewFlagSet("add-app-password", flag.ExitOnError)
	imapHost := fs.String("imap-host", "", "IMAP host (default imap.gmail.com)")
	imapPort := fs.Int("imap-port", 0, "IMAP port (default 993)")
	smtpHost := fs.String("smtp-host", "", "SMTP host (default smtp.gmail.com)")
	smtpPort := fs.Int("smtp-port", 0, "SMTP port (default 587)")
	noAvailability := fs.Bool("no-availability", false, "exclude this account's calendar from availability")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: google-auth add-app-password <email> [--imap-host H] [--imap-port P] [--smtp-host H] [--smtp-port P] [--no-availability]")
	}
	email := fs.Arg(0)

	key := os.Getenv("OPS_TOKEN_KEY")
	if key == "" {
		return errors.New("OPS_TOKEN_KEY is not set")
	}

	password, err := readAppPassword(os.Stdin)
	if err != nil {
		return err
	}

	hosts := google.MailHosts{
		IMAPHost: *imapHost, IMAPPort: *imapPort,
		SMTPHost: *smtpHost, SMTPPort: *smtpPort,
	}.WithDefaults()

	ctx := context.Background()

	// Verify BEFORE storing. A wrong or expired password stored silently turns
	// into every ingest pass failing to authenticate — an error far from its
	// cause. Failing here, while the operator is watching, is the whole point.
	fmt.Fprintf(os.Stderr, "verifying %s against %s ...\n", email, hosts.IMAPAddr())
	src := google.NewIMAPClientSource(hosts, email, password)
	folders, err := src.Folders(ctx)
	_ = src.Close()
	if err != nil {
		return fmt.Errorf("IMAP verification failed for %s: %w", email, err)
	}
	selected := google.SelectFolders(folders, google.FoldersFromEnv())
	if len(selected) == 0 {
		return fmt.Errorf("IMAP login for %s succeeded but no INBOX/Sent folder was found among %d mailboxes", email, len(folders))
	}

	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	id, err := google.UpsertAppPasswordAccount(ctx, pool, email, password, key, hosts, !*noAvailability)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(selected))
	for _, f := range selected {
		names = append(names, f.Name)
	}
	fmt.Printf("stored app-password account %s (id %d); folders in scope: %s\n", email, id, strings.Join(names, ", "))
	fmt.Printf("send_enabled is false — enable it deliberately when you want switchboard to send from this mailbox\n")
	return nil
}
