package main

// add-microsoft onboards one Microsoft mailbox for the IMAP path (SWT-66).
//
// There is no secret to read from stdin, unlike add-app-password: the Azure app
// is a PUBLIC client, and the only credential this command stores is the refresh
// token the device flow returns. MS_OAUTH_CLIENT_ID is the client id and is not
// a secret either.
//
// Identity is verified TWICE before anything is written, because picking the
// wrong account in a browser is the realistic mistake and a mislabelled row
// poisons the own-address set (invariant 5) and every thread key it produces:
//
//  1. the id_token's claim must equal the email argument;
//  2. an IMAP LIST with the fresh access token must find a usable folder set.
//
// Only then is the row stored.

import (
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

// microsoftHosts are Outlook's documented endpoints: IMAP implicit TLS on 993,
// SMTP STARTTLS on 587. They are stored per account, so nothing about the Gmail
// defaults applies to this row.
var microsoftHosts = google.MailHosts{
	IMAPHost: "outlook.office365.com", IMAPPort: 993,
	SMTPHost: "smtp-mail.outlook.com", SMTPPort: 587,
}

// addMicrosoftOpts is the command's seam: the two side effects are injected so
// the flow is testable offline, and so the ORDER of them is assertable.
type addMicrosoftOpts struct {
	Email    string
	TokenKey string
	Out      io.Writer
	// VerifyIMAP opens IMAP with the fresh access token and must refuse when no
	// usable folder is found.
	VerifyIMAP func(ctx context.Context, hosts google.MailHosts, email, accessToken string) error
	// Store is the ONLY write. Called at most once, and only after both identity
	// checks pass.
	Store func(ctx context.Context, email, refreshToken string, scopes []string, hosts google.MailHosts) (int64, error)
}

// runAddMicrosoft is the whole flow minus flag parsing and wiring.
func runAddMicrosoft(ctx context.Context, o addMicrosoftOpts) error {
	if strings.TrimSpace(o.Email) == "" {
		return errors.New("add-microsoft: no email given")
	}
	if o.TokenKey == "" {
		return errors.New("OPS_TOKEN_KEY is not set")
	}
	cfg, err := google.MicrosoftOAuthConfig()
	if err != nil {
		return err
	}

	tok, err := google.DeviceFlow(ctx, cfg, o.Out)
	if err != nil {
		return err
	}
	refresh := tok.RefreshToken
	if refresh == "" {
		return fmt.Errorf("the token response carried no refresh token for %s: without offline_access "+
			"this mailbox would stop authenticating within the hour", o.Email)
	}

	// Check 1, the cheapest and the most common mistake: which account signed in.
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		return fmt.Errorf("the token response carried no id_token for %s, so the account that signed in "+
			"cannot be verified; openid and email are requested precisely for this", o.Email)
	}
	identity, claims, err := google.IDTokenIdentity(rawID)
	if err != nil {
		return fmt.Errorf("verifying which account signed in for %s: %w", o.Email, err)
	}
	if !strings.EqualFold(strings.TrimSpace(identity), strings.TrimSpace(o.Email)) {
		return fmt.Errorf("the browser signed in as %s but %s was requested; nothing was stored "+
			"(claims present: %s)", identity, o.Email, strings.Join(claims, ", "))
	}

	// Check 2: the credential actually opens the mailbox. A token that satisfies
	// the claim check but cannot LIST is a scope or a licensing problem, and
	// finding that out now — while the operator is watching — is the whole point.
	if o.VerifyIMAP != nil {
		fmt.Fprintf(o.Out, "verifying %s against %s:%d ...\n", o.Email, microsoftHosts.IMAPHost, microsoftHosts.IMAPPort)
		if err := o.VerifyIMAP(ctx, microsoftHosts, o.Email, tok.AccessToken); err != nil {
			return fmt.Errorf("IMAP verification failed for %s: %w", o.Email, err)
		}
	}

	id, err := o.Store(ctx, o.Email, refresh, google.MicrosoftScopes, microsoftHosts)
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "stored xoauth2 account %s (id %d)\n", o.Email, id)
	fmt.Fprintf(o.Out, "send_enabled is false and no SMTP scope was requested: this mailbox is read-only\n")
	return nil
}

// verifyIMAPWithToken is the production VerifyIMAP: open IMAP with the bearer
// token and require a non-empty folder selection, the same check
// add-app-password makes with a password.
func verifyIMAPWithToken(ctx context.Context, hosts google.MailHosts, email, accessToken string) error {
	src := google.NewIMAPClientSource(hosts, email, "")
	src.AccessToken = func(context.Context) (string, error) { return accessToken, nil }
	defer func() { _ = src.Close() }()

	folders, err := src.Folders(ctx)
	if err != nil {
		return err
	}
	selected := google.SelectFolders(folders, google.FoldersFromEnv())
	if len(selected) == 0 {
		return fmt.Errorf("login succeeded but no INBOX/Sent folder was found among %d mailboxes", len(folders))
	}
	names := make([]string, 0, len(selected))
	for _, f := range selected {
		names = append(names, f.Name)
	}
	fmt.Printf("folders in scope: %s\n", strings.Join(names, ", "))
	return nil
}

func addMicrosoftCmd(args []string) error {
	fs := flag.NewFlagSet("add-microsoft", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: google-auth add-microsoft <email>")
	}
	email := fs.Arg(0)

	ctx := context.Background()
	pool, err := store.NewPool(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	key := os.Getenv("OPS_TOKEN_KEY")
	return runAddMicrosoft(ctx, addMicrosoftOpts{
		Email:      email,
		TokenKey:   key,
		Out:        os.Stdout,
		VerifyIMAP: verifyIMAPWithToken,
		Store: func(ctx context.Context, email, refreshToken string, scopes []string, hosts google.MailHosts) (int64, error) {
			return google.UpsertXOAuth2Account(ctx, pool, email, refreshToken, key, scopes, hosts)
		},
	})
}
